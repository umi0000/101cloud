package instances

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/catalog"
	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Recovery reads the original journal, but records all new work under the live
// reconciliation lease. An operation whose submission is ambiguous is never replayed.
type workerRecoveryKey struct{}

func registerWorkers(a *platform.App) {
	a.RegisterJob("instance.create", runCreate)
	a.RegisterJob("instance.start", runLifecycle)
	a.RegisterJob("instance.stop", runLifecycle)
	a.RegisterJob("instance.restart", runLifecycle)
	a.RegisterJob("instance.resize", runResize)
	a.RegisterJob("instance.delete", runDelete)
	a.RegisterJob("instance.import", runImport)
	a.RegisterJob("instance.reconcile", runReconcile)
	a.RegisterJob("instance.gpu", runGPU)
	a.RegisterJob("instance.creation_cleanup", runCreationCleanup)
	a.AddPoller("failed-creation-cleanup", 15*time.Second, pollFailedCreations)
	for _, kind := range []string{"instance.create", "instance.start", "instance.stop", "instance.restart", "instance.resize", "instance.delete", "instance.import", "instance.gpu"} {
		a.RegisterReconciler(kind, runReconcile)
	}
}

func workerParams(j platform.Job) (platform.Row, error) {
	var p platform.Row
	if err := json.Unmarshal(j.Parameters, &p); err != nil {
		return nil, err
	}
	return p, nil
}
func workerPath(r platform.Row, suffix string) string {
	return "/1.0/instances/" + url.PathEscape(platform.Text(r["incus_name"])) + suffix + "?project=" + url.QueryEscape(platform.Text(r["project_name"]))
}
func workerMetadata(v map[string]any) (platform.Row, error) {
	if platform.Text(v["type"]) == "error" || platform.Uint(v["error_code"]) >= 400 {
		return nil, fmt.Errorf("native operation rejected (%s)", platform.Text(v["error_code"]))
	}
	m, ok := v["metadata"].(map[string]any)
	if !ok {
		return nil, errors.New("native response omitted metadata")
	}
	return m, nil
}

// Native config may contain cloud-init password hashes. Keep it transient: never
// persist this result in incus_config_snapshot, task results, audit or logs.
func workerObserve(ctx context.Context, a *platform.App, r platform.Row) (platform.Row, string, error) {
	v, etag, err := a.AgentConditional(ctx, platform.Uint(r["node_id"]), http.MethodGet, workerPath(r, ""), nil, "")
	if err != nil {
		return nil, "", err
	}
	m, err := workerMetadata(v)
	if err != nil {
		return nil, "", err
	}
	cfg := object(m["expanded_config"])
	if len(cfg) == 0 {
		cfg = object(m["config"])
	}
	uuid := platform.Text(cfg["volatile.uuid"])
	expected := platform.Text(r["incus_uuid"])
	if uuid == "" || (expected != "" && uuid != expected) {
		return nil, "", errors.New("instance identity could not be verified")
	}
	if platform.Text(m["name"]) != platform.Text(r["incus_name"]) {
		return nil, "", errors.New("instance name changed")
	}
	if platform.Bool(r["stuck_cleanup"]) {
		if err := stuckDeleteIdentity(r, m); err != nil {
			return nil, "", err
		}
	}
	if r["creation_failed_at"] != nil && platform.Text(cfg["user.101cloud.instance_id"]) != platform.Text(r["id"]) {
		return nil, "", errors.New("creation cleanup ownership marker mismatch")
	}
	return m, etag, nil
}
func workerState(ctx context.Context, a *platform.App, r platform.Row) (string, error) {
	v, err := a.Agent(ctx, platform.Uint(r["node_id"]), http.MethodGet, workerPath(r, "/state"), nil)
	if err != nil {
		return "", err
	}
	m, err := workerMetadata(v)
	if err != nil {
		return "", err
	}
	switch platform.Text(m["status"]) {
	case "Running":
		return "running", nil
	case "Stopped":
		return "stopped", nil
	case "Frozen":
		return "frozen", nil
	}
	return "unknown", nil
}
func workerStep(ctx context.Context, a *platform.App, j platform.Job, key string) (platform.Row, error) {
	var steps []platform.Row
	ids := []uint64{j.ID}
	switch source := ctx.Value(workerRecoveryKey{}).(type) {
	case uint64:
		if source != 0 {
			ids = append(ids, source)
		}
	case []uint64:
		ids = append(ids, source...)
	}
	err := a.DB.WithContext(ctx).Table("task_steps").Where("task_id IN ? AND step_key = ?", ids, key).Order("task_id = " + strconv.FormatUint(j.ID, 10) + " DESC, task_id DESC, attempt DESC").Limit(1).Find(&steps).Error
	if err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return nil, nil
	}
	return steps[0], nil
}

// workerOperation commits intent before dispatch, then commits the operation ID
// before waiting. A missing ID after dispatch can only be resolved by observation.
func workerOperation(ctx context.Context, a *platform.App, j platform.Job, key string, send func() (map[string]any, error), confirmed func() (bool, error)) (out platform.Row, operationErr error) {
	if initializationStage(key) {
		defer func() { operationErr = initializationOperationError(ctx, key, operationErr) }()
	}
	old, err := workerStep(ctx, a, j, key)
	if err != nil {
		return nil, err
	}
	if old != nil {
		if platform.Text(old["status"]) == "cancelled" {
			return nil, workerSkipped
		}
		op := platform.Text(old["operation_id"])
		if op != "" {
			v, e := a.WaitOperation(ctx, j.NodeID, op)
			if e != nil {
				if confirmed != nil {
					ok, observeErr := confirmed()
					if observeErr == nil && ok {
						if stepErr := a.Step(ctx, j, key, op, "succeeded"); stepErr != nil {
							return nil, platform.Unknown(stepErr)
						}
						return platform.Row{}, nil
					}
				}
				return nil, platform.Unknown(e)
			}
			m, e := workerMetadata(v)
			if e != nil {
				return nil, platform.Unknown(e)
			}
			if e = workerValidateOperation(ctx, a, j, key, op, m); e != nil {
				return nil, e
			}
			if e = a.Step(ctx, j, key, op, "succeeded"); e != nil {
				return nil, platform.Unknown(e)
			}
			return m, nil
		}
		if platform.Text(old["status"]) == "succeeded" {
			return platform.Row{}, nil
		}
		if confirmed != nil {
			ok, e := confirmed()
			if e != nil {
				return nil, platform.Unknown(e)
			}
			if ok {
				if e = a.Step(ctx, j, key, "", "succeeded"); e != nil {
					return nil, platform.Unknown(e)
				}
				return platform.Row{}, nil
			}
		}
		return nil, platform.Unknown(errors.New("operation submission is ambiguous; observation cannot authorize replay"))
	}
	if err = a.Step(ctx, j, key, "", "running"); err != nil {
		return nil, err
	}
	v, err := send()
	if err != nil {
		if nativeMissing(err) && confirmed != nil {
			if absent, observeErr := confirmed(); observeErr == nil && absent {
				if stepErr := a.Step(ctx, j, key, "", "succeeded"); stepErr != nil {
					return nil, platform.Unknown(stepErr)
				}
				return platform.Row{}, nil
			}
		}
		if errors.Is(err, workerSkipped) {
			if e := a.Step(ctx, j, key, "", "cancelled"); e != nil {
				return nil, e
			}
			return nil, workerSkipped
		}
		return nil, platform.Unknown(err)
	}
	if platform.Text(v["type"]) == "error" {
		return nil, platform.Unknown(errors.New("native operation returned an error after dispatch"))
	}
	op := platform.Text(v["operation"])
	if op == "" {
		if confirmed == nil {
			return nil, platform.Unknown(errors.New("native mutation omitted operation identity"))
		}
		ok, e := confirmed()
		if e != nil {
			return nil, platform.Unknown(e)
		}
		if !ok {
			return nil, platform.Unknown(errors.New("native mutation not confirmed"))
		}
		if e = a.Step(ctx, j, key, "", "succeeded"); e != nil {
			return nil, platform.Unknown(e)
		}
		return platform.Row{}, nil
	}
	if ref, e := url.Parse(op); e == nil {
		q := ref.Query()
		if q.Get("project") == "" && j.InstanceID != 0 {
			r, e := workerInstance(ctx, a, j)
			if e != nil {
				return nil, platform.Unknown(e)
			}
			q.Set("project", platform.Text(r["project_name"]))
			ref.RawQuery = q.Encode()
			op = ref.String()
		}
	}
	if err = a.Step(ctx, j, key, op, "waiting"); err != nil {
		return nil, platform.Unknown(err)
	}
	v, err = a.WaitOperation(ctx, j.NodeID, op)
	if err != nil {
		if confirmed != nil {
			ok, observeErr := confirmed()
			if observeErr == nil && ok {
				if stepErr := a.Step(ctx, j, key, op, "succeeded"); stepErr != nil {
					return nil, platform.Unknown(stepErr)
				}
				return platform.Row{}, nil
			}
		}
		return nil, platform.Unknown(err)
	}
	m, err := workerMetadata(v)
	if err != nil {
		return nil, platform.Unknown(err)
	}
	if err = workerValidateOperation(ctx, a, j, key, op, m); err != nil {
		return nil, err
	}
	if err = a.Step(ctx, j, key, op, "succeeded"); err != nil {
		return nil, platform.Unknown(err)
	}
	return m, nil
}
func workerInstance(ctx context.Context, a *platform.App, j platform.Job) (platform.Row, error) {
	r, err := platform.Get(a.DB.WithContext(ctx), "instances", j.InstanceID, false)
	if err != nil {
		return nil, err
	}
	cleanup := platform.Bool(object(string(j.Parameters))["cleanup_stuck"])
	if cleanup {
		r["stuck_cleanup"] = true
	}
	if platform.Uint(r["node_id"]) != j.NodeID || platform.Uint(r["user_id"]) != j.UserID {
		return nil, errors.New("task instance ownership or placement changed")
	}
	readOnlyStoppedRecovery := j.Type == "instance.reconcile" && ctx.Value(forceStopObservationKey{}) != nil
	if r["creation_failed_at"] != nil && !cleanup && !readOnlyStoppedRecovery && j.Type != "instance.creation_cleanup" && ctx.Value(creationCleanupAuthorityKey{}) != j.ID {
		return nil, platform.E(404, "not_found", "Instance not found")
	}
	return r, nil
}
func workerUnknown(ctx context.Context, a *platform.App, j platform.Job, r platform.Row, err error) error {
	// Never release IP, disk, quota or GPU reservations on an uncertain outcome.
	_ = a.DB.WithContext(context.WithoutCancel(ctx)).Transaction(func(db *gorm.DB) error {
		if e := lockWorkerAccount(db, a, j, platform.Uint(r["user_id"])); e != nil {
			return e
		}
		return db.Table("instances").Where("id=? AND deleted_at IS NULL", r["id"]).Updates(platform.Row{"provisioning_state": "unknown", "revision": gorm.Expr("revision+1")}).Error
	})
	return platform.Unknown(err)
}

func workerCreatePreflightFailed(ctx context.Context, a *platform.App, j platform.Job, r platform.Row, cause error) error {
	// Recovered tasks never acquire new release authority from missing journals.
	if ctx.Value(workerRecoveryKey{}) != nil {
		return workerUnknown(ctx, a, j, r, workerRetainedCreationError(cause))
	}
	err := a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if e := lockWorkerAccount(db, a, j, j.UserID); e != nil {
			return e
		}
		current, e := platform.Get(db, "instances", j.InstanceID, true)
		if e != nil {
			return e
		}
		delivered, e := creationDelivered(db, current)
		if e != nil {
			return e
		}
		if delivered {
			return errors.New("delivered creation cannot release preflight reservations")
		}
		if !workerPristineCreation(current, j) {
			return errors.New("creation state changed before reservation cleanup")
		}
		// All other task history is conservatively disqualifying, including unknown
		// reconciliation descendants. Missing journals alone never authorize release.
		var other, mutations int64
		if e = db.Table("tasks").Where("instance_id=? AND id<>?", j.InstanceID, j.ID).Count(&other).Error; e != nil {
			return e
		}
		if e = db.Table("task_steps").Where("task_id=? AND (step_key<>? OR (operation_id IS NOT NULL AND operation_id<>''))", j.ID, "create.preflight").Count(&mutations).Error; e != nil {
			return e
		}
		if other != 0 || mutations != 0 {
			return errors.New("creation has prior task or mutation intent")
		}
		var ips, allocations []platform.Row
		if e = locked(db).Table("ip_addresses").Where("instance_id=?", j.InstanceID).Find(&ips).Error; e != nil {
			return e
		}
		for _, ip := range ips {
			if platform.Text(ip["status"]) != "reserved" || platform.Uint(ip["reservation_task_id"]) != j.ID || platform.Uint(ip["node_id"]) != j.NodeID || ip["bound_at"] != nil {
				return errors.New("IP reservation ownership changed")
			}
		}
		if e = locked(db).Table("gpu_allocations").Where("instance_id=?", j.InstanceID).Find(&allocations).Error; e != nil {
			return e
		}
		for _, g := range allocations {
			if platform.Text(g["status"]) != "reserved" || platform.Uint(g["user_id"]) != j.UserID || platform.Uint(g["node_id"]) != j.NodeID || g["configured"] == nil || g["observed"] == nil || platform.Bool(g["configured"]) || platform.Bool(g["observed"]) || g["attached_at"] != nil {
				return errors.New("GPU reservation is not pristine")
			}
		}
		now := a.Time()
		current["creation_failed_at"] = now
		if e = db.Table("instances").Where("id=?", j.InstanceID).Updates(platform.Row{"creation_failed_at": now, "creation_failure_task_id": j.ID, "creation_cleanup_state": "completed", "desired_state": "deleted", "deleted_at": now}).Error; e != nil {
			return e
		}
		if e = billing.CancelFailedCreation(ctx, db, current, now); e != nil {
			return e
		}
		if e = db.Table("gpu_allocations").Where("instance_id=? AND status='reserved'", j.InstanceID).Updates(platform.Row{"status": "cancelled", "released_at": now, "release_reason": "creation failed before dispatch", "release_verified": true, "last_verified_at": now, "revision": gorm.Expr("revision+1")}).Error; e != nil {
			return e
		}
		if e = db.Table("ip_addresses").Where("instance_id=? AND reservation_task_id=? AND status='reserved'", j.InstanceID, j.ID).Updates(platform.Row{"status": "available", "node_id": nil, "node_network_binding_id": nil, "instance_id": nil, "reservation_task_id": nil, "reserved_at": nil, "reservation_expires_at": nil, "revision": gorm.Expr("revision+1")}).Error; e != nil {
			return e
		}
		if e = db.Table("instances").Where("id=?", j.InstanceID).Updates(platform.Row{"provisioning_state": "failed", "observed_state": "error", "observed_at": now, "incus_config_snapshot": gorm.Expr("JSON_SET(incus_config_snapshot, '$.creation_reservations_released', true)"), "revision": gorm.Expr("revision+1")}).Error; e != nil {
			return e
		}
		return db.Table("tasks").Where("id=? AND lease_generation=?", j.ID, j.LeaseGeneration).Update("result_redacted", jsonText(platform.Row{"creation_reservations_released": true})).Error
	})
	if err != nil {
		platform.LogError("retain failed creation reservations", err)
		return workerUnknown(ctx, a, j, r, workerRetainedCreationError(confirmedCreationFailure(cause)))
	}
	return cause
}

