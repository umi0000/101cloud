package instances

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

// The HTTP transaction already holds the owner account and instance locks.
// Dispatch locks through durable intent and actual HTTP submission, not response
// waiting. Unidentified intent blocks cleanup; late operation IDs remain journaled.
func admitStuckDelete(a *platform.App, db *gorm.DB, r platform.Row) (platform.Row, error) {
	var active []platform.Row
	if err := locked(db).Table("tasks").Where("instance_id=? AND (status IN ? OR (is_instance_mutation=1 AND status='cancelled' AND error_code='task_force_stopped'))", r["id"], []string{"queued", "running", "waiting", "unknown"}).Order("id").Find(&active).Error; err != nil {
		return nil, err
	}
	for _, task := range active {
		if platform.Text(task["status"]) != "cancelled" && platform.Text(task["task_type"]) == "instance.delete" && platform.Bool(object(task["parameters_redacted"])["cleanup_stuck"]) {
			return platform.Row{"task_id": platform.Text(task["id"]), "instance_id": platform.Text(r["id"]), "status": task["status"]}, nil
		}
	}
	now := a.Time()
	for _, task := range active {
		if platform.Text(task["status"]) == "cancelled" && platform.Text(task["error_code"]) == "task_force_stopped" {
			// Transfer the mutation slot, not the stopped orchestration. The new
			// cleanup retains this source's native journals and must prove quiescence.
			if err := db.Table("tasks").Where("id=?", task["id"]).Updates(platform.Row{"is_instance_mutation": false, "revision": gorm.Expr("revision+1")}).Error; err != nil {
				return nil, err
			}
			continue
		}
		if err := db.Table("tasks").Where("id=?", task["id"]).Updates(platform.Row{"status": "cancelled", "error_code": "superseded_by_cleanup", "finished_at": now, "lease_owner": nil, "lease_until": nil, "lease_generation": gorm.Expr("lease_generation+1"), "revision": gorm.Expr("revision+1")}).Error; err != nil {
			return nil, err
		}
		// Preserve native operation IDs and all terminal step evidence.
		if err := db.Table("task_steps").Where("task_id=? AND status IN ?", task["id"], []string{"queued", "running", "waiting", "unknown"}).Updates(platform.Row{"status": "cancelled", "error_code": "superseded_by_cleanup", "finished_at": now}).Error; err != nil {
			return nil, err
		}
	}
	if err := db.Table("instances").Where("id=? AND deleted_at IS NULL", r["id"]).Updates(platform.Row{"desired_state": "deleted", "revision": gorm.Expr("revision+1")}).Error; err != nil {
		return nil, err
	}
	return platform.Enqueue(db, "instance.delete", platform.Uint(r["user_id"]), platform.Uint(r["node_id"]), platform.Uint(r["id"]), true, platform.Row{"cleanup_stuck": true, "force": true, "delete_data": true})
}

func nativeMissing(err error) bool {
	var e *platform.HTTPError
	return errors.As(err, &e) && e.Code == "native_not_found"
}

