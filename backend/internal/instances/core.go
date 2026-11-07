package instances

import (
	"context"
	"encoding/json"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Resources struct {
	VCPUs       uint64 `json:"vcpus"`
	MemoryBytes string `json:"memory_bytes"`
	DiskBytes   string `json:"disk_bytes"`
}
type GPURequest struct {
	Count         int      `json:"count"`
	Model         string   `json:"model"`
	AccessMode    string   `json:"access_mode"`
	MaxHourlyPT   string   `json:"max_hourly_pt"`
	MinMemoryMiB  string   `json:"-"`
	AllowLive     bool     `json:"allow_live"`
	FixedUUIDs    []string `json:"fixed_uuids,omitempty"`
	AllocationIDs []string `json:"allocation_ids,omitempty"`
	// Reason is an internal scheduler control marker, never HTTP input or configuration.
	Reason string `json:"-"`
}
type Spec struct {
	Name                  string          `json:"name"`
	InstanceType          string          `json:"instance_type,omitempty"`
	PlanID                string          `json:"plan_id,omitempty"`
	PlanRevision          string          `json:"plan_revision,omitempty"`
	ImageID               string          `json:"image_id"`
	NetworkOptionID       string          `json:"network_option_id"`
	NodeID                string          `json:"node_id,omitempty"`
	Resources             Resources       `json:"resources"`
	GPU                   GPURequest      `json:"gpu"`
	InitParameters        map[string]any  `json:"init_parameters"`
	InitTemplateVersionID string          `json:"init_template_version_id,omitempty"`
	AllowedUserActions    []string        `json:"allowed_user_actions,omitempty"`
	GPUUserPermissions    map[string]bool `json:"gpu_user_permissions,omitempty"`
}

func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }
func object(v any) map[string]any {
	if r, ok := v.(map[string]any); ok {
		return r
	}
	if values, ok := v.(map[string]string); ok {
		r := make(map[string]any, len(values))
		for key, value := range values {
			r[key] = value
		}
		return r
	}
	var r map[string]any
	switch x := v.(type) {
	case []byte:
		_ = json.Unmarshal(x, &r)
	case string:
		_ = json.Unmarshal([]byte(x), &r)
	}
	if r == nil {
		return map[string]any{}
	}
	return r
}
func rows(v any) []any {
	if r, ok := v.([]any); ok {
		return r
	}
	if s, ok := v.([]string); ok {
		r := make([]any, len(s))
		for i, v := range s {
			r[i] = v
		}
		return r
	}
	var r []any
	switch x := v.(type) {
	case []byte:
		_ = json.Unmarshal(x, &r)
	case string:
		_ = json.Unmarshal([]byte(x), &r)
	}
	return r
}
func queryRow(db *gorm.DB, table string, where string, args ...any) (platform.Row, error) {
	var r platform.Row
	e := db.Table(table).Where(where, args...).Take(&r).Error
	return r, platform.WrapDB(e)
}
func locked(db *gorm.DB) *gorm.DB         { return db.Clauses(clause.Locking{Strength: "UPDATE"}) }
func bad(message string) error            { return platform.E(422, "invalid_request", message) }
func conflict(code, message string) error { return platform.E(409, code, message) }

// instanceImageQuery deliberately bypasses catalog visibility: existing instances
// retain their source OS metadata when an image is disabled or retired.
func instanceImageQuery(db *gorm.DB) *gorm.DB {
	return db.Table("instances").Joins("LEFT JOIN (SELECT id AS source_image_id, os_name AS image_os_name, os_release AS image_os_release FROM images) AS source_image ON source_image.source_image_id=instances.image_id")
}

func owned(c *gin.Context, a *platform.App) (platform.Row, error) {
	return ownedQuery(c, a, platform.DB(c, a).Table("instances"))
}