func workerPristineCreation(r platform.Row, j platform.Job) bool {
	state := platform.Text(r["provisioning_state"])
	return j.Type == "instance.create" && platform.Uint(r["id"]) == j.InstanceID && platform.Uint(r["user_id"]) == j.UserID && platform.Uint(r["node_id"]) == j.NodeID && platform.Text(r["incus_uuid"]) == "" && r["deleted_at"] == nil && platform.Text(r["observed_state"]) == "pending" && (state == "pending" || state == "creating")
}

func workerRetainedCreationError(cause error) error {
	public := platform.PublicError(cause, true)
	details, _ := public["details"].(map[string]any)
	if details == nil {
		details = map[string]any{}
	}
	details["resource_reservations_released"] = false
	details["action"] = "物理资源预留仍保留；确定未交付失败将由后台安全回收，派发有歧义时仅核对原操作，不会重放创建或初始化。"
	return platform.EDetails(409, platform.Text(public["code"]), platform.Text(public["message"]), platform.Row(details))
}

// Recovery wrappers retain the original operation identity, including when a
// CREATE invokes START internally. A repeated observation is not a new event.
func workerBillingIdentity(ctx context.Context, db *gorm.DB, j platform.Job) (uint64, string, error) {
	id := j.ID
	switch source := ctx.Value(workerRecoveryKey{}).(type) {
	case uint64:
		if source != 0 && source < id {
			id = source
		}
	case []uint64:
		for _, candidate := range source {
			if candidate != 0 && candidate < id {
				id = candidate
			}
		}
	}
	task, err := platform.Get(db, "tasks", id, false)
	if err != nil {
		return 0, "", err
	}
	return id, platform.Text(task["task_type"]), nil
}

func workerConfirm(ctx context.Context, a *platform.App, j platform.Job, r platform.Row, state, event string, extra platform.Row) error {
	return workerConfirmStep(ctx, a, j, r, state, event, extra, "")
}