// Resolve asynchronous mutations before checking absence: a project listing
// alone cannot prove that a previously accepted CREATE will not appear later.
func stuckDeleteQuiescent(ctx context.Context, a *platform.App, j platform.Job, r platform.Row) error {
	var steps []platform.Row
	if err := a.DB.WithContext(ctx).Table("task_steps s").Select("s.*").Joins("JOIN tasks t ON t.id=s.task_id").Where("t.instance_id=? AND t.id<>?", j.InstanceID, j.ID).Find(&steps).Error; err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, step := range steps {
		op := platform.Text(step["operation_id"])
		if op == "" {
			key, status := platform.Text(step["step_key"]), platform.Text(step["status"])
			mutation := key == "create.native" || key == "delete.native" || key == cloudInitPrepareStep || strings.HasPrefix(key, "state.") || key == "resize.native" || key == "gpu.configure" || initializationStage(key)
			terminal := status == "succeeded" || status == "failed" || (status == "cancelled" && platform.Text(step["error_code"]) != "superseded_by_cleanup" && platform.Text(step["error_code"]) != "task_force_stopped")
			if mutation && !terminal && !platform.Bool(object(step["result_redacted"])["native_dispatch_fenced"]) {
				return unresolvedStuckSubmission()
			}
			if platform.Bool(object(step["result_redacted"])["native_dispatch_started"]) && platform.Text(step["status"]) != "succeeded" {
				return unresolvedStuckSubmission()
			}
			continue // An unsent/revoked request cannot outlive the dispatch fence.
		}
		if !strings.HasPrefix(op, "/1.0/operations/") {
			op = "/1.0/operations/" + url.PathEscape(op)
		}
		ref, err := url.Parse(op)
		if err != nil || ref.IsAbs() || !strings.HasPrefix(ref.Path, "/1.0/operations/") || strings.Contains(strings.TrimPrefix(ref.Path, "/1.0/operations/"), "/") {
			return errors.New("invalid prior native operation identity")
		}
		q := ref.Query()
		if project := q.Get("project"); project != "" && project != platform.Text(r["project_name"]) {
			return errors.New("prior native operation project conflicts with managed instance")
		}
		q.Set("project", platform.Text(r["project_name"]))
		ref.RawQuery = q.Encode()
		op = ref.String()
		if seen[op] {
			continue
		}
		seen[op] = true
		if err := settleStuckOperation(ctx, a, j, op); err != nil {
			return err
		}
	}
	// Incus groups recursive operation metadata by state. Inventory is scoped to
	// the frozen project. Unknown/incomplete active resources fail closed.
	v, err := a.Agent(ctx, j.NodeID, http.MethodGet, "/1.0/operations?recursion=1&project="+url.QueryEscape(platform.Text(r["project_name"])), nil)
	if err != nil {
		return err
	}
	groups, err := workerMetadata(v)
	if err != nil {
		return err
	}
	for _, group := range groups {
		operations, ok := group.([]any)
		if !ok {
			return errors.New("native operation inventory incomplete")
		}
		for _, item := range operations {
			op := object(item)
			code := platform.Uint(op["status_code"])
			if code >= 200 {
				continue
			}
			if code == 0 {
				return errors.New("native operation inventory omitted state")
			}
			resources := object(op["resources"])
			if len(resources) == 0 {
				return errors.New("pending native operation has unverified resources")
			}
			for kind, entries := range resources {
				if kind != "instances" && kind != "containers" {
					continue
				}
				items, ok := entries.([]any)
				if !ok {
					return errors.New("native operation resource list incomplete")
				}
				for _, entry := range items {
					target, err := url.Parse(platform.Text(entry))
					if err != nil || target.Path == "" {
						return errors.New("native operation target incomplete")
					}
					if project := target.Query().Get("project"); project != "" && project != platform.Text(r["project_name"]) {
						continue
					}
					if target.Path == "/1.0/instances/"+url.PathEscape(platform.Text(r["incus_name"])) || target.Path == "/1.0/containers/"+url.PathEscape(platform.Text(r["incus_name"])) {
						id := platform.Text(op["id"])
						if id == "" {
							return errors.New("pending native mutation omitted identity")
						}
						if err := settleStuckOperation(ctx, a, j, "/1.0/operations/"+url.PathEscape(id)+"?project="+url.QueryEscape(platform.Text(r["project_name"]))); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

func settleStuckOperation(ctx context.Context, a *platform.App, j platform.Job, op string) error {
	v, err := a.Agent(ctx, j.NodeID, http.MethodGet, op, nil)
	if nativeMissing(err) {
		return nil // Authoritative operation expiry; project inventory still required.
	}
	if err != nil {
		return err
	}
	m, err := workerMetadata(v)
	if err != nil {
		return err
	}
	if platform.Uint(m["status_code"]) >= 200 {
		return nil
	}
	if platform.Uint(m["status_code"]) == 0 {
		return errors.New("native operation omitted status")
	}
	if ctx.Value(forceStopObservationKey{}) != nil {
		return forceStopPending("native_operation_running")
	}
	ref, err := url.Parse(op)
	if err != nil {
		return err
	}
	key := "delete.quiesce." + strings.TrimPrefix(ref.Path, "/1.0/operations/")
	if err := a.Step(ctx, j, key, op, "running"); err != nil {
		return err
	}
	if platform.Bool(m["may_cancel"]) {
		// Cancellation acknowledgement is not terminal evidence. Even a rejected
		// cancellation may race completion, so always observe the operation again.
		_, _ = a.Agent(platform.WithTaskDispatch(ctx, j, key), j.NodeID, http.MethodDelete, op, nil)
	}
	_, _ = a.WaitOperation(ctx, j.NodeID, op)
	v, err = a.Agent(ctx, j.NodeID, http.MethodGet, op, nil)
	if err != nil {
		return err
	}
	m, err = workerMetadata(v)
	if err != nil {
		return err
	}
	if platform.Uint(m["status_code"]) < 200 {
		return errors.New("prior native mutation remains pending")
	}
	return a.Step(ctx, j, key, op, "succeeded")
}

func stuckDeleteIdentity(r, native platform.Row) error {
	cfg := object(native["expanded_config"])
	if len(cfg) == 0 {
		cfg = object(native["config"])
	}
	if platform.Text(r["incus_uuid"]) == "" && platform.Text(cfg["user.101cloud.instance_id"]) != strconv.FormatUint(platform.Uint(r["id"]), 10) {
		return errors.New("cleanup ownership marker mismatch")
	}
	if marker := platform.Text(cfg["user.101cloud.instance_id"]); marker != "" && marker != platform.Text(r["id"]) {
		return errors.New("cleanup ownership marker conflicts with managed instance")
	}
	return nil
}

func unresolvedStuckSubmission() error {
	return platform.EDetails(409, "cleanup_native_submission_unknown", "原任务的节点请求可能已提交，但缺少可验证的操作编号。已撤销旧任务，尚未释放资源；请管理员核实节点操作日志并恢复操作编号后，对此清理任务执行协调。", platform.Row{"reason": "missing_native_operation_identity", "action": "核实 Incus 原生操作日志并恢复 task_steps.operation_id，再协调当前清理任务；不要重放原创建或启动任务。"})
}

func workerDeleteAbsent(ctx context.Context, a *platform.App, r platform.Row, cleanup bool) (bool, error) {
	absent, err := workerAbsent(ctx, a, r)
	if err == nil || !cleanup {
		return absent, err
	}
	// Only a target GET whose HTTP status AND Incus error envelope agree on
	// 404 is absence. Offline/forbidden/malformed responses never qualify.
	_, targetErr := a.Agent(ctx, platform.Uint(r["node_id"]), http.MethodGet, workerPath(r, ""), nil)
	if nativeMissing(targetErr) {
		return true, nil
	}
	return false, err
}