func ownedQuery(c *gin.Context, a *platform.App, q *gorm.DB) (platform.Row, error) {
	id, e := platform.ID(c, "id")
	if e != nil {
		return nil, e
	}
	db := platform.DB(c, a)
	var r platform.Row
	e = platform.WrapDB(q.Where("instances.id=?", id).Take(&r).Error)
	if e != nil {
		return nil, e
	}
	if r["creation_failed_at"] != nil && (!c.GetBool("admin") || (c.Request.Method != "GET" && !c.GetBool("allow_stuck_cleanup"))) {
		return nil, platform.E(404, "not_found", "Instance not found")
	}
	if !c.GetBool("admin") && platform.Uint(r["user_id"]) != platform.Current(c).ID {
		return nil, platform.E(404, "not_found", "Instance not found")
	}
	if c.Request.Method != "GET" {
		if e = lockOwnerAccount(db, platform.Uint(r["user_id"])); e != nil {
			return nil, e
		}
		current, err := platform.Get(db, "instances", id, true)
		if err != nil {
			return nil, err
		}
		if current["creation_failed_at"] != nil && !c.GetBool("allow_stuck_cleanup") {
			return nil, platform.E(404, "not_found", "Instance not found")
		}
		if !c.GetBool("allow_stuck_cleanup") {
			if err = platform.TaskMutationBarrier(db, id); err != nil {
				return nil, err
			}
		}
		return current, nil
	}
	return r, nil
}
func permitted(c *gin.Context, r platform.Row, action string) error {
	if c.GetBool("admin") {
		return nil
	}
	for _, v := range rows(r["allowed_user_actions"]) {
		if platform.Text(v) == action {
			return nil
		}
	}
	return platform.E(403, "action_not_allowed", "Administrator has not authorized this instance action")
}
func instanceDTO(a *platform.App, r platform.Row) platform.Row {
	out := platform.Public(r, "id", "product_type", "billing_mode", "desired_state", "observed_state", "provisioning_state", "observed_at", "revision", "created_at", "updated_at", "node_id", "user_id")
	out["policy_versions"] = object(r["policy_versions"])
	if r["creation_failed_at"] != nil {
		out["creation_failed_at"] = r["creation_failed_at"]
		out["creation_failure_task_id"] = platform.Text(r["creation_failure_task_id"])
		out["creation_cleanup_state"] = platform.Text(r["creation_cleanup_state"])
	}
	out["resource_reservations_released"] = platform.Text(r["provisioning_state"]) == "failed" && platform.Bool(object(r["incus_config_snapshot"])["creation_reservations_released"])
	out["name"] = platform.Text(r["logical_name"])
	out["instance_type"] = platform.Text(r["image_type"])
	out["plan_id"] = nil
	if r["instance_plan_id"] != nil {
		out["plan_id"] = strconv.FormatUint(platform.Uint(r["instance_plan_id"]), 10)
	}
	out["plan_snapshot"] = nil
	if platform.Text(r["product_type"]) == "elastic" {
		if snapshot := object(r["plan_snapshot"]); len(snapshot) > 0 {
			out["plan_snapshot"] = snapshot
		}
	}
	out["image_id"] = strconv.FormatUint(platform.Uint(r["image_id"]), 10)
	out["image_os_name"] = platform.Text(r["image_os_name"])
	out["image_os_release"] = platform.Text(r["image_os_release"])
	out["resources"] = Resources{platform.Uint(r["vcpus"]), strconv.FormatUint(platform.Uint(r["memory_bytes"]), 10), strconv.FormatUint(platform.Uint(r["disk_bytes"]), 10)}
	stale := platform.Stamp(r["observed_at"]).IsZero() || a.Time().Sub(platform.Stamp(r["observed_at"])) > time.Minute
	out["state_stale"] = stale
	caps := platform.Row{"start": !stale && platform.Text(r["observed_state"]) == "stopped", "stop": !stale && platform.Text(r["observed_state"]) == "running", "gpu_attach": !stale && platform.Text(r["image_type"]) == "container", "gpu_rebind": !stale && platform.Text(r["image_type"]) == "container", "console": !stale && platform.Text(r["observed_state"]) == "running"}
	out["capabilities"] = caps
	return out
}