// The resize checkpoint and its accounting cutover commit together. Recovery
// must not end/reopen the new held sessions after START has been submitted.
func workerConfirmStep(ctx context.Context, a *platform.App, j platform.Job, r platform.Row, state, event string, extra platform.Row, stepKey string) error {
	return a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if err := lockWorkerAccount(db, a, j, platform.Uint(r["user_id"])); err != nil {
			return err
		}
		locked, err := platform.Get(db, "instances", platform.Uint(r["id"]), true)
		if err != nil {
			return err
		}
		if locked["creation_failed_at"] != nil {
			if state == "running" || (extra != nil && platform.Text(extra["desired_state"]) == "running") {
				return errors.New("failed creation cannot resume")
			}
			if extra != nil && platform.Text(extra["provisioning_state"]) == "ready" {
				extra["provisioning_state"] = "failed"
			}
		}
		at := a.Time()
		root, kind, err := workerBillingIdentity(ctx, db, j)
		if err != nil {
			return err
		}
		key := "task:" + strconv.FormatUint(root, 10) + ":"
		var finish func() error
		if locked["creation_failed_at"] == nil {
			eventType, published := "", true
			if kind == "instance.create" && event == "running" {
				eventType, published = "instance.create", false
			} else if kind == "instance.create" && event == "stopped" {
				var draft struct {
					ID        *uint64
					Published bool
				}
				// Current read without locking an absent event key/gap: only our
				// already-owned instance is locked, even for wallet-less users.
				if err = db.Table("instances i").Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "i"}}).Select("e.id,e.published").Joins("LEFT JOIN billing_events e ON e.user_id=i.user_id AND e.business_key=?", key+"instance.create").Where("i.id=?", locked["id"]).Take(&draft).Error; err != nil {
					return err
				}
				if draft.ID == nil {
					eventType = "instance.create"
				} else if !draft.Published {
					eventType = "instance.stop"
				}
			} else if kind == "instance.create" && stepKey == "create.verify" && state == "stopped" {
				eventType = "instance.create"
			} else if kind == "instance.start" && event == "running" {
				eventType = "instance.start"
			} else if (event == "stopped" || event == "resize_stopped") && (kind == "instance.stop" || kind == "instance.start" || kind == "instance.resize" || kind == "instance.delete") {
				eventType = "instance.stop"
			}
			if eventType != "" {
				finish, err = billing.PrepareInstanceEvent(db, locked, eventType, key+eventType, at, published)
				if err != nil {
					return err
				}
			}
		}
		changes := platform.Row{"observed_state": state, "observed_at": a.Time(), "revision": gorm.Expr("revision + 1")}
		for k, v := range extra {
			changes[k] = v
			locked[k] = v
		}
		locked["observed_state"] = state
		locked["observed_at"] = a.Time()
		if err = db.Table("instances").Where("id = ?", r["id"]).Updates(changes).Error; err != nil {
			return err
		}
		if event != "" {
			if err = billing.InstanceUsageAt(ctx, db, locked, event, nil, at); err != nil {
				return err
			}
		}
		if finish != nil {
			if err = finish(); err != nil {
				return err
			}
		}
		deliveredStopped := j.Type == "instance.create" && event == "stopped" && platform.Text(locked["provisioning_state"]) == "ready"
		if kind == "instance.create" && (stepKey == "create.verify" || deliveredStopped) && locked["creation_failed_at"] == nil {
			if err = billing.PublishInstanceEvent(db, platform.Uint(locked["user_id"]), key+"instance.create"); err != nil {
				return err
			}
		}
		if stepKey != "" {
			now := a.Time()
			step := platform.Row{"task_id": j.ID, "step_key": stepKey, "attempt": j.LeaseGeneration, "status": "succeeded", "parameters_redacted": "{}", "started_at": now, "finished_at": now}
			if err = db.Table("task_steps").Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "task_id"}, {Name: "step_key"}, {Name: "attempt"}}, DoUpdates: clause.AssignmentColumns([]string{"status", "finished_at"})}).Create(&step).Error; err != nil {
				return err
			}
			return db.Table("tasks").Where("id=? AND lease_generation=?", j.ID, j.LeaseGeneration).Update("phase", stepKey).Error
		}
		return nil
	})
}
func workerFingerprint(v any) string {
	b := []byte(platform.Text(v))
	if len(b) == 32 {
		return hex.EncodeToString(b)
	}
	if len(b) == 64 {
		if _, err := hex.DecodeString(string(b)); err == nil {
			return strings.ToLower(string(b))
		}
	}
	return ""
}
func workerNetwork(ctx context.Context, a *platform.App, r platform.Row) (platform.Row, platform.Row, platform.Row, error) {
	var ips []platform.Row
	err := a.DB.WithContext(ctx).Table("ip_addresses").Where("instance_id = ? AND status IN ('reserved','bound')", r["id"]).Find(&ips).Error
	if err != nil {
		return nil, nil, nil, err
	}
	if len(ips) != 1 {
		return nil, nil, nil, platform.E(409, "invalid_state", "实例未持有唯一的已批准预留地址，请管理员检查 IP 预留与网络绑定。")
	}
	binding, err := platform.Get(a.DB.WithContext(ctx), "node_network_bindings", platform.Uint(ips[0]["node_network_binding_id"]), false)
	if err != nil {
		return nil, nil, nil, err
	}
	pool, err := platform.Get(a.DB.WithContext(ctx), "network_pools", platform.Uint(ips[0]["network_pool_id"]), false)
	if err != nil {
		return nil, nil, nil, err
	}
	if platform.Uint(binding["node_id"]) != platform.Uint(r["node_id"]) || platform.Text(binding["project_name"]) != platform.Text(r["project_name"]) || platform.Text(binding["status"]) != "active" || platform.Text(binding["validation_status"]) != "verified" {
		return nil, nil, nil, platform.E(409, "configuration_conflict", "已批准的网络绑定已变更或未验证，请管理员重新核对节点网络配置。")
	}
	return ips[0], binding, pool, nil
}
func workerIP(v any) (string, error) {
	b := []byte(platform.Text(v))
	ip, ok := netip.AddrFromSlice(b)
	if !ok {
		return "", errors.New("invalid packed approved IP")
	}
	return ip.String(), nil
}
func workerNoNAT(ctx context.Context, a *platform.App, node uint64, project string, binding platform.Row) error {
	if platform.Text(binding["mode"]) != "managed_bridge" {
		return nil
	}
	path := "/1.0/networks/" + url.PathEscape(platform.Text(binding["incus_network_name"])) + "?project=" + url.QueryEscape(project)
	v, err := a.Agent(ctx, node, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	m, err := workerMetadata(v)
	if err != nil {
		return err
	}
	cfg := object(m["config"])
	for _, key := range []string{"ipv4.nat", "ipv6.nat"} {
		if platform.Text(cfg[key]) != "false" {
			return platform.E(409, "configuration_conflict", "受管网桥必须明确关闭 IPv4 和 IPv6 NAT，请管理员修正网络配置。")
		}
	}
	return nil
}
func workerCreateBody(ctx context.Context, a *platform.App, r platform.Row) (platform.Row, error) {
	ip, binding, pool, err := workerNetwork(ctx, a, r)
	if err != nil {
		return nil, err
	}
	if err = workerNoNAT(ctx, a, platform.Uint(r["node_id"]), platform.Text(r["project_name"]), binding); err != nil {
		return nil, err
	}
	address, err := workerIP(ip["address"])
	if err != nil {
		return nil, err
	}
	if platform.Text(r["image_type"]) == "virtual-machine" && platform.Text(binding["mode"]) == "macvlan" {
		return nil, platform.E(409, "configuration_conflict", "当前 macvlan 网络不支持虚拟机，请选择已批准的桥接网络。")
	}
	nic := platform.Row{"type": "nic", "name": "eth0"}
	switch platform.Text(binding["mode"]) {
	case "bridge", "macvlan":
		nic["nictype"] = "macvlan"
		if platform.Text(binding["mode"]) == "bridge" {
			nic["nictype"] = "bridged"
		}
		nic["parent"] = platform.Text(binding["parent_interface"])
	case "managed_bridge":
		nic["network"] = platform.Text(binding["incus_network_name"])
		if platform.Uint(ip["address_family"]) == 4 {
			nic["ipv4.address"] = address
		} else {
			nic["ipv6.address"] = address
		}
	default:
		return nil, platform.E(409, "configuration_conflict", "当前批准的网络模式不受支持，请管理员检查网络绑定。")
	}
	if mtu := platform.Uint(binding["mtu"]); mtu != 0 {
		nic["mtu"] = strconv.FormatUint(mtu, 10)
	}
	eth := platform.Row{"dhcp4": false, "dhcp6": false, "addresses": []string{address + "/" + platform.Text(pool["prefix_length"])}}
	if platform.Text(pool["gateway"]) != "" {
		gateway, e := workerIP(pool["gateway"])
		if e != nil {
			return nil, e
		}
		dest := "0.0.0.0/0"
		if platform.Uint(ip["address_family"]) == 6 {
			dest = "::/0"
		}
		eth["routes"] = []platform.Row{{"to": dest, "via": gateway}}
	}
	var dns []string
	if err = json.Unmarshal([]byte(platform.Text(pool["dns_servers"])), &dns); err != nil {
		return nil, err
	}
	eth["nameservers"] = platform.Row{"addresses": dns}
	network := jsonText(platform.Row{"version": 2, "ethernets": platform.Row{"eth0": eth}})
	cfg := platform.Row{"limits.cpu": platform.Text(r["vcpus"]), "limits.memory": platform.Text(r["memory_bytes"]), "user.101cloud.instance_id": platform.Text(r["id"]), "cloud-init.network-config": network}
	if platform.Text(r["login_config_ciphertext"]) != "" || platform.Bool(object(object(r["incus_config_snapshot"])["creation"])["cloud_init_capable"]) {
		data, e := loginUserData(a, r)
		if e != nil {
			return nil, e
		}
		cfg["cloud-init.user-data"] = data
	}
	if platform.Text(r["image_type"]) == "container" {
		cfg["security.privileged"] = "false"
	}
	var allocations int64
	if err = a.DB.WithContext(ctx).Table("gpu_allocations").Where("instance_id = ? AND active_slot = 1", r["id"]).Count(&allocations).Error; err != nil {
		return nil, err
	}
	if allocations > 0 && platform.Text(r["image_type"]) != "container" {
		return nil, platform.E(409, "configuration_conflict", "GPU 仅支持容器实例，请管理员核对实例类型与 GPU 申请。")
	}
	// Only admission's frozen creation capability may prepare a zero-GPU
	// container; recovery must not consult mutable image catalog metadata.
	nvidiaRuntime := platform.Text(r["image_type"]) == "container" && platform.Bool(object(object(r["incus_config_snapshot"])["creation"])["nvidia_runtime"])
	if allocations > 0 || nvidiaRuntime {
		cfg["nvidia.runtime"] = "true"
		cfg["nvidia.driver.capabilities"] = "compute,utility"
	}
	fingerprint := workerFingerprint(r["image_fingerprint"])
	if fingerprint == "" {
		return nil, platform.E(409, "configuration_conflict", "实例缺少已固定的镜像指纹，请管理员重新核对镜像配置。")
	}
	source := platform.Row{"type": "image", "fingerprint": fingerprint}
	return platform.Row{"name": platform.Text(r["incus_name"]), "type": platform.Text(r["image_type"]), "architecture": platform.Text(r["architecture"]), "profiles": []string{}, "config": cfg, "devices": platform.Row{"root": platform.Row{"type": "disk", "path": "/", "pool": platform.Text(r["storage_pool"]), "size": platform.Text(r["disk_bytes"])}, "eth0": nic}, "source": source}, nil
}
func runCreateWork(ctx context.Context, a *platform.App, j platform.Job) error {
	r, err := workerInstance(ctx, a, j)
	if err != nil {
		return err
	}
	if r["creation_failed_at"] != nil || r["deleted_at"] != nil {
		return platform.E(409, "creation_retired", "失败创建已退役，不可重新派发。")
	}
	prior, err := workerStep(ctx, a, j, "create.native")
	if err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	// A recovered task without a mutation journal is not authorization to create.
	// Reconciliation may enrich its diagnosis, but never replay a historical create.
	if prior == nil {
		if ctx.Value(workerRecoveryKey{}) == nil {
			if err = a.Step(ctx, j, "create.preflight", "", "running"); err != nil {
				return err
			}
		}
	}
	body, err := workerCreateBody(ctx, a, r)
	if err != nil {
		if prior == nil {
			return workerCreatePreflightFailed(ctx, a, j, r, err)
		}
		return workerUnknown(ctx, a, j, r, err)
	}
	confirm := func() (bool, error) {
		m, _, e := workerObserve(ctx, a, r)
		if e != nil {
			return false, e
		}
		cfg := object(m["expanded_config"])
		if platform.Text(cfg["user.101cloud.instance_id"]) != platform.Text(r["id"]) {
			return false, errors.New("creation ownership marker mismatch")
		}
		return workerFingerprint(cfg["volatile.base_image"]) == workerFingerprint(r["image_fingerprint"]), nil
	}
	if prior == nil {
		if err = workerDiskCapacity(ctx, a, r, platform.Uint(r["disk_bytes"])); err != nil {
			return workerCreatePreflightFailed(ctx, a, j, r, err)
		}
		if ctx.Value(workerRecoveryKey{}) != nil {
			return workerUnknown(ctx, a, j, r, platform.E(409, "creation_dispatch_unconfirmed", "历史创建任务没有可确认的派发记录，未自动重新创建；请管理员核对节点与资源预留后处理。"))
		}
		if err = a.Step(ctx, j, "create.preflight", "", "succeeded"); err != nil {
			return workerCreatePreflightFailed(ctx, a, j, r, err)
		}
	}
	_, err = workerOperation(ctx, a, j, "create.native", func() (map[string]any, error) {
		return a.Agent(platform.WithTaskDispatch(ctx, j, "create.native"), j.NodeID, http.MethodPost, "/1.0/instances?project="+url.QueryEscape(platform.Text(r["project_name"])), body)
	}, confirm)
	if err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	m, _, err := workerObserve(ctx, a, r)
	if err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	cfg := object(m["expanded_config"])
	if len(cfg) == 0 {
		cfg = object(m["config"])
	}
	if platform.Text(cfg["user.101cloud.instance_id"]) != platform.Text(r["id"]) || workerFingerprint(cfg["volatile.base_image"]) != workerFingerprint(r["image_fingerprint"]) {
		return workerUnknown(ctx, a, j, r, confirmedCreationFailure(platform.EDetails(409, "creation_configuration_invalid", "创建结果的归属或镜像不符合批准配置，实例未交付，将安全回收。", platform.Row{"reason": "identity_or_image_mismatch"})))
	}
	securityValid := (platform.Text(r["image_type"]) == "container" && platform.Text(cfg["security.privileged"]) == "false") || (platform.Text(r["image_type"]) == "virtual-machine" && cfg["security.privileged"] == nil)
	if !workerResourcesMatch(m, platform.Uint(r["vcpus"]), platform.Text(r["memory_bytes"]), platform.Text(r["disk_bytes"])) || !securityValid || len(rows(m["profiles"])) != 0 {
		return workerUnknown(ctx, a, j, r, confirmedCreationFailure(platform.EDetails(409, "creation_configuration_invalid", "创建结果的资源或安全配置不符合批准配置，实例未交付，将安全回收。", platform.Row{"reason": "resources_or_security_mismatch"})))
	}
	if err = workerCreatedDevices(m, body); err != nil {
		return workerUnknown(ctx, a, j, r, confirmedCreationFailure(platform.EDetails(409, "creation_configuration_invalid", "创建结果的设备不符合批准配置，实例未交付，将安全回收。", platform.Row{"reason": "device_configuration_mismatch"})))
	}
	r["incus_uuid"] = cfg["volatile.uuid"]
	if err = a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if e := lockWorkerAccount(db, a, j, j.UserID); e != nil {
			return e
		}
		if e := db.Table("instances").Where("id = ?", r["id"]).Updates(platform.Row{"incus_uuid": r["incus_uuid"], "provisioning_state": "initializing", "revision": gorm.Expr("revision + 1")}).Error; e != nil {
			return e
		}
		if e := db.Table("ip_addresses").Where("instance_id = ? AND status = 'reserved'", r["id"]).Updates(platform.Row{"status": "bound", "bound_at": a.Time(), "reservation_expires_at": nil, "revision": gorm.Expr("revision + 1")}).Error; e != nil {
			return e
		}
		return billing.InstanceUsage(ctx, db, r, "disk_reserved", nil)
	}); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	var allocations []platform.Row
	if err = a.DB.WithContext(ctx).Table("gpu_allocations").Where("instance_id = ? AND active_slot = 1", r["id"]).Find(&allocations).Error; err != nil {
		return err
	}
	if len(allocations) > 0 {
		ids := make([]uint64, 0, len(allocations))
		for _, g := range allocations {
			ids = append(ids, platform.Uint(g["id"]))
		}
		gpu := j
		gpu.Parameters = json.RawMessage(jsonText(platform.Row{"action": "attach", "target_ids": ids, "allow_live": false}))
		if err = runGPU(ctx, a, gpu); err != nil {
			return workerUnknown(ctx, a, j, r, err)
		}
	}
	current, err := workerInstance(ctx, a, j)
	if err != nil {
		return err
	}
	if platform.Text(current["desired_state"]) != "running" {
		state, e := workerState(ctx, a, r)
		if e != nil {
			return workerUnknown(ctx, a, j, r, e)
		}
		if state != "stopped" {
			return workerUnknown(ctx, a, j, r, workerLateStop(ctx, a, j, r))
		}
		if e = workerConfirm(ctx, a, j, r, "stopped", "stopped", platform.Row{"provisioning_state": "ready"}); e != nil {
			return workerUnknown(ctx, a, j, r, e)
		}
		return a.TaskResult(ctx, j, platform.Row{"instance_id": strconv.FormatUint(j.InstanceID, 10), "observed_state": "stopped", "initialization": "pending"})
	}
	if err = workerPrepareCloudInit(ctx, a, j, current); err != nil {
		if errors.Is(err, workerSkipped) {
			return a.TaskResult(ctx, j, platform.Row{"instance_id": strconv.FormatUint(j.InstanceID, 10), "skipped": true, "reason": "superseded or no longer authorized"})
		}
		return workerUnknown(ctx, a, j, r, err)
	}
	start := j
	start.Type = "instance.start"
	start.Parameters = json.RawMessage(`{"force":false}`)
	if err = runLifecycle(ctx, a, start); err != nil {
		return err
	}
	finalState, err := workerState(ctx, a, r)
	if err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	if finalState != "running" && finalState != "stopped" {
		return workerUnknown(ctx, a, j, r, errors.New("created instance final state is not stable"))
	}
	current, err = workerInstance(ctx, a, j)
	if err != nil {
		return err
	}
	if finalState != "running" || platform.Text(current["desired_state"]) != "running" {
		if finalState == "stopped" {
			if err = workerConfirmStep(ctx, a, j, r, "stopped", "", platform.Row{"provisioning_state": "ready"}, "create.verify"); err != nil {
				return workerUnknown(ctx, a, j, r, err)
			}
		}
		return a.TaskResult(ctx, j, platform.Row{"instance_id": strconv.FormatUint(j.InstanceID, 10), "observed_state": finalState, "initialization": "pending"})
	}
	if err = workerConfirm(ctx, a, j, r, finalState, finalState, platform.Row{"provisioning_state": "ready"}); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	if err = a.Step(ctx, j, "state.start", "", "succeeded"); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	if err = workerConfirmStep(ctx, a, j, r, finalState, "", platform.Row{"incus_config_snapshot": gorm.Expr("JSON_SET(incus_config_snapshot, '$.creation_delivered', true)")}, "create.verify"); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	return a.TaskResult(ctx, j, platform.Row{"instance_id": strconv.FormatUint(j.InstanceID, 10), "observed_state": finalState})
}

