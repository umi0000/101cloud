package instances

import (
	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/catalog"
	"101cloud/backend/internal/platform"
	"context"
	"crypto/rand"
	"encoding/hex"
	"gorm.io/gorm"
	"strconv"
	"strings"
	"time"
)

func createInstance(ctx context.Context, a *platform.App, db *gorm.DB, owner uint64, spec Spec, custom bool, versions map[string]string) (platform.Row, error) {
	return admitCreate(ctx, a, db, owner, spec, custom, versions, nil)
}
func reserveImported(ctx context.Context, a *platform.App, db *gorm.DB, owner uint64, spec Spec, native platform.Row) (platform.Row, error) {
	return admitCreate(ctx, a, db, owner, spec, true, nil, native)
}
func admitCreate(ctx context.Context, a *platform.App, db *gorm.DB, owner uint64, spec Spec, custom bool, versions map[string]string, native platform.Row) (platform.Row, error) {
	if e := lockOwnerAccount(db, owner); e != nil {
		return nil, e
	}
	if !custom {
		if len(versions) == 0 {
			return nil, bad("policy_versions from a current quote are required")
		}
		if e := validateVersions(ctx, a, db, versions); e != nil {
			return nil, e
		}
	}
	if strings.TrimSpace(spec.Name) == "" || len([]rune(spec.Name)) > 128 {
		return nil, bad("Name must be 1–128 characters")
	}
	memory, disk, e := validResources(spec.Resources)
	if e != nil {
		return nil, e
	}
	if spec.GPU.Count < 0 || spec.GPU.Count > 64 {
		return nil, bad("GPU count is outside supported range")
	}
	if spec.GPU.AccessMode == "" {
		spec.GPU.AccessMode = "shared"
	}
	if spec.GPU.MaxHourlyPT == "" {
		spec.GPU.MaxHourlyPT = "0.000000"
	}
	if spec.InitParameters == nil {
		spec.InitParameters = map[string]any{}
	}
	zone, e := platform.IDString(spec.ZoneID)
	if e != nil {
		return nil, e
	}
	imageID, e := platform.IDString(spec.ImageID)
	if e != nil {
		return nil, e
	}
	bindingID, e := platform.IDString(spec.NetworkOptionID)
	if e != nil {
		return nil, e
	}
	var image, plan, binding platform.Row
	var pricingID, creditID, idleID, templateID uint64
	if !custom {
		var model *string
		if spec.GPU.Model != "" {
			model = &spec.GPU.Model
		}
		validated, err := catalog.ValidateElastic(ctx, db, a, catalog.CreateElasticSpec{Name: spec.Name, ZoneID: spec.ZoneID, PlanID: spec.PlanID, ImageID: spec.ImageID, NetworkOptionID: spec.NetworkOptionID, Resources: catalog.Resources{VCPUs: spec.Resources.VCPUs, MemoryBytes: spec.Resources.MemoryBytes, DiskBytes: spec.Resources.DiskBytes}, GPU: catalog.GPURequest{Count: uint64(spec.GPU.Count), Model: model, AccessMode: spec.GPU.AccessMode, MaxHourlyPT: spec.GPU.MaxHourlyPT}, InitParameters: spec.InitParameters}, versions)
		if err != nil {
			return nil, err
		}
		image, plan, binding = validated.Image, validated.Plan, validated.Network
		pricingID, creditID, idleID = validated.PricingVersionID, validated.CreditPolicyVersionID, validated.IdlePolicyVersionID
		versions = validated.PolicyVersions
		templateID = platform.Uint(plan["init_template_version_id"])
		if spec.InstanceType != "" && spec.InstanceType != platform.Text(image["image_type"]) {
			return nil, bad("Instance type does not match image")
		}
		spec.InstanceType = platform.Text(image["image_type"])
	} else {
		image, e = platform.Get(db, "images", imageID, false)
		if e != nil {
			return nil, e
		}
		if platform.Text(image["status"]) != "active" {
			return nil, conflict("image_unavailable", "Image is not active")
		}
		binding, e = platform.Get(db, "node_network_bindings", bindingID, false)
		if e != nil {
			return nil, e
		}
		if spec.InstanceType != platform.Text(image["image_type"]) {
			return nil, bad("Instance type does not match image")
		}
		versions = map[string]string{}
		for kind, dest := range map[string]*uint64{"pricing": &pricingID, "credit": &creditID, "gpu-idle": &idleID} {
			var p platform.Row
			if e = db.Table("policy_versions p").Select("p.id,p.source_id").Joins("JOIN platform_policy_bindings b ON b.policy_id=p.id").Where("b.kind=? AND p.status='active' AND p.effective_at<=?", kind, a.Time()).Take(&p).Error; e != nil || platform.Uint(p["source_id"]) == 0 {
				return nil, platform.E(503, "policy_not_configured", "Published source policy is required")
			}
			*dest = platform.Uint(p["source_id"])
			versions[kind] = strconv.FormatUint(platform.Uint(p["id"]), 10)
		}
		if spec.InitTemplateVersionID != "" {
			templateID, e = platform.IDString(spec.InitTemplateVersionID)
			if e != nil {
				return nil, e
			}
		}
		if e = catalog.ValidateInit(ctx, db, templateID, spec.InitParameters); e != nil {
			return nil, e
		}
	}
	binding, e = platform.Get(db, "node_network_bindings", bindingID, true)
	if e != nil {
		return nil, e
	}
	if spec.InstanceType != "container" && spec.InstanceType != "virtual-machine" {
		return nil, bad("Unsupported instance type")
	}
	if spec.GPU.Count > 0 && (spec.InstanceType != "container" || !platform.Bool(image["gpu_ready"])) {
		return nil, bad("GPU requires a GPU-ready container image")
	}
	if platform.Text(binding["status"]) != "active" || platform.Text(binding["validation_status"]) != "verified" {
		return nil, conflict("network_unavailable", "Network option must be active and verified")
	}
	if spec.InstanceType == "virtual-machine" && platform.Text(binding["mode"]) == "macvlan" {
		return nil, conflict("unsupported_configuration", "Virtual machines require a bridge network")
	}
	nodeID := platform.Uint(binding["node_id"])
	if spec.NodeID != "" && mustUint(spec.NodeID) != nodeID {
		return nil, bad("Node does not match network option")
	}
	node, e := platform.Get(locked(db), "nodes", nodeID, true)
	if e != nil {
		return nil, e
	}
	if platform.Uint(node["availability_zone_id"]) != zone {
		return nil, bad("Node and zone do not match")
	}
	if platform.Text(node["administrative_state"]) != "active" || platform.Text(node["health"]) != "online" || platform.Stamp(node["connection_verified_at"]).IsZero() || a.Time().Sub(platform.Stamp(node["resources_sampled_at"])) > time.Minute || a.Time().Sub(platform.Stamp(node["last_heartbeat_at"])) > time.Minute {
		return nil, conflict("node_unavailable", "Node is not verified online with fresh inventory")
	}
	resources := object(node["resources"])
	arch := platform.Text(resources["architecture"])
	if arch != "" && arch != platform.Text(image["architecture"]) {
		return nil, conflict("unsupported_configuration", "Node architecture is incompatible")
	}
	pool, e := platform.Get(locked(db), "network_pools", platform.Uint(binding["network_pool_id"]), true)
	if e != nil {
		return nil, e
	}
	if platform.Text(pool["status"]) != "active" || platform.Uint(pool["availability_zone_id"]) != zone {
		return nil, conflict("network_unavailable", "Address pool is inactive or incompatible")
	}
	pricing, e := platform.Get(db, "pricing_versions", pricingID, false)
	if e != nil {
		return nil, e
	}
	allowed := spec.AllowedUserActions
	if !custom {
		allowed = []string{"start", "stop", "restart", "resize", "delete", "gpu_attach", "gpu_detach", "gpu_rebind", "gpu_policy", "console"}
	}
	if allowed == nil {
		allowed = []string{}
	}
	permissions := spec.GPUUserPermissions
	if !custom {
		permissions = map[string]bool{"idle": true, "idle_timeout": true, "idle_action": true, "rebalance": true, "allow_live": true, "max_hourly_pt": true}
	}
	if permissions == nil {
		permissions = map[string]bool{}
	}
	random := make([]byte, 12)
	if _, e = rand.Read(random); e != nil {
		return nil, e
	}
	incusName := "cloud-" + hex.EncodeToString(random)
	product, billingMode := "elastic", "credit"
	if custom {
		product, billingMode = "custom", "admin_exempt"
	}
	// custom_reason is retained for historic records; new provisioning stores no operation reason.
	r := platform.Row{"user_id": owner, "node_id": nodeID, "image_id": imageID, "instance_plan_id": nil, "logical_name": spec.Name, "project_name": binding["project_name"], "incus_name": incusName, "image_type": spec.InstanceType, "architecture": image["architecture"], "image_fingerprint": image["fingerprint"], "vcpus": spec.Resources.VCPUs, "memory_bytes": memory, "disk_bytes": disk, "storage_pool": node["default_storage_pool"], "pricing_version_id": pricingID, "cpu_hourly_pt": pricing["cpu_hourly_pt"], "memory_gib_hourly_pt": pricing["memory_gib_hourly_pt"], "disk_gib_hourly_pt": pricing["disk_gib_hourly_pt"], "credit_policy_version_id": creditID, "billing_mode": billingMode, "desired_state": "running", "observed_state": "pending", "provisioning_state": "pending", "auto_release_gpu": false, "auto_rebind_gpu": false, "idle_policy_version_id": idleID, "max_gpu_hourly_pt": spec.GPU.MaxHourlyPT, "init_template_version_id": nil, "init_parameters": jsonText(spec.InitParameters), "incus_config_snapshot": "{}", "product_type": product, "allowed_user_actions": jsonText(allowed), "gpu_user_permissions": jsonText(permissions), "custom_reason": nil, "policy_versions": jsonText(versions), "gpu_policy": jsonText(platform.Row{"idle": platform.Row{"enabled": false, "timeout_seconds": 1800, "action": "detach_gpu"}, "rebalance": platform.Row{"enabled": false, "allow_live": false, "max_hourly_pt": spec.GPU.MaxHourlyPT}}), "revision": 1, "created_at": a.Time(), "updated_at": a.Time()}
	if !custom {
		r["instance_plan_id"] = plan["id"]
	}
	if templateID != 0 {
		r["init_template_version_id"] = templateID
	}
	if native != nil {
		r["incus_name"] = native["name"]
		r["incus_uuid"] = object(native["config"])["volatile.uuid"]
		r["observed_state"] = strings.ToLower(platform.Text(native["status"]))
		r["desired_state"] = r["observed_state"]
		r["observed_at"] = a.Time()
		r["provisioning_state"] = "ready"
	}
	if native != nil {
		r["import_task_id"] = native["_import_task_id"]
	}
	if e = reserveExisting(ctx, a, db, r, nil); e != nil {
		return nil, e
	}
	if e = billing.AdmitInstance(ctx, db, r, "create"); e != nil {
		return nil, e
	}
	if e = db.Table("instances").Create(&r).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	id, e := platform.InsertedID(r)
	if e != nil {
		return nil, e
	}
	var ip platform.Row
	q := locked(db).Table("ip_addresses").Where("network_pool_id=? AND status='available'", pool["id"])
	if native != nil {
		nativeIP := platform.Text(native["_ip"])
		addr, err := parseAddress(nativeIP)
		if err != nil {
			return nil, err
		}
		q = q.Where("address=?", addr)
	}
	if e = q.Order("id").Take(&ip).Error; e != nil {
		return nil, conflict("ip_capacity_unavailable", "No approved address inventory is available")
	}
	if native != nil {
		uuids := []string{}
		for _, v := range rows(native["_gpu_uuids"]) {
			uuids = append(uuids, platform.Text(v))
		}
		spec.GPU.FixedUUIDs = uuids
		if spec.GPU.Count != len(uuids) {
			return nil, bad("Approved GPU count differs from imported configuration")
		}
	}
	var allocated []uint64
	if spec.GPU.Count > 0 {
		allocated, e = reserveGPUs(ctx, a, db, r, spec.GPU, nil)
		if e != nil {
			return nil, e
		}
	}
	if e = billing.InstanceUsage(ctx, db, r, "disk_reserved", nil); e != nil {
		return nil, e
	}
	if len(allocated) > 0 {
		if e = billing.InstanceUsage(ctx, db, r, "gpu_reserved", allocated); e != nil {
			return nil, e
		}
	}
	if native != nil {
		if e = db.Table("ip_addresses").Where("id=?", ip["id"]).Updates(platform.Row{"status": "bound", "node_id": nodeID, "node_network_binding_id": bindingID, "instance_id": id, "bound_at": a.Time()}).Error; e != nil {
			return nil, e
		}
		if len(allocated) > 0 {
			if e = db.Table("gpu_allocations").Where("id IN ?", allocated).Updates(platform.Row{"status": "active", "configured": true, "observed": true, "attached_at": a.Time(), "last_verified_at": a.Time()}).Error; e != nil {
				return nil, e
			}
			if e = billing.InstanceUsage(ctx, db, r, "gpu_attached", allocated); e != nil {
				return nil, e
			}
		}
		if platform.Text(r["observed_state"]) == "running" {
			if e = billing.InstanceUsage(ctx, db, r, "running", nil); e != nil {
				return nil, e
			}
		}
		return r, nil
	}
	task, e := platform.Enqueue(db, "instance.create", owner, nodeID, id, true, platform.Row{})
	if e != nil {
		return nil, e
	}
	if e = db.Table("ip_addresses").Where("id=?", ip["id"]).Updates(platform.Row{"status": "reserved", "node_id": nodeID, "node_network_binding_id": bindingID, "instance_id": id, "reservation_task_id": mustUint(platform.Text(task["task_id"])), "reserved_at": a.Time(), "reservation_expires_at": a.Time().Add(30 * time.Minute)}).Error; e != nil {
		return nil, e
	}
	return task, nil
}
func reserveExisting(ctx context.Context, a *platform.App, db *gorm.DB, r platform.Row, replacement *Resources) error {
	owner := platform.Uint(r["user_id"])
	userRecord, e := platform.Get(locked(db), "users", owner, true)
	if e != nil {
		return e
	}
	if platform.Text(userRecord["status"]) != "active" {
		return platform.E(403, "owner_inactive", "Instance owner is not active")
	}
	limits, e := queryRow(locked(db), "user_limits", "user_id=?", owner)
	if e != nil {
		if api, ok := e.(*platform.HTTPError); ok && api.Status == 404 {
			return conflict("quota_not_configured", "User quota is required")
		}
		return e
	}
	id := platform.Uint(r["id"])
	var user, node platform.Row
	if e = db.Table("instances").Select("COUNT(*) AS instances,COALESCE(SUM(vcpus),0) AS vcpus,COALESCE(SUM(memory_bytes),0) AS memory_bytes,COALESCE(SUM(disk_bytes),0) AS disk_bytes,COALESCE(SUM(product_type='custom'),0) AS resident").Where("user_id=? AND deleted_at IS NULL AND id<>?", owner, id).Take(&user).Error; e != nil {
		return e
	}
	v, m, d := platform.Uint(r["vcpus"]), platform.Uint(r["memory_bytes"]), platform.Uint(r["disk_bytes"])
	if replacement != nil {
		v = replacement.VCPUs
		m = mustUint(replacement.MemoryBytes)
		d = mustUint(replacement.DiskBytes)
	}
	for _, check := range []struct{ used, want, max uint64 }{{platform.Uint(user["instances"]), 1, platform.Uint(limits["max_instances"])}, {platform.Uint(user["vcpus"]), v, platform.Uint(limits["max_vcpus"])}, {platform.Uint(user["memory_bytes"]), m, platform.Uint(limits["max_memory_bytes"])}, {platform.Uint(user["disk_bytes"]), d, platform.Uint(limits["max_disk_bytes"])}} {
		if check.want > check.max || check.used > check.max-check.want {
			return conflict("quota_exceeded", "User resource quota exceeded")
		}
	}
	if platform.Text(r["product_type"]) == "custom" && platform.Uint(user["resident"])+1 > platform.Uint(limits["max_resident_instances"]) {
		return conflict("quota_exceeded", "Resident instance quota exceeded")
	}
	n, e := platform.Get(locked(db), "nodes", platform.Uint(r["node_id"]), true)
	if e != nil {
		return e
	}
	if platform.Text(n["administrative_state"]) != "active" || platform.Text(n["health"]) != "online" || platform.Stamp(n["connection_verified_at"]).IsZero() || a.Time().Sub(platform.Stamp(n["resources_sampled_at"])) > time.Minute {
		return conflict("node_unavailable", "Admission requires a verified online node with fresh inventory")
	}
	if e = db.Table("instances").Select("COALESCE(SUM(CASE WHEN product_type='custom' OR desired_state<>'stopped' OR observed_state<>'stopped' OR provisioning_state<>'ready' THEN vcpus ELSE 0 END),0) AS vcpus,COALESCE(SUM(CASE WHEN product_type='custom' OR desired_state<>'stopped' OR observed_state<>'stopped' OR provisioning_state<>'ready' THEN memory_bytes ELSE 0 END),0) AS memory_bytes,COALESCE(SUM(disk_bytes),0) AS disk_bytes,COALESCE(SUM(CASE WHEN product_type='custom' THEN vcpus ELSE 0 END),0) AS resident_vcpus,COALESCE(SUM(CASE WHEN product_type='custom' THEN memory_bytes ELSE 0 END),0) AS resident_memory,COALESCE(SUM(CASE WHEN product_type='custom' THEN disk_bytes ELSE 0 END),0) AS resident_disk").Where("node_id=? AND deleted_at IS NULL AND id<>?", r["node_id"], id).Take(&node).Error; e != nil {
		return e
	}
	for _, check := range []struct{ used, want, max uint64 }{{platform.Uint(node["vcpus"]), v, platform.Uint(n["allocatable_vcpus"])}, {platform.Uint(node["memory_bytes"]), m, platform.Uint(n["allocatable_memory_bytes"])}, {platform.Uint(node["disk_bytes"]), d, platform.Uint(n["allocatable_disk_bytes"])}} {
		if check.want > check.max || check.used > check.max-check.want {
			return conflict("node_capacity_unavailable", "Node resource capacity exhausted")
		}
	}
	if platform.Text(r["product_type"]) == "custom" {
		for _, c := range []struct{ used, want, max uint64 }{{platform.Uint(node["resident_vcpus"]), v, platform.Uint(n["resident_vcpu_budget"])}, {platform.Uint(node["resident_memory"]), m, platform.Uint(n["resident_memory_budget_bytes"])}, {platform.Uint(node["resident_disk"]), d, platform.Uint(n["resident_disk_budget_bytes"])}} {
			if c.want > c.max || c.used > c.max-c.want {
				return conflict("resident_capacity_unavailable", "Resident capacity exhausted")
			}
		}
	}
	return nil
}