func Register(a *platform.App, user, admin *gin.RouterGroup) {
	billing.SetStopHook(QueueStop)
	registerWorkers(a)
	registerProvisioning(a, user, admin)
	registerGPU(a, user, admin)
	for _, g := range []*gin.RouterGroup{user, admin} {
		g.GET("/instances", platform.Handle(func(c *gin.Context) (any, error) {
			q := instanceImageQuery(platform.DB(c, a)).Select("id,user_id,node_id,logical_name,product_type,billing_mode,image_type,image_id,instance_plan_id,plan_snapshot,policy_versions,vcpus,memory_bytes,disk_bytes,desired_state,observed_state,provisioning_state,observed_at,allowed_user_actions,incus_config_snapshot,revision,created_at,updated_at,image_os_name,image_os_release").Where("deleted_at IS NULL AND creation_failed_at IS NULL")
			if !c.GetBool("admin") {
				q = q.Where("user_id=?", platform.Current(c).ID)
			}
			for _, f := range []string{"product_type", "observed_state", "node_id"} {
				if v := c.Query(f); v != "" {
					q = q.Where(f+"=?", v)
				}
			}
			if v := c.Query("owner_user_id"); v != "" && c.GetBool("admin") {
				q = q.Where("user_id=?", v)
			}
			return platform.ListMap(c, a, q, []string{"product_type", "observed_state", "owner_user_id", "node_id"}, func(r platform.Row) (platform.Row, error) { return instanceView(c, a, r) })
		}))
		g.GET("/instances/:id", platform.Handle(func(c *gin.Context) (any, error) {
			r, e := ownedQuery(c, a, instanceImageQuery(platform.DB(c, a)).Select("instances.*,image_os_name,image_os_release"))
			if e != nil {
				return nil, e
			}
			platform.SetETag(c, r)
			return instanceView(c, a, r)
		}))
		g.GET("/instances/:id/resize-limits", platform.Handle(func(c *gin.Context) (any, error) {
			return resizeLimits(c, a)
		}))
		g.PATCH("/instances/:id", platform.Handle(func(c *gin.Context) (any, error) {
			r, e := owned(c, a)
			if e != nil {
				return nil, e
			}
			var in struct {
				Name string `json:"name"`
			}
			if e = platform.Decode(c, &in); e != nil {
				return nil, e
			}
			if strings.TrimSpace(in.Name) == "" || len([]rune(in.Name)) > 128 {
				return nil, bad("Name must be 1–128 characters")
			}
			r, e = platform.Save(c, a, "instances", r, platform.Row{"logical_name": in.Name})
			if e != nil {
				return nil, e
			}
			return instanceView(c, a, r)
		}))
		for _, action := range []string{"start", "stop", "restart", "resize", "delete"} {
			action := action
			handler := platform.Handle(func(c *gin.Context) (any, error) { return lifecycle(c, a, action) })
			if action == "delete" {
				g.DELETE("/instances/:id", handler)
			} else {
				g.POST("/instances/:id/"+action, handler)
			}
		}
		g.GET("/instances/:id/network", platform.Handle(func(c *gin.Context) (any, error) {
			r, e := owned(c, a)
			if e != nil {
				return nil, e
			}
			var ips []platform.Row
			if e = platform.DB(c, a).Table("ip_addresses ip").Select("ip.address,p.prefix_length,p.gateway,p.dns_servers,b.mode,ip.updated_at AS observed_at,ip.status").Joins("JOIN network_pools p ON p.id=ip.network_pool_id").Joins("JOIN node_network_bindings b ON b.id=ip.node_network_binding_id").Where("ip.instance_id=?", r["id"]).Find(&ips).Error; e != nil {
				return nil, e
			}
			stale := platform.Stamp(r["observed_at"]).IsZero() || a.Time().Sub(platform.Stamp(r["observed_at"])) > time.Minute
			out := make([]platform.Row, 0, len(ips))
			for _, ip := range ips {
				entry := platform.Public(ip, "prefix_length", "mode", "observed_at", "status")
				entry["ip"] = packedIP(ip["address"])
				entry["gateway"] = packedIP(ip["gateway"])
				entry["dns"] = rows(ip["dns_servers"])
				entry["quality"] = platform.Row{"configuration_bound": platform.Text(ip["status"]) == "bound", "state_stale": stale}
				out = append(out, entry)
			}
			return out, nil
		}))
		g.GET("/tasks", platform.Handle(func(c *gin.Context) (any, error) {
			db := platform.DB(c, a)
			q := db.Table("tasks")
			if !c.GetBool("admin") {
				q = q.Where("user_id=?", platform.Current(c).ID)
			}
			for _, f := range []string{"instance_id", "status", "user_id", "node_id"} {
				if v := c.Query(f); v != "" {
					if !c.GetBool("admin") && (f == "user_id" || f == "node_id") {
						return nil, bad("Unsupported task filter")
					}
					q = q.Where(f+"=?", v)
				}
			}
			return platform.ListMap(c, a, q, []string{"instance_id", "status", "user_id", "node_id"}, func(r platform.Row) (platform.Row, error) { return taskRequestDTO(c, db, r) })
		}))
		g.GET("/tasks/:id", platform.Handle(func(c *gin.Context) (any, error) {
			id, e := platform.ID(c, "id")
			if e != nil {
				return nil, e
			}
			r, e := platform.Owned(c, a, "tasks", id)
			if e != nil {
				return nil, e
			}
			platform.SetETag(c, r)
			return taskRequestDTO(c, platform.DB(c, a), r)
		}))
		g.POST("/tasks/:id/force-stop", platform.Handle(func(c *gin.Context) (any, error) { return forceStopTask(c, a) }))
	}
	user.POST("/instances", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			Spec
			PolicyVersions map[string]string `json:"policy_versions"`
			Login          *LoginRequest     `json:"login,omitempty"`
		}
		if e := platform.Decode(c, &in); e != nil {
			return nil, e
		}
		if len(in.GPU.FixedUUIDs) > 0 || len(in.AllowedUserActions) > 0 || len(in.GPUUserPermissions) > 0 {
			return nil, bad("Administrator-only configuration")
		}
		login, e := normalizeLogin(in.Login)
		if e != nil {
			return nil, e
		}
		r, e := admitCreate(c.Request.Context(), a, platform.DB(c, a), platform.Current(c).ID, in.Spec, false, in.PolicyVersions, nil, login)
		if e != nil {
			return nil, e
		}
		taskAccepted(c, r)
		return nil, nil
	}))
	admin.POST("/instances/:id/delete", platform.Handle(func(c *gin.Context) (any, error) {
		return lifecycle(c, a, "delete")
	}))
	admin.POST("/tasks/:id/reconcile", platform.Handle(func(c *gin.Context) (any, error) {
		id, e := platform.ID(c, "id")
		if e != nil {
			return nil, e
		}
		r, e := platform.Get(platform.DB(c, a), "tasks", id, true)
		if e != nil {
			return nil, e
		}
		if e = platform.Revision(c, r); e != nil {
			return nil, e
		}
		if e = platform.Decode(c, &struct{}{}); e != nil {
			return nil, e
		}
		stopped := platform.Text(r["status"]) == "cancelled" && platform.Text(r["error_code"]) == "task_force_stopped" && platform.Bool(object(object(r["result_redacted"])["force_stop"])["requires_reconciliation"])
		if platform.Text(r["status"]) != "unknown" && !stopped {
			return nil, conflict("task_not_unknown", "Only unknown or force-stopped tasks with retained evidence require reconciliation")
		}
		out, e := platform.Enqueue(platform.DB(c, a), "instance.reconcile", platform.Uint(r["user_id"]), platform.Uint(r["node_id"]), platform.Uint(r["instance_id"]), false, platform.Row{"source_task_id": id, "read_only_force_stop_reconcile": stopped})
		if e != nil {
			return nil, e
		}
		taskAccepted(c, out)
		return nil, nil
	}))
}
func packedIP(v any) any {
	var b []byte
	switch x := v.(type) {
	case []byte:
		b = x
	case string:
		b = []byte(x)
	}
	if ip, ok := netip.AddrFromSlice(b); ok {
		return ip.String()
	}
	return nil
}
func lifecycle(c *gin.Context, a *platform.App, action string) (any, error) {
	if action == "delete" && c.GetBool("admin") {
		c.Set("allow_stuck_cleanup", true)
	}
	r, e := owned(c, a)
	if e != nil {
		return nil, e
	}
	if e = platform.Revision(c, r); e != nil {
		return nil, e
	}
	if e = permitted(c, r, action); e != nil {
		return nil, e
	}
	var in struct {
		Mode           string    `json:"mode"`
		Confirm        bool      `json:"confirm_data_loss_risk"`
		ConfirmName    string    `json:"confirm_name"`
		DeleteData     bool      `json:"delete_data"`
		CleanupStuck   bool      `json:"cleanup_stuck"`
		ConfirmRestart bool      `json:"confirm_restart"`
		Resources      Resources `json:"resources"`
	}
	if e = platform.Decode(c, &in); e != nil {
		return nil, e
	}
	if in.Mode != "" && in.Mode != "force" && in.Mode != "graceful" {
		return nil, bad("Invalid stop mode")
	}
	force := in.Mode == "force"
	if force && (!c.GetBool("admin") || !in.Confirm) {
		if !c.GetBool("admin") {
			return nil, platform.E(403, "force_not_allowed", "Force action requires administrator permission")
		}
		return nil, bad("Confirm data loss risk")
	}
	if in.CleanupStuck {
		if !c.GetBool("admin") {
			return nil, platform.E(403, "force_not_allowed", "Stuck cleanup requires administrator permission")
		}
		if action != "delete" || !force || !in.Confirm || !in.DeleteData || in.ConfirmName != platform.Text(r["logical_name"]) {
			return nil, bad("Stuck cleanup requires force mode, risk, exact name and delete_data confirmation")
		}
		out, err := admitStuckDelete(a, platform.DB(c, a), r)
		if err != nil {
			return nil, err
		}
		taskAccepted(c, out)
		return nil, nil
	}
	if r["creation_failed_at"] != nil {
		return nil, platform.E(404, "not_found", "Instance not found")
	}
	db := platform.DB(c, a)
	id := platform.Uint(r["id"])
	owner := platform.Uint(r["user_id"])
	if action == "stop" {
		out, e := QueueStop(c.Request.Context(), a, db, id, owner, force, "")
		if e != nil {
			return nil, e
		}
		taskAccepted(c, out)
		return nil, nil
	}
	if platform.Text(r["provisioning_state"]) != "ready" {
		return nil, conflict("instance_not_ready", "Instance is not ready")
	}
	params := platform.Row{"force": force}
	switch action {
	case "start":
		if platform.Text(r["observed_state"]) != "stopped" {
			return nil, conflict("invalid_state", "Start requires confirmed stopped state")
		}
		if e = reserveExisting(c.Request.Context(), a, db, r, nil); e != nil {
			return nil, e
		}
		if e = billing.AdmitInstance(c.Request.Context(), db, r, "start"); e != nil {
			return nil, e
		}
		if e = billing.InstanceUsage(c.Request.Context(), db, r, "disk_reserved", nil); e != nil {
			return nil, e
		}
	case "restart":
		if platform.Text(r["observed_state"]) != "running" {
			return nil, conflict("invalid_state", "Restart requires running state")
		}
		if e = billing.AdmitInstance(c.Request.Context(), db, r, "restart"); e != nil {
			return nil, e
		}
	case "delete":
		state := platform.Text(r["observed_state"])
		if (state != "stopped" && state != "running") || a.Time().Sub(platform.Stamp(r["observed_at"])) > time.Minute {
			return nil, conflict("invalid_state", "Deletion requires fresh confirmed stopped or running state")
		}
		if state == "running" && (!force || !in.Confirm) {
			return nil, conflict("force_confirmation_required", "Running deletion requires administrator force mode and data loss risk confirmation")
		}
		if in.ConfirmName != platform.Text(r["logical_name"]) || !in.DeleteData {
			return nil, bad("Exact name and delete_data confirmation required")
		}
	case "resize":
		state := platform.Text(r["observed_state"])
		restart := state == "running"
		if state != "stopped" && !restart {
			return nil, conflict("unsupported_configuration", "Resize requires a confirmed stopped or running instance")
		}
		if restart {
			if !in.ConfirmRestart {
				return nil, bad("Running resize requires confirm_restart")
			}
			for _, required := range []string{"stop", "start"} {
				if e = permitted(c, r, required); e != nil {
					return nil, e
				}
			}
		}
		if _, _, e = validResources(in.Resources); e != nil {
			return nil, e
		}
		if mustUint(in.Resources.DiskBytes) < platform.Uint(r["disk_bytes"]) {
			return nil, bad("Disk shrinking is prohibited")
		}
		if platform.Text(r["product_type"]) == "elastic" {
			p := object(r["plan_snapshot"])
			if len(p) == 0 {
				return nil, conflict("plan_snapshot_missing", "实例套餐快照缺失，请联系管理员修复；不会使用已编辑的套餐替代。")
			}
			over := object(p["allowed_overrides"])
			for key, value := range map[string]uint64{"vcpus": in.Resources.VCPUs, "memory_bytes": mustUint(in.Resources.MemoryBytes), "disk_bytes": mustUint(in.Resources.DiskBytes)} {
				bounds := object(over[key])
				if len(bounds) == 0 {
					if value != platform.Uint(p[key]) {
						return nil, bad("Plan does not permit this resource override")
					}
					continue
				}
				min, max, step := platform.Uint(bounds["min"]), platform.Uint(bounds["max"]), platform.Uint(bounds["step"])
				if step == 0 || value < min || value > max || (value-min)%step != 0 {
					return nil, bad("Resize exceeds allowed plan range or step")
				}
			}
		}
		reserved := in.Resources
		if restart {
			// Keep the larger reservation until native resize is confirmed: the
			// old guest still consumes its original resources before STOP.
			reserved.VCPUs = max(reserved.VCPUs, platform.Uint(r["vcpus"]))
			reserved.MemoryBytes = strconv.FormatUint(max(mustUint(reserved.MemoryBytes), platform.Uint(r["memory_bytes"])), 10)
		}
		if e = reserveExisting(c.Request.Context(), a, db, r, &in.Resources); e != nil {
			return nil, e
		}
		proposed := platform.Row{}
		for k, v := range r {
			proposed[k] = v
		}
		proposed["vcpus"] = in.Resources.VCPUs
		proposed["memory_bytes"] = mustUint(in.Resources.MemoryBytes)
		proposed["disk_bytes"] = mustUint(in.Resources.DiskBytes)
		if e = billing.AdmitInstance(c.Request.Context(), db, proposed, "resize"); e != nil {
			return nil, e
		}
		usageEvent := "resize_reserved"
		if restart {
			usageEvent = "resize_running_reserved"
		}
		if e = billing.InstanceUsage(c.Request.Context(), db, proposed, usageEvent, nil); e != nil {
			return nil, e
		}
		params["resources"] = in.Resources
		params["restart_after_resize"] = restart
		params["previous_resources"] = Resources{platform.Uint(r["vcpus"]), strconv.FormatUint(platform.Uint(r["memory_bytes"]), 10), strconv.FormatUint(platform.Uint(r["disk_bytes"]), 10)}
		if e = db.Table("instances").Where("id=?", id).Updates(platform.Row{"vcpus": reserved.VCPUs, "memory_bytes": mustUint(reserved.MemoryBytes), "disk_bytes": mustUint(reserved.DiskBytes)}).Error; e != nil {
			return nil, e
		}
	}
	desired := "running"
	if action == "delete" {
		desired = "deleted"
	}
	if action == "resize" && !platform.Bool(params["restart_after_resize"]) {
		desired = "stopped"
	}
	if e = db.Table("instances").Where("id=?", id).Updates(platform.Row{"desired_state": desired, "revision": gorm.Expr("revision+1")}).Error; e != nil {
		return nil, e
	}
	out, e := platform.Enqueue(db, "instance."+action, owner, platform.Uint(r["node_id"]), id, true, params)
	if e != nil {
		return nil, e
	}
	taskAccepted(c, out)
	return nil, nil
}
func QueueStop(ctx context.Context, a *platform.App, db *gorm.DB, instanceID, userID uint64, force bool, reason string) (platform.Row, error) {
	if e := lockOwnerAccount(db, userID); e != nil {
		return nil, e
	}
	r, e := platform.Get(locked(db), "instances", instanceID, true)
	if e != nil {
		return nil, e
	}
	if r["creation_failed_at"] != nil {
		return nil, platform.E(404, "not_found", "Instance not found")
	}
	if platform.Uint(r["user_id"]) != userID {
		return nil, platform.E(403, "forbidden", "Instance owner does not match")
	}
	if platform.Text(r["desired_state"]) == "deleted" {
		return nil, conflict("invalid_state", "Instance is being deleted")
	}
	if strings.HasPrefix(reason, "credit-enforcement:") {
		var active []platform.Row
		if e = locked(db).Table("tasks").Where("instance_id=? AND status IN ?", instanceID, []string{"queued", "running", "waiting", "unknown"}).Order("id").Find(&active).Error; e != nil {
			return nil, e
		}
		for _, task := range active {
			if platform.Text(task["task_type"]) != "instance.stop" {
				continue
			}
			if platform.Text(object(task["parameters_redacted"])["reason"]) != reason {
				continue
			}
			if platform.Text(task["status"]) == "unknown" {
				var count int64
				if e = db.Table("tasks").Where("task_type='instance.reconcile' AND instance_id=? AND status IN ?", instanceID, []string{"queued", "running", "waiting", "unknown"}).Count(&count).Error; e != nil {
					return nil, e
				}
				if count == 0 {
					if _, e = platform.Enqueue(db, "instance.reconcile", userID, platform.Uint(r["node_id"]), instanceID, false, platform.Row{"source_task_id": platform.Uint(task["id"]), "reason": reason}); e != nil {
						return nil, e
					}
				}
			}
			return platform.Row{"task_id": strconv.FormatUint(platform.Uint(task["id"]), 10), "instance_id": strconv.FormatUint(instanceID, 10), "status": platform.Text(task["status"])}, nil
		}
	}
	if e = db.Table("instances").Where("id=?", instanceID).Updates(platform.Row{"desired_state": "stopped", "revision": gorm.Expr("revision+1")}).Error; e != nil {
		return nil, e
	}
	control := strings.HasPrefix(reason, "credit-enforcement:")
	params := platform.Row{"force": force}
	if reason != "" {
		params["reason"] = reason
	}
	return platform.Enqueue(db, "instance.stop", userID, platform.Uint(r["node_id"]), instanceID, !control, params)
}
func mustUint(s string) uint64 { n, _ := strconv.ParseUint(s, 10, 64); return n }
func validResources(r Resources) (uint64, uint64, error) {
	m, e := strconv.ParseUint(r.MemoryBytes, 10, 64)
	if e != nil || m == 0 {
		return 0, 0, bad("memory_bytes must be a positive integer string")
	}
	d, e := strconv.ParseUint(r.DiskBytes, 10, 64)
	if e != nil || d == 0 || r.VCPUs == 0 || r.VCPUs > 65535 {
		return 0, 0, bad("Resources must be positive and within supported limits")
	}
	return m, d, nil
}
func validateVersions(ctx context.Context, a *platform.App, db *gorm.DB, versions map[string]string) error {
	for _, kind := range []string{"pricing", "credit", "metrics"} {
		if versions[kind] == "" {
			return conflict("policy_changed", "Required policy binding is missing: "+kind)
		}
	}
	for kind, value := range versions {
		if kind != "pricing" && kind != "credit" && kind != "gpu-idle" && kind != "gpu-scheduling" && kind != "gpu-pricing" && kind != "metrics" {
			return bad("Unknown quote policy kind")
		}
		id, err := platform.IDString(value)
		if err != nil {
			return err
		}
		policy, err := platform.Get(locked(db), "policy_versions", id, false)
		if err != nil {
			return err
		}
		if platform.Text(policy["kind"]) != kind || platform.Text(policy["status"]) != "active" || platform.Stamp(policy["effective_at"]).After(a.Time()) {
			return conflict("policy_changed", "Quoted policy is no longer published and effective")
		}
	}
	// Catalog.ValidateElastic separately matches the quoted plan revision and
	// policy versions; existing instances retain their admitted bindings.
	return nil
}
func validateCustomVersions(ctx context.Context, a *platform.App, db *gorm.DB, versions map[string]string, spec Spec) error {
	if e := validateVersions(ctx, a, db, versions); e != nil {
		return e
	}
	if spec.InstanceType == "container" {
		for _, kind := range []string{"gpu-idle", "gpu-scheduling"} {
			if versions[kind] == "" {
				return bad("Required custom policy binding is missing: " + kind)
			}
		}
	}
	if spec.GPU.Count > 0 {
		if versions["gpu-pricing"] == "" {
			return bad("Required custom policy binding is missing: gpu-pricing")
		}
		_, source, e := platform.PolicySource(db, "gpu-pricing", versions["gpu-pricing"], true)
		if e != nil {
			return e
		}
		price, e := platform.Get(db, "gpu_price_versions", source, false)
		if e != nil {
			return e
		}
		if platform.Text(price["gpu_model"]) != spec.GPU.Model || platform.Text(price["access_mode"]) != spec.GPU.AccessMode {
			return bad("GPU pricing profile does not match the requested model and access mode")
		}
	}
	return nil
}
func lockOwnerAccount(db *gorm.DB, owner uint64) error {
	var accounts []platform.Row
	return locked(db).Table("credit_accounts").Where("user_id=?", owner).Find(&accounts).Error
}
func instanceView(c *gin.Context, a *platform.App, r platform.Row) (platform.Row, error) {
	out := instanceDTO(a, r)
	creation := object(object(r["incus_config_snapshot"])["creation"])
	out["login_mode"] = "unknown"
	if mode := platform.Text(creation["login_mode"]); mode == "none" || mode == "password" || mode == "ssh_key" || mode == "password_and_ssh_key" {
		out["login_mode"] = mode
	}
	var creationTasks []platform.Row
	if err := platform.DB(c, a).Table("tasks").Where("instance_id=? AND task_type='instance.create'", r["id"]).Order("id").Limit(1).Find(&creationTasks).Error; err != nil {
		return nil, err
	}
	out["creation_task_id"] = nil
	if len(creationTasks) > 0 {
		out["creation_task_id"] = strconv.FormatUint(platform.Uint(creationTasks[0]["id"]), 10)
		progress, err := creationProgress(platform.DB(c, a), creationTasks[0])
		if err != nil {
			return nil, err
		}
		out["initialization_pending"] = progress["initialization_pending"]
	}
	caps := out["capabilities"].(platform.Row)
	for key, value := range caps {
		caps[key] = platform.Bool(value) && permitted(c, r, key) == nil
	}
	state := platform.Text(r["observed_state"])
	ready := platform.Text(r["provisioning_state"]) == "ready" && !platform.Bool(out["state_stale"])
	caps["resize"] = ready && (state == "running" || state == "stopped") && permitted(c, r, "resize") == nil && (state != "running" || (permitted(c, r, "stop") == nil && permitted(c, r, "start") == nil))
	caps["delete"] = ready && (state == "stopped" || (state == "running" && c.GetBool("admin"))) && permitted(c, r, "delete") == nil
	db := platform.DB(c, a)
	var active []platform.Row
	if e := db.Table("tasks").Select("id,status").Where("instance_id=? AND ((is_instance_mutation=1 AND status IN ?) OR ("+platform.RetainedTaskMutationSQL+"))", r["id"], []string{"queued", "running", "waiting", "unknown"}).Order("id DESC").Limit(1).Find(&active).Error; e != nil {
		return nil, e
	}
	out["active_task_id"] = nil
	out["active_task_status"] = nil
	if len(active) > 0 {
		out["active_task_id"] = strconv.FormatUint(platform.Uint(active[0]["id"]), 10)
		out["active_task_status"] = platform.Text(active[0]["status"])
	}
	var events []platform.Row
	if e := db.Table("credit_enforcements e").Select("e.id,e.status,e.shutdown_deadline").Joins("JOIN credit_enforcement_instances i ON i.enforcement_id=e.id").Where("i.instance_id=? AND e.status IN ?", r["id"], []string{"grace", "stopping", "unknown"}).Order("e.id DESC").Limit(1).Find(&events).Error; e != nil {
		return nil, e
	}
	out["credit_enforcement"] = platform.Row{"status": "normal", "event_id": nil, "shutdown_deadline": nil}
	if len(events) > 0 {
		event := events[0]
		out["credit_enforcement"] = platform.Row{"status": platform.Text(event["status"]), "event_id": strconv.FormatUint(platform.Uint(event["id"]), 10), "shutdown_deadline": platform.Stamp(event["shutdown_deadline"]).Format(time.RFC3339Nano)}
		caps := out["capabilities"].(platform.Row)
		caps["start"] = false
		caps["resize"] = false
		caps["gpu_attach"] = false
		caps["gpu_rebind"] = false
	}
	if r["creation_failed_at"] != nil {
		for key := range caps {
			caps[key] = false
		}
	}
	return out, nil
}
func taskDTO(db *gorm.DB, r platform.Row) (platform.Row, error) {
	out := platform.Public(r, "id", "instance_id", "node_id", "user_id", "status", "phase", "created_at", "started_at", "finished_at", "error_code", "revision")
	out["type"] = platform.Text(r["task_type"])
	if stopped := object(object(r["result_redacted"])["force_stop"]); platform.Bool(stopped["orchestration_stopped"]) {
		out["force_stop"] = platform.Row{"orchestration_stopped": true, "external_state": platform.Text(stopped["external_state"]), "requires_reconciliation": platform.Bool(stopped["requires_reconciliation"]) && !platform.Bool(stopped["reconciliation_completed"]), "resources_retained": platform.Bool(stopped["resources_retained"]), "reconciliation_completed": platform.Bool(stopped["reconciliation_completed"])}
	}
	if platform.Text(r["task_type"]) == "instance.create" {
		progress, err := creationProgress(db, r)
		if err != nil {
			return nil, err
		}
		out["creation_progress"] = progress
	}
	if platform.Text(r["task_type"]) == "image.prepare" {
		result := object(r["result_redacted"])
		safeResult := platform.Row{}
		for _, key := range []string{"progress", "status", "image_node_id", "fingerprint", "architecture"} {
			if value, ok := result[key].(string); ok {
				if len(value) > 512 {
					value = value[:512]
				}
				safeResult[key] = value
			}
		}
		out["result"] = safeResult
	}
	var steps []platform.Row
	if e := db.Table("task_steps").Select("step_key AS name,status,error_code,result_redacted,started_at,finished_at").Where("task_id=?", r["id"]).Order("id").Find(&steps).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	safe := make([]platform.Row, 0, len(steps))
	for _, step := range steps {
		view := platform.Public(step, "name", "status", "error_code", "started_at", "finished_at")
		view["error"] = platform.StoredTaskError(step["result_redacted"], platform.Text(step["status"]), platform.Text(step["error_code"]))
		safe = append(safe, view)
	}
	out["steps"] = safe
	out["error"] = platform.StoredTaskError(r["result_redacted"], platform.Text(r["status"]), platform.Text(r["error_code"]))
	out["resource_reservations_released"] = platform.Bool(object(r["result_redacted"])["creation_reservations_released"])
	out["next_action"] = "wait"
	if platform.Text(r["status"]) == "unknown" {
		out["next_action"] = "administrator_reconcile"
	} else if platform.Text(r["status"]) == "failed" {
		out["next_action"] = "inspect_failure"
	} else if platform.Text(r["status"]) == "succeeded" || platform.Text(r["status"]) == "cancelled" {
		out["next_action"] = "none"
	}
	if platform.Text(r["task_type"]) == "instance.create" {
		instance, err := platform.Get(db, "instances", platform.Uint(r["instance_id"]), false)
		if err != nil {
			return nil, err
		}
		if instance["creation_failed_at"] != nil {
			pending, err := billing.FailedCreationBillingPending(db.Statement.Context, db, instance)
			if err != nil {
				return nil, err
			}
			billingStatus := "refunded"
			if pending {
				billingStatus = "pending"
			}
			status := platform.Text(instance["creation_cleanup_state"])
			out["resource_reservations_released"] = status == "completed"
			out["creation_cleanup"] = platform.Row{"status": status, "billing_status": billingStatus, "instance_name": platform.Text(instance["logical_name"])}
			out["next_action"] = "wait_cleanup"
			if status == "completed" && !pending {
				out["next_action"] = "inspect_failure"
			}
		}
	}
	if stopped := object(out["force_stop"]); platform.Bool(stopped["requires_reconciliation"]) {
		out["next_action"] = "administrator_reconcile"
	}
	return out, nil
}