func runLifecycle(ctx context.Context, a *platform.App, j platform.Job) error {
	r, err := workerInstance(ctx, a, j)
	if err != nil {
		return err
	}
	if _, _, err = workerObserve(ctx, a, r); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	p, err := workerParams(j)
	if err != nil {
		return err
	}
	action := strings.TrimPrefix(j.Type, "instance.")
	want, event := "running", "running"
	if action == "stop" {
		want, event = "stopped", "stopped"
	}
	if action != "start" && action != "stop" && action != "restart" {
		return errors.New("invalid lifecycle task")
	}
	skip := func() error {
		return a.TaskResult(ctx, j, platform.Row{"instance_id": strconv.FormatUint(j.InstanceID, 10), "skipped": true, "reason": "superseded or no longer authorized"})
	}
	check := func() error {
		current, e := workerInstance(ctx, a, j)
		if e != nil {
			return e
		}
		if action != "stop" {
			if current["creation_failed_at"] != nil {
				return workerSkipped
			}
			if platform.Text(current["desired_state"]) != "running" {
				return workerSkipped
			}
			return a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
				if err := lockWorkerAccount(db, a, j, j.UserID); err != nil {
					return err
				}
				return billing.AdmitInstance(ctx, db, current, action)
			})
		}
		reason := platform.Text(p["reason"])
		if strings.HasPrefix(reason, "credit-enforcement:") {
			id, e := strconv.ParseUint(strings.TrimPrefix(reason, "credit-enforcement:"), 10, 64)
			if e != nil {
				return e
			}
			var proceed bool
			e = a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
				if err := lockWorkerAccount(db, a, j, j.UserID); err != nil {
					return err
				}
				var x error
				proceed, x = billing.CheckCreditStop(ctx, db, j.InstanceID, id)
				return x
			})
			if e != nil {
				return e
			}
			if !proceed {
				return workerSkipped
			}
		}
		if reason == "automatic_gpu_idle" {
			return workerIdleStop(ctx, a, current)
		}
		return nil
	}
	state, err := workerState(ctx, a, r)
	if err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	prior, e := workerStep(ctx, a, j, "state."+action)
	if e != nil {
		return e
	}
	if state != want || action == "restart" || prior != nil {
		if prior == nil {
			if e = check(); e != nil {
				if errors.Is(e, workerSkipped) {
					return skip()
				}
				return e
			}
		}
		timeout := uint64(30)
		if platform.Text(p["reason"]) == "automatic_gpu_idle" {
			policy, e := a.InstancePolicy(ctx, r, "gpu-idle")
			if e != nil {
				return e
			}
			timeout = platform.Uint(policy["graceful_stop_timeout_seconds"])
			if timeout == 0 {
				return errors.New("idle stop timeout not configured")
			}
			if platform.Bool(p["force"]) {
				return errors.New("automatic idle stop cannot force termination")
			}
		}
		confirmed := func() (bool, error) {
			if _, _, e := workerObserve(ctx, a, r); e != nil {
				return false, e
			}
			s, e := workerState(ctx, a, r)
			return s == want, e
		}
		if action == "restart" {
			confirmed = nil
		}
		_, err = workerOperation(ctx, a, j, "state."+action, func() (map[string]any, error) {
			if e := check(); e != nil {
				return nil, e
			}
			if _, _, e := workerObserve(ctx, a, r); e != nil {
				return nil, e
			}
			return a.Agent(platform.WithTaskDispatch(ctx, j, "state."+action), j.NodeID, http.MethodPut, workerPath(r, "/state"), platform.Row{"action": action, "timeout": timeout, "force": platform.Bool(p["force"]), "stateful": false})
		}, confirmed)
		if errors.Is(err, workerSkipped) {
			return skip()
		}
		if err != nil && action == "stop" && platform.Text(p["reason"]) == "automatic_gpu_idle" && !platform.Bool(p["force"]) {
			policy, policyErr := a.InstancePolicy(ctx, r, "gpu-idle")
			if policyErr != nil {
				return workerUnknown(ctx, a, j, r, policyErr)
			}
			if platform.Bool(policy["allow_force_stop"]) {
				var operationError *platform.HTTPError
				if errors.As(err, &operationError) && operationError.Code == "operation_failed" {
					if stepErr := a.Step(ctx, j, "state."+action, "", "failed"); stepErr != nil {
						return workerUnknown(ctx, a, j, r, stepErr)
					}
				}
				current, currentErr := workerInstance(ctx, a, j)
				if currentErr != nil {
					return workerUnknown(ctx, a, j, r, currentErr)
				}
				if currentErr = workerIdleStop(ctx, a, current); errors.Is(currentErr, workerSkipped) {
					return skip()
				} else if currentErr != nil {
					return workerUnknown(ctx, a, j, r, currentErr)
				}
				_, err = workerOperation(ctx, a, j, "state.idle-force-stop", func() (map[string]any, error) {
					latestPolicy, e := a.InstancePolicy(ctx, current, "gpu-idle")
					if e != nil {
						return nil, e
					}
					if !platform.Bool(latestPolicy["allow_force_stop"]) {
						return nil, workerSkipped
					}
					if e = check(); e != nil {
						return nil, e
					}
					if _, _, e = workerObserve(ctx, a, r); e != nil {
						return nil, e
					}
					return a.Agent(platform.WithTaskDispatch(ctx, j, "state.idle-force-stop"), j.NodeID, http.MethodPut, workerPath(r, "/state"), platform.Row{"action": "stop", "timeout": timeout, "force": true, "stateful": false})
				}, confirmed)
				if errors.Is(err, workerSkipped) {
					return skip()
				}
			}
		}
		if err != nil {
			return workerUnknown(ctx, a, j, r, err)
		}
	}
	if _, _, err = workerObserve(ctx, a, r); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	state, err = workerState(ctx, a, r)
	if err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	current, err := workerInstance(ctx, a, j)
	if err != nil {
		return err
	}
	if action != "stop" && platform.Text(current["desired_state"]) != "running" {
		if state == "running" {
			return workerUnknown(ctx, a, j, r, workerLateStop(ctx, a, j, r))
		}
		if err = workerConfirm(ctx, a, j, r, state, "stopped", platform.Row{"provisioning_state": "ready"}); err != nil {
			return workerUnknown(ctx, a, j, r, err)
		}
		return skip()
	}
	if state != want {
		return workerUnknown(ctx, a, j, r, errors.New("lifecycle operation has not reached its required state"))
	}
	var confirmedResources platform.Row
	if action == "stop" && p["resize_resources"] != nil {
		target := object(p["resize_resources"])
		if platform.Uint(target["vcpus"]) == 0 || platform.Uint(target["memory_bytes"]) == 0 || platform.Uint(target["disk_bytes"]) == 0 {
			return workerUnknown(ctx, a, j, r, errors.New("resize stop reservation is incomplete"))
		}
		confirmedResources = platform.Row{"vcpus": target["vcpus"], "memory_bytes": target["memory_bytes"], "disk_bytes": target["disk_bytes"]}
		event = "resize_stopped"
	}
	if err = workerConfirm(ctx, a, j, r, state, event, confirmedResources); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	if action == "start" {
		if err = workerInitialize(ctx, a, j, r); err != nil {
			return workerUnknown(ctx, a, j, r, err)
		}
	}
	if action == "start" {
		if _, _, err = workerObserve(ctx, a, r); err != nil {
			return workerUnknown(ctx, a, j, r, err)
		}
		state, err = workerState(ctx, a, r)
		if err != nil {
			return workerUnknown(ctx, a, j, r, err)
		}
		current, err = workerInstance(ctx, a, j)
		if err != nil {
			return err
		}
		if platform.Text(current["desired_state"]) != "running" {
			if state == "running" {
				return workerUnknown(ctx, a, j, r, workerLateStop(ctx, a, j, r))
			}
			if err = workerConfirm(ctx, a, j, r, state, "stopped", platform.Row{"provisioning_state": "ready"}); err != nil {
				return workerUnknown(ctx, a, j, r, err)
			}
			return skip()
		}
		if state != "running" {
			return workerUnknown(ctx, a, j, r, errors.New("instance stopped during initialization"))
		}
	}
	if err = workerConfirm(ctx, a, j, r, state, "", platform.Row{"provisioning_state": "ready"}); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	return a.TaskResult(ctx, j, platform.Row{"instance_id": strconv.FormatUint(j.InstanceID, 10), "observed_state": state})
}

var workerEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func workerInitialize(ctx context.Context, a *platform.App, j platform.Job, r platform.Row) error {
	if err := workerLoginInitialize(ctx, a, j, r); err != nil {
		return err
	}
	id := platform.Uint(r["init_template_version_id"])
	if id == 0 {
		return nil
	}
	var initialized int64
	if err := a.DB.WithContext(ctx).Table("instance_init_runs").Where("instance_id = ? AND init_template_version_id = ? AND status = 'succeeded'", j.InstanceID, id).Count(&initialized).Error; err != nil {
		return err
	}
	if initialized > 0 {
		return nil
	}
	template, err := platform.Get(a.DB.WithContext(ctx), "init_template_versions", id, false)
	if err != nil {
		return err
	}
	if status := platform.Text(template["status"]); status != "active" && status != "retired" {
		return initializationError("init.template", "initialization_script_failed", "template_status_mismatch", "unknown", nil)
	}
	script := platform.Text(template["script_content"])
	sum := sha256.Sum256([]byte(script))
	if workerFingerprint(template["script_sha256"]) != hex.EncodeToString(sum[:]) {
		return initializationError("init.template", "initialization_script_failed", "template_hash_mismatch", "unknown", nil)
	}
	seconds := platform.Uint(template["timeout_seconds"])
	if seconds == 0 {
		return initializationError("init.template", "initialization_script_failed", "template_timeout_invalid", "unknown", nil)
	}
	params := object(r["init_parameters"])
	env := platform.Row{"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	for key, value := range params {
		if !workerEnvName.MatchString(key) {
			continue
		}
		var text string
		switch v := value.(type) {
		case string:
			text = v
		case bool, float64, json.Number:
			text = fmt.Sprint(v)
		default:
			text = jsonText(value)
		}
		if strings.IndexByte(text, 0) >= 0 {
			continue
		}
		env["INIT_PARAM_"+key] = text
	}
	env["INIT_PARAMETERS_JSON"] = jsonText(params)
	timeout, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	var run platform.Row
	err = a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if e := lockWorkerAccount(db, a, j, j.UserID); e != nil {
			return e
		}
		if _, e := platform.Get(db, "instances", j.InstanceID, true); e != nil {
			return e
		}
		var existing []platform.Row
		if e := db.Table("instance_init_runs").Where("instance_id = ? AND task_id = ?", j.InstanceID, j.ID).Find(&existing).Error; e != nil {
			return e
		}
		if len(existing) > 0 {
			run = existing[0]
			return nil
		}
		var n uint64
		if e := db.Table("instance_init_runs").Where("instance_id = ?", j.InstanceID).Select("COALESCE(MAX(run_number),0)").Scan(&n).Error; e != nil {
			return e
		}
		run = platform.Row{"instance_id": j.InstanceID, "task_id": j.ID, "init_template_version_id": id, "run_number": n + 1, "status": "running", "parameters_redacted": "{}", "started_at": a.Time()}
		if e := db.Table("instance_init_runs").Create(&run).Error; e != nil {
			return e
		}
		_, e := platform.InsertedID(run)
		return e
	})
	if err != nil {
		return err
	}
	exec := func(key string, command []string) (platform.Row, error) {
		return workerOperation(timeout, a, j, key, func() (map[string]any, error) {
			current, e := workerInstance(timeout, a, j)
			if e != nil {
				return nil, e
			}
			if platform.Text(current["desired_state"]) != "running" {
				return nil, workerSkipped
			}
			if _, _, e := workerObserve(timeout, a, r); e != nil {
				return nil, e
			}
			return a.Agent(platform.WithTaskDispatch(timeout, j, key), j.NodeID, http.MethodPost, workerPath(r, "/exec"), platform.Row{"command": command, "environment": env, "interactive": false, "wait-for-websocket": false, "record-output": false})
		}, nil)
	}
	if platform.Text(template["phase"]) == "cloud_init" {
		result, e := exec("init.cloud-ready", []string{"/usr/bin/timeout", "--signal=KILL", strconv.FormatUint(seconds, 10), "/bin/sh", "-c", "/usr/bin/cloud-init status --wait >/dev/null 2>&1"})
		if e != nil {
			return workerInitFailure(ctx, a, j, e)
		}
		if e = workerExit(result); e != nil {
			return workerInitFailure(ctx, a, j, e)
		}
	}
	result, err := exec("init.template", []string{"/usr/bin/timeout", "--signal=KILL", strconv.FormatUint(seconds, 10), "/bin/sh", "-c", script})
	if err != nil {
		return workerInitFailure(ctx, a, j, err)
	}
	if err = workerExit(result); err != nil {
		return workerInitFailure(ctx, a, j, err)
	}
	return a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if err := workerLease(a, db, j); err != nil {
			return err
		}
		return db.Table("instance_init_runs").Where("instance_id = ? AND task_id = ?", j.InstanceID, j.ID).Updates(platform.Row{"status": "succeeded", "exit_code": 0, "finished_at": a.Time(), "revision": gorm.Expr("revision + 1")}).Error
	})
}

// Exec operation completion only proves transport completion. Persist success
// only after inspecting the process exit status, including during recovery.
func workerValidateOperation(ctx context.Context, a *platform.App, j platform.Job, key, op string, result platform.Row) error {
	if !initializationStage(key) {
		return nil
	}
	n, known := initializationExit(result)
	if !known || n != 0 {
		status := "failed"
		var err error
		if !known {
			status = "unknown"
			err = initializationError(key, "initialization_result_unknown", "result_unknown", "unknown", nil)
		} else if key != "init.template" {
			err = workerCloudInitDiagnostic(ctx, a, j, key, n)
		} else if n == 124 {
			err = initializationError(key, "initialization_timeout", "timeout", "unknown", &n)
		} else if n == 137 {
			err = initializationTerminatedError(key, n)
		} else {
			err = initializationError(key, "initialization_script_failed", "script_exit_nonzero", "unknown", &n)
		}
		if stepErr := a.Step(context.WithoutCancel(ctx), j, key, op, status); stepErr != nil {
			return platform.Unknown(err)
		}
		return platform.Unknown(err)
	}
	return nil
}
func workerExit(operation platform.Row) error {
	metadata := object(operation["metadata"])
	value, ok := metadata["return"]
	if !ok {
		return platform.Unknown(errors.New("exec exit status unavailable; script must not be replayed"))
	}
	n, err := strconv.ParseInt(platform.Text(value), 10, 32)
	if err != nil {
		return platform.Unknown(errors.New("invalid exec exit status"))
	}
	if n != 0 {
		return fmt.Errorf("initialization command exited with status %d", n)
	}
	return nil
}
func workerInitFailure(ctx context.Context, a *platform.App, j platform.Job, err error) error {
	_ = a.DB.WithContext(context.WithoutCancel(ctx)).Transaction(func(db *gorm.DB) error {
		if err := workerLease(a, db, j); err != nil {
			return err
		}
		return db.Table("instance_init_runs").Where("instance_id = ? AND task_id = ?", j.InstanceID, j.ID).Updates(platform.Row{"status": "unknown", "revision": gorm.Expr("revision + 1")}).Error
	})
	return platform.Unknown(err)
}

