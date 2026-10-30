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
	Reason        string   `json:"reason,omitempty"`
}
type Spec struct {
	Name                  string          `json:"name"`
	InstanceType          string          `json:"instance_type,omitempty"`
	ZoneID                string          `json:"zone_id"`
	PlanID                string          `json:"plan_id,omitempty"`
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
func owned(c *gin.Context, a *platform.App) (platform.Row, error) {
	id, e := platform.ID(c, "id")
	if e != nil {
		return nil, e
	}
	db := platform.DB(c, a)
	r, e := platform.Get(db, "instances", id, false)
	if e != nil {
		return nil, e
	}
	if !c.GetBool("admin") && platform.Uint(r["user_id"]) != platform.Current(c).ID {
		return nil, platform.E(404, "not_found", "Instance not found")
	}
	if c.Request.Method != "GET" {
		if e = lockOwnerAccount(db, platform.Uint(r["user_id"])); e != nil {
			return nil, e
		}
		return platform.Get(db, "instances", id, true)
	}
	return r, nil
}
func permitted(c *gin.Context, r platform.Row, action string) error {
	if c.GetBool("admin") || platform.Text(r["product_type"]) != "custom" {
		return nil
	}
	for _, v := range rows(r["allowed_user_actions"]) {
		if platform.Text(v) == action {
			return nil
		}
	}
	return platform.E(403, "action_not_allowed", "Administrator has not authorized this custom instance action")
}
func instanceDTO(a *platform.App, r platform.Row) platform.Row {
	out := platform.Public(r, "id", "product_type", "billing_mode", "desired_state", "observed_state", "provisioning_state", "observed_at", "revision", "created_at", "updated_at", "node_id", "user_id")
	out["name"] = platform.Text(r["logical_name"])
	out["instance_type"] = platform.Text(r["image_type"])
	out["plan_id"] = nil
	if r["instance_plan_id"] != nil {
		out["plan_id"] = strconv.FormatUint(platform.Uint(r["instance_plan_id"]), 10)
	}
	out["image_id"] = strconv.FormatUint(platform.Uint(r["image_id"]), 10)
	out["resources"] = Resources{platform.Uint(r["vcpus"]), strconv.FormatUint(platform.Uint(r["memory_bytes"]), 10), strconv.FormatUint(platform.Uint(r["disk_bytes"]), 10)}
	stale := platform.Stamp(r["observed_at"]).IsZero() || a.Time().Sub(platform.Stamp(r["observed_at"])) > time.Minute
	out["state_stale"] = stale
	caps := platform.Row{"start": !stale && platform.Text(r["observed_state"]) == "stopped", "stop": !stale && platform.Text(r["observed_state"]) == "running", "gpu_attach": !stale && platform.Text(r["image_type"]) == "container", "gpu_rebind": !stale && platform.Text(r["image_type"]) == "container", "console": !stale && platform.Text(r["observed_state"]) == "running"}
	if platform.Text(r["product_type"]) == "custom" {
		allowed := map[string]bool{}
		for _, v := range rows(r["allowed_user_actions"]) {
			allowed[platform.Text(v)] = true
		}
		for key, value := range caps {
			caps[key] = platform.Bool(value) && allowed[key]
		}
	}
	out["capabilities"] = caps
	return out
}

func Register(a *platform.App, user, admin *gin.RouterGroup) {
	billing.SetStopHook(QueueStop)
	registerWorkers(a)
	registerRequests(a, user, admin)
	registerGPU(a, user, admin)
	for _, g := range []*gin.RouterGroup{user, admin} {
		g.GET("/instances", platform.Handle(func(c *gin.Context) (any, error) {
			q := platform.DB(c, a).Table("instances").Select("id,user_id,node_id,logical_name,product_type,billing_mode,image_type,image_id,instance_plan_id,vcpus,memory_bytes,disk_bytes,desired_state,observed_state,provisioning_state,observed_at,allowed_user_actions,revision,created_at,updated_at").Where("deleted_at IS NULL")
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
			if v := c.Query("zone_id"); v != "" {
				q = q.Where("node_id IN (SELECT id FROM nodes WHERE availability_zone_id=?)", v)
			}
			data, e := platform.List(c, a, q, []string{"product_type", "observed_state", "zone_id", "owner_user_id", "node_id"})
			if e != nil {
				return nil, e
			}
			page := data.(platform.Row)
			items := page["items"].([]platform.Row)
			for i, r := range items {
				out, e := instanceView(c, a, r)
				if e != nil {
					return nil, e
				}
				items[i] = out
			}
			return page, nil
		}))
		g.GET("/instances/:id", platform.Handle(func(c *gin.Context) (any, error) {
			r, e := owned(c, a)
			if e != nil {
				return nil, e
			}
			platform.SetETag(c, r)
			return instanceView(c, a, r)
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
			q := platform.DB(c, a).Table("tasks").Select("id,task_type AS type,instance_id,node_id,user_id,status,phase,created_at,started_at,finished_at,error_code,revision")
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
			return platform.List(c, a, q, []string{"instance_id", "status", "user_id", "node_id"})
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
			out := platform.Public(r, "id", "instance_id", "status", "phase", "created_at", "started_at", "finished_at", "error_code", "revision")
			out["type"] = platform.Text(r["task_type"])
			var steps []platform.Row
			if e = platform.DB(c, a).Table("task_steps").Select("step_key AS name,status,error_code,started_at,finished_at").Where("task_id=?", id).Order("id").Find(&steps).Error; e != nil {
				return nil, e
			}
			safe := make([]platform.Row, 0, len(steps))
			for _, step := range steps {
				safe = append(safe, platform.Public(step))
			}
			out["steps"] = safe
			out["next_action"] = "wait"
			if platform.Text(r["status"]) == "unknown" {
				out["next_action"] = "administrator_reconcile"
			}
			platform.SetETag(c, r)
			return out, nil
		}))
	}
	user.POST("/instances", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			Spec
			PolicyVersions map[string]string `json:"policy_versions"`
		}
		if e := platform.Decode(c, &in); e != nil {
			return nil, e
		}
		if in.NodeID != "" || len(in.GPU.FixedUUIDs) > 0 || len(in.AllowedUserActions) > 0 || len(in.GPUUserPermissions) > 0 {
			return nil, bad("Administrator-only configuration")
		}
		r, e := createInstance(c.Request.Context(), a, platform.DB(c, a), platform.Current(c).ID, in.Spec, false, in.PolicyVersions, "")
		if e != nil {
			return nil, e
		}
		requestAccepted(c, r)
		return nil, nil
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
		var in struct {
			Reason string `json:"reason"`
		}
		if e = platform.Decode(c, &in); e != nil {
			return nil, e
		}
		if strings.TrimSpace(in.Reason) == "" {
			return nil, bad("Reason is required")
		}
		if platform.Text(r["status"]) != "unknown" {
			return nil, conflict("task_not_unknown", "Only unknown tasks require reconciliation")
		}
		out, e := platform.Enqueue(platform.DB(c, a), "instance.reconcile", platform.Uint(r["user_id"]), platform.Uint(r["node_id"]), platform.Uint(r["instance_id"]), false, platform.Row{"source_task_id": id, "reason": in.Reason})
		if e != nil {
			return nil, e
		}
		requestAccepted(c, out)
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
		Mode           string            `json:"mode"`
		Confirm        bool              `json:"confirm_data_loss_risk"`
		ConfirmName    string            `json:"confirm_name"`
		DeleteData     bool              `json:"delete_data"`
		Reason         string            `json:"reason"`
		Resources      Resources         `json:"resources"`
		PolicyVersions map[string]string `json:"policy_versions"`
	}
	if e = platform.Decode(c, &in); e != nil {
		return nil, e
	}
	if c.GetBool("admin") && strings.TrimSpace(in.Reason) == "" {
		return nil, bad("Administrator reason is required")
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
	db := platform.DB(c, a)
	id := platform.Uint(r["id"])
	owner := platform.Uint(r["user_id"])
	if strings.HasPrefix(in.Reason, "credit-enforcement:") || strings.HasPrefix(in.Reason, "automatic_gpu_") {
		return nil, bad("Reserved internal operation reason")
	}
	if action == "stop" {
		out, e := QueueStop(c.Request.Context(), a, db, id, owner, force, in.Reason)
		if e != nil {
			return nil, e
		}
		requestAccepted(c, out)
		return nil, nil
	}
	if platform.Text(r["provisioning_state"]) != "ready" {
		return nil, conflict("instance_not_ready", "Instance is not ready")
	}
	params := platform.Row{"force": force, "reason": in.Reason}
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
		if platform.Text(r["observed_state"]) != "stopped" || a.Time().Sub(platform.Stamp(r["observed_at"])) > time.Minute {
			return nil, conflict("invalid_state", "Deletion requires fresh confirmed stopped state")
		}
		if in.ConfirmName != platform.Text(r["logical_name"]) || !in.DeleteData {
			return nil, bad("Exact name and delete_data confirmation required")
		}
	case "resize":
		if platform.Text(r["observed_state"]) != "stopped" {
			return nil, conflict("unsupported_configuration", "Stop the instance before resizing")
		}
		if _, _, e = validResources(in.Resources); e != nil {
			return nil, e
		}
		if mustUint(in.Resources.DiskBytes) < platform.Uint(r["disk_bytes"]) {
			return nil, bad("Disk shrinking is prohibited")
		}
		if platform.Text(r["product_type"]) == "elastic" {
			p, e := platform.Get(db, "instance_plans", platform.Uint(r["instance_plan_id"]), false)
			if e != nil {
				return nil, e
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
			if e = validateVersions(c.Request.Context(), a, db, in.PolicyVersions); e != nil {
				return nil, e
			}
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
		if e = billing.InstanceUsage(c.Request.Context(), db, proposed, "resize_reserved", nil); e != nil {
			return nil, e
		}
		params["resources"] = in.Resources
		params["previous_resources"] = Resources{platform.Uint(r["vcpus"]), strconv.FormatUint(platform.Uint(r["memory_bytes"]), 10), strconv.FormatUint(platform.Uint(r["disk_bytes"]), 10)}
		if e = db.Table("instances").Where("id=?", id).Updates(platform.Row{"vcpus": in.Resources.VCPUs, "memory_bytes": mustUint(in.Resources.MemoryBytes), "disk_bytes": mustUint(in.Resources.DiskBytes)}).Error; e != nil {
			return nil, e
		}
	}
	desired := "running"
	if action == "delete" {
		desired = "deleted"
	}
	if action == "resize" {
		desired = "stopped"
	}
	if e = db.Table("instances").Where("id=?", id).Updates(platform.Row{"desired_state": desired, "revision": gorm.Expr("revision+1")}).Error; e != nil {
		return nil, e
	}
	out, e := platform.Enqueue(db, "instance."+action, owner, platform.Uint(r["node_id"]), id, true, params)
	if e != nil {
		return nil, e
	}
	requestAccepted(c, out)
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
	return platform.Enqueue(db, "instance.stop", userID, platform.Uint(r["node_id"]), instanceID, !control, platform.Row{"force": force, "reason": reason})
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
	for _, kind := range []string{"pricing", "credit", "gpu-idle", "gpu-scheduling"} {
		if versions[kind] == "" {
			return conflict("policy_changed", "Required quote policy is missing")
		}
	}
	for kind, value := range versions {
		if kind != "pricing" && kind != "credit" && kind != "gpu-idle" && kind != "gpu-scheduling" && kind != "gpu-pricing" {
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
	// Catalog.ValidateElastic separately matches the selected immutable plan's
	// versions. A new platform default must not invalidate an unchanged quote.
	return nil
}
func lockOwnerAccount(db *gorm.DB, owner uint64) error {
	var accounts []platform.Row
	return locked(db).Table("credit_accounts").Where("user_id=?", owner).Find(&accounts).Error
}
func instanceView(c *gin.Context, a *platform.App, r platform.Row) (platform.Row, error) {
	out := instanceDTO(a, r)
	db := platform.DB(c, a)
	var node platform.Row
	if e := db.Table("nodes").Select("availability_zone_id").Where("id=?", r["node_id"]).Take(&node).Error; e != nil {
		return nil, e
	}
	out["zone_id"] = strconv.FormatUint(platform.Uint(node["availability_zone_id"]), 10)
	var active []platform.Row
	if e := db.Table("tasks").Select("id").Where("instance_id=? AND is_instance_mutation=1 AND status IN ?", r["id"], []string{"queued", "running", "waiting", "unknown"}).Order("id DESC").Limit(1).Find(&active).Error; e != nil {
		return nil, e
	}
	out["active_task_id"] = nil
	if len(active) > 0 {
		out["active_task_id"] = strconv.FormatUint(platform.Uint(active[0]["id"]), 10)
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
		caps["gpu_attach"] = false
		caps["gpu_rebind"] = false
	}
	return out, nil
}