// Preserve user-data for the native PUT only; this is not a persistable snapshot.
func workerWritable(m platform.Row) platform.Row {
	return platform.Row{"architecture": m["architecture"], "config": object(m["config"]), "devices": object(m["devices"]), "profiles": m["profiles"], "ephemeral": m["ephemeral"], "description": m["description"]}
}
func workerResourcesMatch(m platform.Row, cpu uint64, memory, disk string) bool {
	cfg := object(m["expanded_config"])
	if len(cfg) == 0 {
		cfg = object(m["config"])
	}
	devices := object(m["expanded_devices"])
	if len(devices) == 0 {
		devices = object(m["devices"])
	}
	root := object(devices["root"])
	actualMemory, e1 := workerByteSize(platform.Text(cfg["limits.memory"]))
	actualDisk, e2 := workerByteSize(platform.Text(root["size"]))
	expectedMemory, e3 := workerByteSize(memory)
	expectedDisk, e4 := workerByteSize(disk)
	return e1 == nil && e2 == nil && e3 == nil && e4 == nil && platform.Text(cfg["limits.cpu"]) == strconv.FormatUint(cpu, 10) && actualMemory == expectedMemory && actualDisk == expectedDisk
}
func runResize(ctx context.Context, a *platform.App, j platform.Job) error {
	r, err := workerInstance(ctx, a, j)
	if err != nil {
		return err
	}
	var p struct {
		Resources Resources `json:"resources"`
		Previous  Resources `json:"previous_resources"`
		Restart   bool      `json:"restart_after_resize"`
	}
	if err = json.Unmarshal(j.Parameters, &p); err != nil {
		return err
	}
	if p.Resources.VCPUs == 0 || p.Resources.MemoryBytes == "" || p.Resources.DiskBytes == "" {
		return errors.New("resize resources missing")
	}
	confirmed := func() (bool, error) {
		v, _, e := workerObserve(ctx, a, r)
		if e != nil {
			return false, e
		}
		return workerResourcesMatch(v, p.Resources.VCPUs, p.Resources.MemoryBytes, p.Resources.DiskBytes), nil
	}
	prior, err := workerStep(ctx, a, j, "resize.native")
	if err != nil {
		return err
	}
	checkpoint, err := workerStep(ctx, a, j, "resize.confirmed")
	if err != nil {
		return err
	}
	startIntent, err := workerStep(ctx, a, j, "state.start")
	if err != nil {
		return err
	}
	settled := checkpoint != nil && platform.Text(checkpoint["status"]) == "succeeded"
	if startIntent != nil && (!p.Restart || !settled) {
		return workerUnknown(ctx, a, j, r, errors.New("resize start intent lacks its accounting checkpoint"))
	}
	// Once START has an intent, neither STOP nor resize may be dispatched again.
	// Even a changed native configuration is an unknown outcome, not replay authority.
	if !settled && startIntent == nil {
		if prior == nil {
			old, e := workerByteSize(p.Previous.DiskBytes)
			if e != nil {
				return e
			}
			next, e := workerByteSize(p.Resources.DiskBytes)
			if e != nil {
				return e
			}
			if next < old {
				return errors.New("disk shrinking is forbidden")
			}
			if e = workerDiskCapacity(ctx, a, r, next-old); e != nil {
				return workerUnknown(ctx, a, j, r, e)
			}
			if p.Restart {
				stop := j
				stop.Type = "instance.stop"
				stop.Parameters = json.RawMessage(jsonText(platform.Row{"force": false, "reason": "instance resize", "resize_resources": p.Resources}))
				if e = runLifecycle(ctx, a, stop); e != nil {
					return e
				}
			}
		}
		m, etag, e := workerObserve(ctx, a, r)
		if e != nil {
			return workerUnknown(ctx, a, j, r, e)
		}
		state, e := workerState(ctx, a, r)
		if e != nil || state != "stopped" {
			if e == nil {
				e = errors.New("resize requires a confirmed stopped instance; earlier intent cannot authorize another stop")
			}
			return workerUnknown(ctx, a, j, r, e)
		}
		if etag == "" {
			return workerUnknown(ctx, a, j, r, errors.New("native instance omitted ETag; resize cannot protect concurrent configuration"))
		}
		body := workerWritable(m)
		cfg := object(body["config"])
		devices := object(body["devices"])
		root := object(devices["root"])
		if platform.Text(root["type"]) != "disk" || platform.Text(root["path"]) != "/" || platform.Text(root["pool"]) != platform.Text(r["storage_pool"]) {
			return workerUnknown(ctx, a, j, r, errors.New("resize root device changed"))
		}
		cfg["limits.cpu"] = strconv.FormatUint(p.Resources.VCPUs, 10)
		cfg["limits.memory"] = p.Resources.MemoryBytes
		root["size"] = p.Resources.DiskBytes
		devices["root"] = root
		_, err = workerOperation(ctx, a, j, "resize.native", func() (map[string]any, error) {
			v, _, e := a.AgentConditional(platform.WithTaskDispatch(ctx, j, "resize.native"), j.NodeID, http.MethodPut, workerPath(r, ""), body, etag)
			return v, e
		}, confirmed)
		if err != nil {
			return workerUnknown(ctx, a, j, r, err)
		}
		ok, e := confirmed()
		if e != nil || !ok {
			if e == nil {
				e = errors.New("resized resources do not match approval")
			}
			return workerUnknown(ctx, a, j, r, e)
		}
		if err = workerConfirmStep(ctx, a, j, r, "stopped", "resize", platform.Row{"vcpus": p.Resources.VCPUs, "memory_bytes": p.Resources.MemoryBytes, "disk_bytes": p.Resources.DiskBytes, "provisioning_state": "ready"}, "resize.confirmed"); err != nil {
			return workerUnknown(ctx, a, j, r, err)
		}
	}
	ok, err := confirmed()
	if err != nil || !ok {
		if err == nil {
			err = errors.New("resized resources changed after confirmation; refusing lifecycle replay")
		}
		return workerUnknown(ctx, a, j, r, err)
	}
	want := "stopped"
	if p.Restart {
		want = "running"
		start := j
		start.Type = "instance.start"
		start.Parameters = json.RawMessage(`{"force":false}`)
		if err = runLifecycle(ctx, a, start); err != nil {
			return err
		}
	}
	state, err := workerState(ctx, a, r)
	if err != nil || state != want {
		if err == nil {
			err = errors.New("resize final state does not match its approved restart policy")
		}
		return workerUnknown(ctx, a, j, r, err)
	}
	if err = workerConfirm(ctx, a, j, r, state, "", platform.Row{"provisioning_state": "ready"}); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	return a.TaskResult(ctx, j, platform.Row{"instance_id": strconv.FormatUint(j.InstanceID, 10), "resources": p.Resources, "restart_after_resize": p.Restart, "observed_state": state})
}

func workerAbsent(ctx context.Context, a *platform.App, r platform.Row) (bool, error) {
	// Enumerating the approved project avoids interpreting an arbitrary transport
	// error, or an untyped 404, as permission to release the instance's resources.
	v, err := a.Agent(ctx, platform.Uint(r["node_id"]), http.MethodGet, "/1.0/instances?recursion=1&project="+url.QueryEscape(platform.Text(r["project_name"])), nil)
	if err != nil {
		return false, err
	}
	list, ok := v["metadata"].([]any)
	if !ok || platform.Text(v["type"]) == "error" || platform.Uint(v["error_code"]) >= 400 {
		return false, errors.New("native instance enumeration incomplete")
	}
	for _, item := range list {
		m := object(item)
		if platform.Text(m["name"]) == "" {
			return false, errors.New("native instance enumeration contains incomplete identity")
		}
		cfg := object(m["expanded_config"])
		if len(cfg) == 0 {
			cfg = object(m["config"])
		}
		if platform.Text(m["name"]) == platform.Text(r["incus_name"]) || (platform.Text(r["incus_uuid"]) != "" && platform.Text(cfg["volatile.uuid"]) == platform.Text(r["incus_uuid"])) || (platform.Text(cfg["user.101cloud.instance_id"]) != "" && platform.Text(cfg["user.101cloud.instance_id"]) == platform.Text(r["id"])) {
			return false, nil
		}
	}
	return true, nil
}
func runDelete(ctx context.Context, a *platform.App, j platform.Job) error {
	r, err := workerInstance(ctx, a, j)
	if err != nil {
		return err
	}
	if platform.Text(r["observed_state"]) == "deleted" {
		return nil
	}
	p, err := workerParams(j)
	if err != nil {
		return err
	}
	cleanup := platform.Bool(p["cleanup_stuck"])
	if cleanup {
		preflightAbsent, preflightErr := workerDeleteAbsent(ctx, a, r, true)
		if preflightErr != nil {
			return workerUnknown(ctx, a, j, r, preflightErr)
		}
		if !preflightAbsent {
			if _, _, observeErr := workerObserve(ctx, a, r); observeErr != nil && !nativeMissing(observeErr) {
				return workerUnknown(ctx, a, j, r, observeErr)
			}
		}
		if err = stuckDeleteQuiescent(ctx, a, j, r); err != nil {
			return workerUnknown(ctx, a, j, r, err)
		}
	}
	old, err := workerStep(ctx, a, j, "delete.native")
	if err != nil {
		return err
	}
	absent, err := workerDeleteAbsent(ctx, a, r, cleanup)
	if err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	if absent && old == nil && !cleanup {
		return workerUnknown(ctx, a, j, r, errors.New("instance disappeared without a durable delete intent"))
	}
	if !absent {
		if _, _, err = workerObserve(ctx, a, r); err != nil {
			if !cleanup || !nativeMissing(err) {
				return workerUnknown(ctx, a, j, r, err)
			}
			absent, err = workerDeleteAbsent(ctx, a, r, true)
			if err != nil || !absent {
				return workerUnknown(ctx, a, j, r, errors.New("native target absence could not be confirmed"))
			}
		}
	}
	if !absent {
		state, e := workerState(ctx, a, r)
		if e != nil {
			return workerUnknown(ctx, a, j, r, e)
		}
		if state != "stopped" {
			if old != nil {
				return workerUnknown(ctx, a, j, r, errors.New("delete intent already exists; refusing to replay stop"))
			}
			stop := j
			stop.Type = "instance.stop"
			stop.Parameters = json.RawMessage(jsonText(platform.Row{"force": platform.Bool(p["force"]), "reason": "instance deletion", "cleanup_stuck": cleanup}))
			if e = runLifecycle(ctx, a, stop); e != nil {
				return e
			}
		}
	}
	if !cleanup || !absent {
		_, err = workerOperation(ctx, a, j, "delete.native", func() (map[string]any, error) {
			if _, _, e := workerObserve(ctx, a, r); e != nil {
				return nil, e
			}
			state, e := workerState(ctx, a, r)
			if e != nil {
				return nil, e
			}
			if state != "stopped" {
				return nil, errors.New("native deletion requires a freshly confirmed stopped instance")
			}
			return a.Agent(platform.WithTaskDispatch(ctx, j, "delete.native"), j.NodeID, http.MethodDelete, workerPath(r, ""), nil)
		}, func() (bool, error) { return workerDeleteAbsent(ctx, a, r, cleanup) })
		if err != nil {
			return workerUnknown(ctx, a, j, r, err)
		}
	} else if err = a.Step(ctx, j, "delete.native", "", "succeeded"); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	absent, err = workerDeleteAbsent(ctx, a, r, cleanup)
	if err != nil || !absent {
		if err == nil {
			err = errors.New("deleted instance is still present")
		}
		return workerUnknown(ctx, a, j, r, err)
	}
	// A cached frame collected before this confirmation cannot authorize release.
	confirmedAt := a.Time()
	if err = a.Step(ctx, j, "delete.verify_reclamation", "", "running"); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	if !cleanup {
		if err = workerDeletedGPUs(ctx, a, r, confirmedAt); err != nil {
			return workerUnknown(ctx, a, j, r, err)
		}
	}
	if err = a.Step(ctx, j, "delete.verify_reclamation", "", "succeeded"); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	if err = a.Step(ctx, j, "delete.finalize", "", "running"); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	err = a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if e := lockWorkerAccount(db, a, j, j.UserID); e != nil {
			return e
		}
		current, e := platform.Get(db, "instances", j.InstanceID, true)
		if e != nil {
			return e
		}
		if current["deleted_at"] != nil {
			return nil
		}
		now := a.Time()
		if e = billing.ConfirmInstanceUsage(ctx, db, current, "deleted", "instance.delete", "instance.delete:"+strconv.FormatUint(j.InstanceID, 10), now); e != nil {
			return e
		}
		if e = db.Table("gpu_allocations").Where("instance_id = ? AND active_slot = 1", j.InstanceID).Updates(platform.Row{"status": "released", "configured": false, "observed": false, "release_verified": true, "released_at": now, "last_verified_at": now, "revision": gorm.Expr("revision + 1")}).Error; e != nil {
			return e
		}
		if e = db.Table("ip_addresses").Where("instance_id = ?", j.InstanceID).Updates(platform.Row{"status": "available", "node_id": nil, "node_network_binding_id": nil, "instance_id": nil, "reservation_task_id": nil, "reserved_at": nil, "reservation_expires_at": nil, "bound_at": nil, "revision": gorm.Expr("revision + 1")}).Error; e != nil {
			return e
		}
		return db.Table("instances").Where("id = ?", j.InstanceID).Updates(platform.Row{"desired_state": "deleted", "observed_state": "deleted", "provisioning_state": "deleted", "deleted_at": now, "observed_at": now, "revision": gorm.Expr("revision + 1")}).Error
	})
	if err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	if err = a.Step(ctx, j, "delete.finalize", "", "succeeded"); err != nil {
		return workerUnknown(ctx, a, j, r, err)
	}
	return a.TaskResult(ctx, j, platform.Row{"instance_id": strconv.FormatUint(j.InstanceID, 10), "observed_state": "deleted"})
}
func workerDeletedGPUs(ctx context.Context, a *platform.App, r platform.Row, confirmedAt time.Time) error {
	var count int64
	if err := a.DB.WithContext(ctx).Table("gpu_allocations").Where("instance_id = ? AND active_slot = 1", r["id"]).Count(&count).Error; err != nil {
		return workerGPUReclamationError("allocation_inventory_unavailable")
	}
	if count == 0 {
		return nil
	}
	// Allow one full 60-second collection interval and a 30-second collection,
	// with polling margin. The deadline also bounds agent requests and respects
	// cancellation; retries only read evidence, never replay the native DELETE.
	waitCtx, cancel := context.WithTimeout(ctx, 95*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	reason := "metrics_unavailable"
	for {
		if waitCtx.Err() != nil {
			return workerGPUReclamationError(reason)
		}
		frame, err := a.Agent(waitCtx, platform.Uint(r["node_id"]), http.MethodGet, "/agent/v1/instances/metrics?project=*", nil)
		if err != nil {
			reason = "metrics_unavailable"
		} else {
			reason = workerDeletedGPUFrame(frame, r, confirmedAt, a.Time())
			if reason == "" {
				return nil
			}
		}
		select {
		case <-waitCtx.Done():
			return workerGPUReclamationError(reason)
		case <-tick.C:
		}
	}
}

func workerGPUReclamationError(reason string) error {
	return platform.EDetails(409, "gpu_reclamation_unverified", "原生实例已删除，但 GPU 回收证据尚未确认；IP、GPU及配额预留仍保留。", platform.Row{
		"stage": "delete.verify_reclamation", "reason": reason, "native_deletion_confirmed": true, "resource_reservations_released": false,
		"action": "请管理员检查节点监控采集是否完整且时间同步，等待删除后的完整新样本，再对原任务执行对账；请勿重复删除或手工释放预留。",
	})
}

// Host attribution covers observed process/context/reference and inventory
// integrity, not physical-card idleness or all host FDs. Owned shared-control
// uncertainty may belong solely to surviving instances, never to this absent
// target. Unrelated stopped-instance quality is not host attribution evidence.
func workerDeletedGPUFrame(frame map[string]any, r platform.Row, confirmedAt, now time.Time) string {
	started, startErr := time.Parse(time.RFC3339Nano, platform.Text(frame["collection_started_at"]))
	collected, collectedErr := time.Parse(time.RFC3339Nano, platform.Text(frame["collected_at"]))
	if startErr != nil || collectedErr != nil || started.IsZero() || collected.IsZero() || collected.Before(started) || collected.After(now.Add(5*time.Second)) {
		return "invalid_collection_time"
	}
	if !started.After(confirmedAt) {
		return "collection_predates_deletion"
	}
	if now.Sub(collected) > 30*time.Second || frame["stale"] != false {
		return "stale_collection"
	}
	if frame["collection_complete"] != true {
		return "incomplete_inventory"
	}
	host := object(frame["host_quality"])
	sharedOnly := host["attribution_complete"] != true
	if sharedOnly {
		if _, valid := frame["host_quality"].(map[string]any); !valid {
			return "incomplete_host_attribution"
		}
		if host["attribution_complete"] != false || !workerDeletedSharedQuality(host) {
			return "incomplete_host_attribution"
		}
		anonymous, ok := frame["anonymous"].(map[string]any)
		if !ok || !workerDeletedNumericZero(anonymous["unattributed_contexts"]) || !workerDeletedNumericZero(anonymous["unresolved_gpu_references"]) || !workerDeletedNumericZero(anonymous["unattributed_activity_samples"]) {
			return "unverified_anonymous_gpu_evidence"
		}
	}
	items, ok := frame["instances"].([]any)
	if !ok {
		return "missing_instance_inventory"
	}
	uuid, name, project := platform.Text(r["incus_uuid"]), platform.Text(r["incus_name"]), platform.Text(r["project_name"])
	if uuid == "" || name == "" || project == "" {
		return "missing_target_identity"
	}
	holders := 0
	type instanceName struct{ name, project string }
	var seenUUIDs map[string]bool
	var seenNames map[instanceName]bool
	if sharedOnly {
		seenUUIDs = make(map[string]bool, len(items))
		seenNames = make(map[instanceName]bool, len(items))
	}
	for _, item := range items {
		m, valid := item.(map[string]any)
		if !valid {
			return "incomplete_instance_identity"
		}
		itemUUID, uuidOK := m["uuid"].(string)
		itemName, nameOK := m["name"].(string)
		itemProject, projectOK := m["project"].(string)
		if !uuidOK || !nameOK || !projectOK || itemUUID == "" || itemName == "" || itemProject == "" {
			return "incomplete_instance_identity"
		}
		if itemUUID == uuid || (itemName == name && itemProject == project) {
			return "target_still_observed"
		}
		if sharedOnly {
			identity := instanceName{itemName, itemProject}
			if seenUUIDs[itemUUID] || seenNames[identity] {
				return "ambiguous_instance_inventory"
			}
			seenUUIDs[itemUUID], seenNames[identity] = true, true
			q, _ := m["quality"].(map[string]any)
			if q["attribution_complete"] == false && workerDeletedSharedQuality(q) {
				state, explicit := m["state"].(string)
				if !explicit || (!strings.EqualFold(state, "running") && !strings.EqualFold(state, "frozen")) {
					return "unverified_shared_gpu_holder"
				}
				holders++
			} else {
				// Unrelated stopped rows may lack runtime PID evidence. Every
				// other row still needs consistent full-attribution evidence;
				// neither may hide an unknown reference or malformed reason.
				if !workerDeletedNonHolderQuality(q, strings.EqualFold(platform.Text(m["state"]), "stopped")) {
					return "unverified_shared_gpu_holder"
				}
			}
		}
	}
	if sharedOnly && holders == 0 {
		return "missing_shared_gpu_holder"
	}
	return ""
}

// An unrelated stopped instance may lack a runtime PID, but no non-holder row
// may hide unknown GPU/reference evidence on the shared-owner-only path.
func workerDeletedNonHolderQuality(q map[string]any, stopped bool) bool {
	switch q["reasons"].(type) {
	case []any, []string:
	default:
		return false
	}
	if full, explicit := q["attribution_complete"].(bool); !explicit || (!stopped && !full) {
		return false
	}
	for _, value := range rows(q["reasons"]) {
		reason, valid := value.(string)
		if !valid {
			return false
		}
		switch reason {
		case "instance_not_running", "instance_attribution_unknown":
			if !stopped {
				return false
			}
		case "gpu_metric_unavailable", "gpu_utilization_unknown", "gpu_utilization_no_samples", "gpu_utilization_stale", "gpu_utilization_identity_unconfirmed":
		default:
			return false
		}
	}
	return true
}

// Reuse the context/reason allowlist, but do not accept a JSON-encoded string
// where the Agent protocol requires an actual reasons array.
func workerDeletedSharedQuality(q map[string]any) bool {
	switch q["reasons"].(type) {
	case []any, []string:
		return sharedControlContextComplete(q)
	default:
		return false
	}
}

// Agent responses use Decoder.UseNumber. Do not coerce absent, boolean, string,
// negative or fractional/underflowed nonzero counters into release evidence.
func workerDeletedNumericZero(value any) bool {
	switch n := value.(type) {
	case json.Number:
		s := n.String()
		number, err := strconv.ParseFloat(s, 64)
		if err != nil || number != 0 || len(s) == 0 || s[0] == '-' {
			return false
		}
		for _, c := range s {
			if c == 'e' || c == 'E' {
				return true
			}
			if c != '0' && c != '.' {
				return false
			}
		}
		return true
	case int:
		return n == 0
	case int64:
		return n == 0
	case uint64:
		return n == 0
	case float64:
		return n == 0
	default:
		return false
	}
}

func runImport(ctx context.Context, a *platform.App, j platform.Job) error {
	var p struct {
		Owner          string            `json:"owner_user_id"`
		UUID           string            `json:"incus_uuid"`
		Approved       Spec              `json:"approved_config"`
		PolicyVersions map[string]string `json:"policy_versions"`
	}
	if err := json.Unmarshal(j.Parameters, &p); err != nil {
		return err
	}
	owner, err := strconv.ParseUint(p.Owner, 10, 64)
	if err != nil || owner != j.UserID || p.UUID == "" {
		return errors.New("invalid import ownership or UUID")
	}
	rootID := j.ID
	switch source := ctx.Value(workerRecoveryKey{}).(type) {
	case uint64:
		if source != 0 && source < rootID {
			rootID = source
		}
	case []uint64:
		for _, id := range source {
			if id != 0 && id < rootID {
				rootID = id
			}
		}
	}
	bindingID, err := platform.IDString(p.Approved.NetworkOptionID)
	if err != nil {
		return err
	}
	binding, err := platform.Get(a.DB.WithContext(ctx), "node_network_bindings", bindingID, false)
	if err != nil {
		return err
	}
	if platform.Uint(binding["node_id"]) != j.NodeID || platform.Text(binding["status"]) != "active" || platform.Text(binding["validation_status"]) != "verified" {
		return errors.New("import binding is not approved on this node")
	}
	project := platform.Text(binding["project_name"])
	v, err := a.Agent(ctx, j.NodeID, http.MethodGet, "/1.0/instances?recursion=1&project="+url.QueryEscape(project), nil)
	if err != nil {
		return platform.Unknown(err)
	}
	list, ok := v["metadata"].([]any)
	if !ok {
		return platform.Unknown(errors.New("native import discovery incomplete"))
	}
	var native platform.Row
	for _, item := range list {
		m := object(item)
		cfg := object(m["expanded_config"])
		if len(cfg) == 0 {
			cfg = object(m["config"])
		}
		if platform.Text(cfg["volatile.uuid"]) == p.UUID {
			if native != nil {
				return errors.New("duplicate native UUID")
			}
			native = m
		}
	}
	if native == nil {
		return errors.New("approved import UUID not found in approved project")
	}
	identity := platform.Row{"node_id": j.NodeID, "incus_name": native["name"], "incus_uuid": p.UUID, "project_name": project}
	var initialETag string
	native, initialETag, err = workerObserve(ctx, a, identity)
	if err != nil {
		return platform.Unknown(err)
	}
	gpuUUIDs, err := workerImportConfig(ctx, a, j.NodeID, p.Approved, native)
	if err != nil {
		return err
	}
	state, err := workerState(ctx, a, identity)
	if err != nil {
		return platform.Unknown(err)
	}
	if state != "running" && state != "stopped" {
		return errors.New("import requires stable running or stopped state")
	}
	stateResponse, err := a.Agent(ctx, j.NodeID, http.MethodGet, workerPath(identity, "/state"), nil)
	if err != nil {
		return platform.Unknown(err)
	}
	stateMeta, err := workerMetadata(stateResponse)
	if err != nil {
		return err
	}
	devices := object(native["expanded_devices"])
	if len(devices) == 0 {
		devices = object(native["devices"])
	}
	networks := object(stateMeta["network"])
	var addresses []string
	for _, raw := range devices {
		d := object(raw)
		if platform.Text(d["type"]) != "nic" {
			continue
		}
		name := platform.Text(d["name"])
		link := object(networks[name])
		for _, rawAddr := range rows(link["addresses"]) {
			address := object(rawAddr)
			s := platform.Text(address["address"])
			ip, e := netip.ParseAddr(s)
			if e == nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				addresses = append(addresses, ip.String())
			}
		}
		for _, key := range []string{"ipv4.address", "ipv6.address"} {
			if s := platform.Text(d[key]); s != "" {
				addresses = append(addresses, s)
			}
		}
	}
	sort.Strings(addresses)
	addresses = workerUnique(addresses)
	if len(addresses) != 1 {
		return errors.New("import requires exactly one unambiguous approved native IP")
	}
	native["_ip"] = addresses[0]
	native["_gpu_uuids"] = gpuUUIDs
	native["project_name"] = project
	native["node_id"] = j.NodeID
	native["incus_uuid"] = p.UUID
	native["observed_state"] = state
	native["status"] = state
	if err = workerImportNetwork(ctx, a, j.NodeID, p.Approved, native, project, addresses[0]); err != nil {
		return err
	}
	_, finalETag, err := workerObserve(ctx, a, identity)
	if err != nil {
		return platform.Unknown(err)
	}
	if initialETag == "" || finalETag != initialETag {
		return platform.Unknown(errors.New("native configuration changed during import verification"))
	}
	finalState, err := workerState(ctx, a, identity)
	if err != nil {
		return platform.Unknown(err)
	}
	if finalState != state {
		return platform.Unknown(errors.New("native state changed during import verification"))
	}
	p.Approved.GPU.FixedUUIDs = gpuUUIDs
	p.Approved.Name = platform.Text(native["name"])
	var imported platform.Row
	err = a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if e := lockWorkerAccount(db, a, j, owner); e != nil {
			return e
		}
		// Serializes competing imports of this UUID without reserving a random IP.
		if _, e := platform.Get(db, "nodes", j.NodeID, true); e != nil {
			return e
		}
		var prior []platform.Row
		if e := db.Clauses(clause.Locking{Strength: "UPDATE"}).Table("instances").Where("node_id = ? AND incus_uuid = ?", j.NodeID, p.UUID).Find(&prior).Error; e != nil {
			return e
		}
		if len(prior) > 0 {
			if platform.Uint(prior[0]["user_id"]) != owner || platform.Uint(prior[0]["import_task_id"]) != rootID {
				return errors.New("native UUID is already managed by another import")
			}
			imported = prior[0]
			return workerLinkImport(a, db, j, rootID, platform.Uint(imported["id"]))
		}
		native["_import_task_id"] = rootID
		var e error
		imported, e = reserveImported(ctx, a, db, owner, p.Approved, native, p.PolicyVersions)
		if e != nil {
			return e
		}
		if platform.Text(imported["product_type"]) != "custom" || platform.Text(imported["billing_mode"]) != "admin_exempt" {
			return errors.New("import must be custom and admin_exempt")
		}
		if e := workerLinkImport(a, db, j, rootID, platform.Uint(imported["id"])); e != nil {
			return e
		}
		return platform.Audit(ctx, db, 0, j.ID, platform.Uint(imported["id"]), "instance.import", platform.Row{"incus_uuid": p.UUID, "owner_user_id": strconv.FormatUint(owner, 10)})
	})
	if err != nil {
		return err
	}
	return a.TaskResult(ctx, j, platform.Row{"instance_id": strconv.FormatUint(platform.Uint(imported["id"]), 10), "observed_state": state})
}
func workerUnique(values []string) []string {
	out := values[:0]
	for _, s := range values {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}
func workerImportConfig(ctx context.Context, a *platform.App, node uint64, spec Spec, m platform.Row) ([]string, error) {
	if platform.Text(m["type"]) != spec.InstanceType {
		return nil, errors.New("native type differs from approval")
	}
	if !workerResourcesMatch(m, spec.Resources.VCPUs, spec.Resources.MemoryBytes, spec.Resources.DiskBytes) {
		return nil, errors.New("native resources differ from approval")
	}
	if profiles := rows(m["profiles"]); len(profiles) != 0 {
		return nil, errors.New("import requires explicit devices and no inherited profiles")
	}
	cfg := object(m["expanded_config"])
	if len(cfg) == 0 {
		cfg = object(m["config"])
	}
	allowed := map[string]bool{"limits.cpu": true, "limits.memory": true, "security.privileged": true, "security.nesting": true, "nvidia.runtime": true, "nvidia.driver.capabilities": true, "image.architecture": true, "image.description": true, "image.os": true, "image.release": true, "image.serial": true, "image.type": true, "image.version": true}
	for key, value := range cfg {
		if strings.HasPrefix(key, "volatile.") || strings.HasPrefix(key, "image.") {
			continue
		}
		if !allowed[key] {
			return nil, fmt.Errorf("native configuration %q requires removal before import", key)
		}
		if (key == "security.privileged" || key == "security.nesting") && platform.Text(value) != "false" && platform.Text(value) != "" {
			return nil, errors.New("unsafe native security configuration")
		}
	}
	devices := object(m["expanded_devices"])
	if len(devices) == 0 {
		devices = object(m["devices"])
	}
	roots, nics := 0, 0
	var pci []string
	for _, raw := range devices {
		d := object(raw)
		switch platform.Text(d["type"]) {
		case "disk":
			roots++
			if roots != 1 || platform.Text(d["path"]) != "/" || platform.Text(d["pool"]) == "" || platform.Text(d["source"]) != "" {
				return nil, errors.New("native host mounts are forbidden")
			}
			for key := range d {
				if key != "type" && key != "path" && key != "pool" && key != "size" {
					return nil, errors.New("unapproved native root device option")
				}
			}
		case "nic":
			nics++
			for key := range d {
				switch key {
				case "type", "name", "nictype", "parent", "network", "mtu", "hwaddr", "ipv4.address", "ipv6.address":
				default:
					return nil, errors.New("unapproved native NIC option")
				}
			}
			if platform.Text(d["name"]) == "" {
				return nil, errors.New("native NIC name missing")
			}
		case "gpu":
			if spec.InstanceType != "container" || platform.Text(d["gputype"]) != "physical" || platform.Text(d["pci"]) == "" {
				return nil, errors.New("native GPU must be physical and explicitly addressed")
			}
			for key := range d {
				if key != "type" && key != "gputype" && key != "pci" {
					return nil, errors.New("unapproved native GPU option")
				}
			}
			pci = append(pci, strings.ToLower(platform.Text(d["pci"])))
		default:
			return nil, errors.New("native instance has an unsafe or unapproved device")
		}
	}
	if roots != 1 || nics != 1 {
		return nil, errors.New("native import requires one root disk and one approved NIC")
	}
	if len(pci) != spec.GPU.Count {
		return nil, errors.New("native GPU count differs from approval")
	}
	if len(pci) == 0 {
		return []string{}, nil
	}
	resources, err := a.Agent(ctx, node, http.MethodGet, "/agent/v1/resources", nil)
	if err != nil {
		return nil, platform.Unknown(err)
	}
	var uuids []string
	for _, address := range pci {
		found := ""
		for _, raw := range rows(resources["gpus"]) {
			g := object(raw)
			if strings.ToLower(platform.Text(g["pci_address"])) == address {
				if spec.GPU.Model != "" && platform.Text(g["model"]) != spec.GPU.Model {
					return nil, errors.New("native GPU model differs from approval")
				}
				found = platform.Text(g["uuid"])
				break
			}
		}
		if found == "" {
			return nil, errors.New("native GPU UUID cannot be resolved")
		}
		uuids = append(uuids, found)
	}
	sort.Strings(uuids)
	if len(workerUnique(append([]string{}, uuids...))) != len(uuids) {
		return nil, errors.New("duplicate native GPU")
	}
	if len(spec.GPU.FixedUUIDs) > 0 {
		expected := append([]string{}, spec.GPU.FixedUUIDs...)
		sort.Strings(expected)
		if strings.Join(expected, "\x00") != strings.Join(uuids, "\x00") {
			return nil, errors.New("native GPUs differ from explicit approval")
		}
	}
	return uuids, nil
}

func runReconcile(ctx context.Context, a *platform.App, j platform.Job) error {
	p, err := workerParams(j)
	if err != nil {
		return err
	}
	if sourceID := platform.Uint(p["source_task_id"]); sourceID != 0 {
		source, e := platform.Get(a.DB.WithContext(ctx), "tasks", sourceID, false)
		if e != nil {
			return e
		}
		if forceStoppedSource(source) {
			return reconcileForceStopped(ctx, a, j, source)
		}
	}
	if j.InstanceID != 0 {
		instance, e := platform.Get(a.DB.WithContext(ctx), "instances", j.InstanceID, false)
		if e != nil {
			return e
		}
		if instance["creation_failed_at"] != nil {
			return platform.E(409, "creation_retired", "失败创建已交由后台安全回收，不得重放原任务。")
		}
	}
	sourceID := platform.Uint(p["source_task_id"])
	seen := map[uint64]bool{j.ID: true}
	var sources []platform.Row
	var ids []uint64
	var source platform.Row
	for depth := range 32 {
		if sourceID == 0 || seen[sourceID] {
			return errors.New("invalid or cyclic reconciliation source")
		}
		seen[sourceID] = true
		source, err = platform.Get(a.DB.WithContext(ctx), "tasks", sourceID, false)
		if err != nil {
			return err
		}
		if platform.Text(source["status"]) != "unknown" || platform.Uint(source["instance_id"]) != j.InstanceID || platform.Uint(source["node_id"]) != j.NodeID || platform.Uint(source["user_id"]) != j.UserID {
			return errors.New("reconciliation source identity or status changed")
		}
		sources = append(sources, source)
		ids = append(ids, sourceID)
		if platform.Text(source["task_type"]) != "instance.reconcile" {
			break
		}
		sourceID = platform.Uint(object(source["parameters_redacted"])["source_task_id"])
		if depth == 31 {
			return errors.New("reconciliation chain is too deep")
		}
	}
	original := j
	original.Type = platform.Text(source["task_type"])
	original.Parameters = json.RawMessage(platform.Text(source["parameters_redacted"]))
	if err = workerClaimReconcile(ctx, a, j, source); err != nil {
		return platform.Unknown(err)
	}
	var journals []platform.Row
	if err = a.DB.WithContext(ctx).Table("tasks").Select("id,parameters_redacted").Where("task_type = 'instance.reconcile' AND user_id <=> ? AND node_id <=> ? AND instance_id <=> ?", source["user_id"], source["node_id"], source["instance_id"]).Find(&journals).Error; err != nil {
		return err
	}
	// Include earlier reconciliation attempts even when the administrator selected
	// the root task rather than the most recent unknown reconciliation wrapper.
	for range 32 {
		changed := false
		for _, journal := range journals {
			id := platform.Uint(journal["id"])
			parent := platform.Uint(object(journal["parameters_redacted"])["source_task_id"])
			if seen[parent] && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	ctx = context.WithValue(ctx, workerRecoveryKey{}, ids)
	switch original.Type {
	case "instance.create":
		err = runCreate(ctx, a, original)
	case "instance.start", "instance.stop", "instance.restart":
		err = runLifecycle(ctx, a, original)
	case "instance.resize":
		err = runResize(ctx, a, original)
	case "instance.delete":
		err = runDelete(ctx, a, original)
	case "instance.gpu":
		err = runGPU(ctx, a, original)
	case "instance.import":
		err = runImport(ctx, a, original)
	default:
		original.Parameters = json.RawMessage(jsonText(platform.Row{"source_task_id": sourceID}))
		err = a.ReconcileJob(ctx, original)
	}
	if err != nil {
		// Image preparation owns its dispatch uncertainty classification and marks
		// known native failures under the source/wrapper lease fence.
		if original.Type == "image.prepare" {
			return err
		}
		return platform.Unknown(err)
	}
	err = a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if e := workerLease(a, db, j); e != nil {
			return e
		}
		for _, s := range sources {
			result := db.Table("tasks").Where("id = ? AND status = 'unknown' AND lease_generation = ? AND revision = ?", s["id"], s["lease_generation"], s["revision"]).Updates(platform.Row{"status": "succeeded", "finished_at": a.Time(), "error_code": nil, "result_redacted": gorm.Expr("JSON_REMOVE(COALESCE(result_redacted, JSON_OBJECT()), '$.error')"), "phase": "reconciled", "lease_owner": nil, "lease_until": nil, "revision": gorm.Expr("revision + 1")})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("reconciliation source changed concurrently")
			}
		}
		return nil
	})
	if err != nil {
		return platform.Unknown(err)
	}
	return a.TaskResult(ctx, j, platform.Row{"source_task_id": strconv.FormatUint(sourceID, 10), "reconciled": true})
}

var workerSkipped = errors.New("operation superseded before dispatch")

func workerImportNetwork(ctx context.Context, a *platform.App, nodeID uint64, spec Spec, native platform.Row, project, address string) error {
	bindingID, err := platform.IDString(spec.NetworkOptionID)
	if err != nil {
		return err
	}
	binding, err := platform.Get(a.DB.WithContext(ctx), "node_network_bindings", bindingID, false)
	if err != nil {
		return err
	}
	if platform.Uint(binding["node_id"]) != nodeID || platform.Text(binding["project_name"]) != project || platform.Text(binding["status"]) != "active" || platform.Text(binding["validation_status"]) != "verified" {
		return errors.New("native project or network differs from approval")
	}
	if err = workerNoNAT(ctx, a, nodeID, project, binding); err != nil {
		return err
	}
	pool, err := platform.Get(a.DB.WithContext(ctx), "network_pools", platform.Uint(binding["network_pool_id"]), false)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return err
	}
	poolIP, err := workerIP(pool["network_address"])
	if err != nil {
		return err
	}
	prefix, err := netip.ParsePrefix(poolIP + "/" + platform.Text(pool["prefix_length"]))
	if err != nil || !prefix.Contains(ip) {
		return errors.New("native IP is outside the approved address pool")
	}
	imageID, err := platform.IDString(spec.ImageID)
	if err != nil {
		return err
	}
	image, err := platform.Get(a.DB.WithContext(ctx), "images", imageID, false)
	if err != nil {
		return err
	}
	image, err = catalog.ResolveNodeImage(ctx, a.DB.WithContext(ctx), image, nodeID, project)
	if err != nil {
		return err
	}
	cfg := object(native["expanded_config"])
	if len(cfg) == 0 {
		cfg = object(native["config"])
	}
	if workerFingerprint(cfg["volatile.base_image"]) != workerFingerprint(image["fingerprint"]) || platform.Text(native["architecture"]) != platform.Text(image["architecture"]) {
		return errors.New("native image fingerprint or architecture differs from approval")
	}
	n, err := platform.Get(a.DB.WithContext(ctx), "nodes", nodeID, false)
	if err != nil {
		return err
	}
	devices := object(native["expanded_devices"])
	if len(devices) == 0 {
		devices = object(native["devices"])
	}
	for _, raw := range devices {
		d := object(raw)
		if platform.Text(d["type"]) == "disk" && platform.Text(d["pool"]) != platform.Text(n["default_storage_pool"]) {
			return errors.New("native root storage pool differs from approval")
		}
		if platform.Text(d["type"]) != "nic" {
			continue
		}
		switch platform.Text(binding["mode"]) {
		case "managed_bridge":
			if platform.Text(d["network"]) != platform.Text(binding["incus_network_name"]) {
				return errors.New("native managed network differs from approval")
			}
		case "bridge", "macvlan":
			wantedNICType := "macvlan"
			if platform.Text(binding["mode"]) == "bridge" {
				wantedNICType = "bridged"
			}
			if platform.Text(d["nictype"]) != wantedNICType || platform.Text(d["parent"]) != platform.Text(binding["parent_interface"]) || platform.Text(d["network"]) != "" {
				return errors.New("native direct network differs from approval")
			}
		default:
			return errors.New("unsupported approved network mode")
		}
		if mtu := platform.Text(binding["mtu"]); mtu != "" && mtu != "0" && platform.Text(d["mtu"]) != mtu {
			return errors.New("native network MTU differs from approval")
		}
	}
	return nil
}

// StorageCapacity performs read-only admission/pre-dispatch validation against Incus.
func StorageCapacity(ctx context.Context, a *platform.App, node uint64, pool, project string, needed uint64) error {
	if needed == 0 {
		return nil
	}
	path := "/1.0/storage-pools/" + url.PathEscape(pool) + "/resources?project=" + url.QueryEscape(project)
	v, err := a.Agent(ctx, node, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	m, err := workerMetadata(v)
	if err != nil {
		return err
	}
	return storageCapacityError(object(m["space"]), pool, needed)
}

func storageCapacityError(space map[string]any, pool string, needed uint64) error {
	total, totalErr := strconv.ParseUint(platform.Text(space["total"]), 10, 64)
	used, usedErr := strconv.ParseUint(platform.Text(space["used"]), 10, 64)
	if totalErr != nil || usedErr != nil {
		return platform.EDetails(409, "storage_pool_capacity_unknown", "暂时无法确认节点存储池可用空间，未提交新的磁盘分配。", platform.Row{"scope": "storage_pool", "resource": "disk_bytes", "unit": "bytes", "pool": pool, "action": "请联系管理员检查节点存储池状态，确认容量后重新提交。", "stage": "storage.preflight"})
	}
	available := uint64(0)
	if total >= used {
		available = total - used
	}
	if total < used || available < needed {
		return platform.EDetails(409, "storage_pool_capacity_exceeded", "节点存储池可用空间不足，无法分配申请的磁盘。", platform.Row{"scope": "storage_pool", "resource": "disk_bytes", "unit": "bytes", "pool": pool, "used": strconv.FormatUint(used, 10), "requested": strconv.FormatUint(needed, 10), "limit": strconv.FormatUint(total, 10), "total": strconv.FormatUint(total, 10), "available": strconv.FormatUint(available, 10), "field": "resources.disk_bytes", "action": "请减少磁盘申请量、选择其他节点，或联系管理员扩容存储池；提高用户配额不能增加实际磁盘空间。", "stage": "create.preflight"})
	}
	return nil
}

func workerDiskCapacity(ctx context.Context, a *platform.App, r platform.Row, needed uint64) error {
	return StorageCapacity(ctx, a, platform.Uint(r["node_id"]), platform.Text(r["storage_pool"]), platform.Text(r["project_name"]), needed)
}

func workerIdleStop(ctx context.Context, a *platform.App, r platform.Row) error {
	cfg := object(object(r["gpu_policy"])["idle"])
	if !platform.Bool(cfg["enabled"]) || platform.Text(cfg["action"]) != "stop_instance" {
		return workerSkipped
	}
	policy, err := a.InstancePolicy(ctx, r, "gpu-idle")
	if err != nil {
		return err
	}
	if !platform.Bool(policy["enabled"]) {
		return workerSkipped
	}
	timeout := platform.Uint(cfg["timeout_seconds"])
	since := platform.Stamp(r["idle_since"])
	if timeout == 0 || since.IsZero() || a.Time().Sub(since) < time.Duration(timeout)*time.Second {
		return workerSkipped
	}
	var allocations []platform.Row
	if err = a.DB.WithContext(ctx).Table("gpu_allocations ga").Select("ga.*,g.uuid").Joins("JOIN node_gpus g ON g.id = ga.gpu_id").Where("ga.instance_id = ? AND ga.active_slot = 1", r["id"]).Find(&allocations).Error; err != nil {
		return err
	}
	if len(allocations) == 0 {
		return workerSkipped
	}
	if err = storedIdle(ctx, a, a.DB, r, allocations); err != nil {
		return err
	}
	live, err := liveGPU(ctx, a, r, false)
	if err != nil {
		return err
	}
	for key, card := range live {
		if key == "_frame" {
			continue
		}
		if err = safeLiveCard(card, false); err != nil {
			return err
		}
	}
	for _, allocation := range allocations {
		if err = safeLiveCard(live[platform.Text(allocation["uuid"])], false); err != nil {
			return err
		}
	}
	return nil
}

func workerByteSize(value string) (uint64, error) {
	multiplier := uint64(1)
	switch {
	case strings.HasSuffix(value, "KiB"):
		multiplier = 1 << 10
		value = strings.TrimSuffix(value, "KiB")
	case strings.HasSuffix(value, "MiB"):
		multiplier = 1 << 20
		value = strings.TrimSuffix(value, "MiB")
	case strings.HasSuffix(value, "GiB"):
		multiplier = 1 << 30
		value = strings.TrimSuffix(value, "GiB")
	case strings.HasSuffix(value, "TiB"):
		multiplier = 1 << 40
		value = strings.TrimSuffix(value, "TiB")
	case strings.HasSuffix(value, "PiB"):
		multiplier = 1 << 50
		value = strings.TrimSuffix(value, "PiB")
	case strings.HasSuffix(value, "EiB"):
		multiplier = 1 << 60
		value = strings.TrimSuffix(value, "EiB")
	case strings.HasSuffix(value, "kB"):
		multiplier = 1000
		value = strings.TrimSuffix(value, "kB")
	case strings.HasSuffix(value, "MB"):
		multiplier = 1000000
		value = strings.TrimSuffix(value, "MB")
	case strings.HasSuffix(value, "GB"):
		multiplier = 1000000000
		value = strings.TrimSuffix(value, "GB")
	case strings.HasSuffix(value, "TB"):
		multiplier = 1000000000000
		value = strings.TrimSuffix(value, "TB")
	case strings.HasSuffix(value, "B"):
		value = strings.TrimSuffix(value, "B")
	}
	number, err := strconv.ParseUint(value, 10, 64)
	if err != nil || number == 0 || number > ^uint64(0)/multiplier {
		return 0, errors.New("native resource quantity is not an exact positive byte size")
	}
	return number * multiplier, nil
}

func workerLateStop(ctx context.Context, a *platform.App, j platform.Job, r platform.Row) error {
	err := a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if e := lockWorkerAccount(db, a, j, j.UserID); e != nil {
			return e
		}
		current, e := platform.Get(db, "instances", j.InstanceID, true)
		if e != nil {
			return e
		}
		if platform.Text(current["desired_state"]) == "running" {
			return nil
		}
		at := a.Time()
		root, kind, e := workerBillingIdentity(ctx, db, j)
		if e != nil {
			return e
		}
		var finish func() error
		if current["creation_failed_at"] == nil && (kind == "instance.create" || kind == "instance.start") {
			key := "task:" + strconv.FormatUint(root, 10) + ":" + kind
			finish, e = billing.PrepareInstanceEvent(db, current, kind, key, at, kind != "instance.create")
			if e != nil {
				return e
			}
		}
		if e = db.Table("instances").Where("id = ?", j.InstanceID).Updates(platform.Row{"observed_state": "running", "observed_at": at, "revision": gorm.Expr("revision + 1")}).Error; e != nil {
			return e
		}
		current["observed_state"] = "running"
		if e = billing.InstanceUsageAt(ctx, db, current, "running", nil, at); e != nil {
			return e
		}
		if finish != nil {
			if e = finish(); e != nil {
				return e
			}
		}
		var stops []platform.Row
		if e = db.Table("tasks").Where("instance_id = ? AND task_type = 'instance.stop'", j.InstanceID).Order("id DESC").Find(&stops).Error; e != nil {
			return e
		}
		for _, stop := range stops {
			p := object(stop["parameters_redacted"])
			reason := platform.Text(p["reason"])
			if !strings.HasPrefix(reason, "credit-enforcement:") {
				continue
			}
			_, e = QueueStop(ctx, a, db, j.InstanceID, j.UserID, platform.Bool(p["force"]), reason)
			return e
		}
		return errors.New("late running state requires an authorized control stop")
	})
	if err != nil {
		return err
	}
	return errors.New("late running state recorded and guarded control stop queued")
}

func workerCreatedDevices(native, requested platform.Row) error {
	cfg := object(native["expanded_config"])
	if len(cfg) == 0 {
		cfg = object(native["config"])
	}
	expectedConfig := object(requested["config"])
	for key, value := range expectedConfig {
		if key == "limits.memory" {
			continue
		}
		if platform.Text(cfg[key]) != platform.Text(value) {
			return fmt.Errorf("created configuration %q differs from approval", key)
		}
	}
	for key := range cfg {
		if strings.HasPrefix(key, "volatile.") || strings.HasPrefix(key, "image.") {
			continue
		}
		if _, ok := expectedConfig[key]; !ok {
			return fmt.Errorf("unapproved created configuration %q", key)
		}
	}
	devices := object(native["expanded_devices"])
	if len(devices) == 0 {
		devices = object(native["devices"])
	}
	expected := object(requested["devices"])
	for name, raw := range expected {
		want := object(raw)
		actual := object(devices[name])
		if len(actual) == 0 {
			return errors.New("created approved device is missing")
		}
		for key, value := range want {
			if key == "size" {
				continue
			}
			if platform.Text(actual[key]) != platform.Text(value) {
				return errors.New("created root or network device differs from approval")
			}
		}
		for key := range actual {
			if key == "hwaddr" || key == "host_name" {
				continue
			}
			if _, ok := want[key]; !ok {
				return errors.New("unapproved option on created device")
			}
		}
	}
	for name, raw := range devices {
		if _, ok := expected[name]; ok {
			continue
		}
		if platform.Text(object(raw)["type"]) != "gpu" {
			return errors.New("unapproved device on created instance")
		}
	}
	return nil
}

func workerClaimReconcile(ctx context.Context, a *platform.App, j platform.Job, source platform.Row) error {
	return a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		root, err := platform.Get(db, "tasks", platform.Uint(source["id"]), true)
		if err != nil {
			return err
		}
		if platform.Text(root["status"]) != "unknown" || platform.Uint(root["revision"]) != platform.Uint(source["revision"]) {
			return errors.New("reconciliation source changed before claim")
		}
		owner := platform.Text(root["lease_owner"])
		if strings.HasPrefix(owner, "reconcile:") {
			prior, err := strconv.ParseUint(strings.TrimPrefix(owner, "reconcile:"), 10, 64)
			if err != nil {
				return err
			}
			if prior != j.ID {
				var live int64
				if err = db.Table("tasks").Where("id = ? AND status = 'running' AND lease_until > ?", prior, a.Time()).Count(&live).Error; err != nil {
					return err
				}
				if live != 0 {
					return errors.New("another live reconciliation owns this source")
				}
			}
		}
		current, err := platform.Get(db, "tasks", j.ID, false)
		if err != nil {
			return err
		}
		if platform.Text(current["status"]) != "running" || platform.Uint(current["lease_generation"]) != j.LeaseGeneration || !platform.Stamp(current["lease_until"]).After(a.Time()) {
			return errors.New("lost reconciliation lease")
		}
		if err = db.Table("tasks").Where("id = ?", source["id"]).Updates(platform.Row{"lease_owner": "reconcile:" + strconv.FormatUint(j.ID, 10), "lease_until": current["lease_until"], "revision": gorm.Expr("revision + 1")}).Error; err != nil {
			return err
		}
		source["revision"] = platform.Uint(source["revision"]) + 1
		return nil
	})
}
func workerLinkImport(a *platform.App, db *gorm.DB, j platform.Job, rootID, instanceID uint64) error {
	if e := workerLease(a, db, j); e != nil {
		return e
	}
	ids := []uint64{j.ID}
	if rootID != j.ID {
		ids = append(ids, rootID)
	}
	return db.Table("tasks").Where("id IN ? AND user_id=? AND node_id=? AND status IN ? AND (instance_id IS NULL OR instance_id=?)", ids, j.UserID, j.NodeID, []string{"running", "unknown"}, instanceID).Update("instance_id", instanceID).Error
}
func lockWorkerAccount(db *gorm.DB, a *platform.App, j platform.Job, owner uint64) error {
	if e := lockOwnerAccount(db, owner); e != nil {
		return e
	}
	return workerLease(a, db, j)
}
func workerLease(a *platform.App, db *gorm.DB, j platform.Job) error {
	if err := db.Statement.Context.Err(); err != nil {
		return err
	}
	if db.Statement.Context.Value(forceStopObservationKey{}) != nil {
		return errors.New("read-only force-stop observation cannot authorize resource state writes")
	}
	task, e := platform.Get(db, "tasks", j.ID, true)
	if e != nil {
		return e
	}
	if platform.Uint(task["lease_generation"]) != j.LeaseGeneration || platform.Text(task["status"]) != "running" || !platform.Stamp(task["lease_until"]).After(a.Time()) || platform.Uint(task["user_id"]) != j.UserID || platform.Uint(task["node_id"]) != j.NodeID {
		return errors.New("worker lease no longer authorizes resource state writes")
	}
	if !platform.TaskCanResolveMutationBarrier(task) {
		return platform.TaskMutationBarrier(db, platform.Uint(task["instance_id"]))
	}
	return nil
}
