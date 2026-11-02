package instances

import (
	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/catalog"
	"101cloud/backend/internal/platform"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
			return nil, bad("请先获取最新报价并提交其中的 policy_versions。")
		}
		if e := validateVersions(ctx, a, db, versions); e != nil {
			return nil, e
		}
	}
	if strings.TrimSpace(spec.Name) == "" || len([]rune(spec.Name)) > 128 {
		return nil, bad("实例名称必须包含 1–128 个字符。")
	}
	memory, disk, e := validResources(spec.Resources)
	if e != nil {
		return nil, e
	}
	if spec.GPU.Count < 0 || spec.GPU.Count > 64 {
		return nil, bad("GPU 数量必须在 0–64 范围内。")
	}
	if spec.GPU.AccessMode == "" {
		spec.GPU.AccessMode = "shared"
	}
	if spec.GPU.MaxHourlyPT == "" {
		spec.GPU.MaxHourlyPT = "0.000000"
	}
	capPrice, err := policyMoney(spec.GPU.MaxHourlyPT)
	if err != nil {
		return nil, err
	}
	spec.GPU.MaxHourlyPT = capPrice.StringFixed(6)
	if spec.InitParameters == nil {
		spec.InitParameters = map[string]any{}
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
		validated, err := catalog.ValidateElastic(ctx, db, a, catalog.CreateElasticSpec{Name: spec.Name, PlanID: spec.PlanID, PlanRevision: spec.PlanRevision, ImageID: spec.ImageID, NetworkOptionID: spec.NetworkOptionID, NodeID: spec.NodeID, Resources: catalog.Resources{VCPUs: spec.Resources.VCPUs, MemoryBytes: spec.Resources.MemoryBytes, DiskBytes: spec.Resources.DiskBytes}, GPU: catalog.GPURequest{Count: uint64(spec.GPU.Count), Model: model, AccessMode: spec.GPU.AccessMode, MaxHourlyPT: spec.GPU.MaxHourlyPT}, InitParameters: spec.InitParameters}, versions)
		if err != nil {
			return nil, err
		}
		image, plan, binding = validated.Image, validated.Plan, validated.Network
		pricingID, creditID, idleID = validated.PricingVersionID, validated.CreditPolicyVersionID, validated.IdlePolicyVersionID
		versions = validated.PolicyVersions
		templateID = platform.Uint(plan["init_template_version_id"])
		if spec.InstanceType != "" && spec.InstanceType != platform.Text(image["image_type"]) {
			return nil, bad("实例类型与镜像不匹配，请选择对应类型的镜像。")
		}
		spec.InstanceType = platform.Text(image["image_type"])
	} else {
		image, e = platform.Get(db, "images", imageID, false)
		if e != nil {
			return nil, e
		}
		if platform.Text(image["status"]) != "active" {
			return nil, conflict("image_unavailable", "镜像未启用，请选择已启用的镜像。")
		}
		binding, e = platform.Get(db, "node_network_bindings", bindingID, false)
		if e != nil {
			return nil, e
		}
		if spec.InstanceType != platform.Text(image["image_type"]) {
			return nil, bad("实例类型与镜像不匹配，请选择对应类型的镜像。")
		}
		versions = map[string]string{}
		for kind, dest := range map[string]*uint64{"pricing": &pricingID, "credit": &creditID, "gpu-idle": &idleID} {
			var p platform.Row
			if e = db.Table("policy_versions p").Select("p.id,p.source_id").Joins("JOIN platform_policy_bindings b ON b.policy_id=p.id").Where("b.kind=? AND p.status='active' AND p.effective_at<=?", kind, a.Time()).Take(&p).Error; e != nil || platform.Uint(p["source_id"]) == 0 {
				return nil, platform.E(503, "policy_not_configured", "缺少已发布且生效的策略，请管理员完成策略配置。")
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
	image, e = catalog.ResolveNodeImage(ctx, db, image, platform.Uint(binding["node_id"]), platform.Text(binding["project_name"]))
	if e != nil {
		return nil, e
	}
	if native != nil {
		config := object(native["expanded_config"])
		if len(config) == 0 {
			config = object(native["config"])
		}
		if workerFingerprint(config["volatile.base_image"]) != workerFingerprint(image["fingerprint"]) || platform.Text(native["architecture"]) != platform.Text(image["architecture"]) {
			return nil, conflict("configuration_conflict", "原生实例镜像指纹或架构与节点镜像配置不匹配，请重新核对导入配置。")
		}
	}
	if spec.InstanceType != "container" && spec.InstanceType != "virtual-machine" {
		return nil, bad("不支持的实例类型，请使用 container 或 virtual-machine。")
	}
	if spec.GPU.Count > 0 && (spec.InstanceType != "container" || !platform.Bool(image["gpu_ready"])) {
		return nil, bad("GPU 仅支持已标记为 GPU 就绪的容器镜像。")
	}
	if platform.Text(binding["status"]) != "active" || platform.Text(binding["validation_status"]) != "verified" {
		return nil, conflict("network_unavailable", "网络选项必须已启用且通过验证，请管理员验证后重试。")
	}
	if spec.InstanceType == "virtual-machine" && platform.Text(binding["mode"]) == "macvlan" {
		return nil, conflict("unsupported_configuration", "虚拟机需要桥接网络，不支持 macvlan。")
	}
	nodeID := platform.Uint(binding["node_id"])
	if spec.NodeID != "" && mustUint(spec.NodeID) != nodeID {
		return nil, bad("节点与网络选项不匹配，请重新选择。")
	}
	node, e := platform.Get(locked(db), "nodes", nodeID, true)
	if e != nil {
		return nil, e
	}
	if platform.Text(node["administrative_state"]) != "active" || platform.Text(node["health"]) != "online" || platform.Stamp(node["connection_verified_at"]).IsZero() || a.Time().Sub(platform.Stamp(node["resources_sampled_at"])) > time.Minute || a.Time().Sub(platform.Stamp(node["last_heartbeat_at"])) > time.Minute {
		return nil, conflict("node_unavailable", "节点未通过连接验证、未在线或资源观测已过期，请管理员检查节点连接与采集状态。")
	}
	resources := object(node["resources"])
	arch := platform.Text(resources["architecture"])
	if arch != "" && arch != platform.Text(image["architecture"]) {
		return nil, conflict("unsupported_configuration", "节点架构与镜像不兼容，请选择相同架构的镜像或节点。")
	}
	pool, e := platform.Get(locked(db), "network_pools", platform.Uint(binding["network_pool_id"]), true)
	if e != nil {
		return nil, e
	}
	if platform.Text(pool["status"]) != "active" {
		return nil, conflict("network_unavailable", "地址池未启用，请检查网络选项。")
	}
	pricing, e := platform.Get(db, "pricing_versions", pricingID, false)
	if e != nil {
		return nil, e
	}
	allowed := spec.AllowedUserActions
	permissions := spec.GPUUserPermissions
	gpuPolicy := platform.Row{"idle": platform.Row{"enabled": false, "timeout_seconds": uint64(1800), "action": "detach_gpu"}, "rebalance": platform.Row{"enabled": false, "allow_live": false, "max_hourly_pt": spec.GPU.MaxHourlyPT}}
	if !custom {
		allowed = nil
		for _, v := range rows(plan["allowed_user_actions"]) {
			allowed = append(allowed, platform.Text(v))
		}
		permissions = map[string]bool{}
		for key, value := range object(plan["gpu_user_permissions"]) {
			permissions[key] = platform.Bool(value)
		}
		gpuPolicy = platform.Row{}
		for key, value := range object(plan["gpu_policy"]) {
			gpuPolicy[key] = value
		}
		rebalance := object(gpuPolicy["rebalance"])
		rebalance["max_hourly_pt"] = spec.GPU.MaxHourlyPT
		gpuPolicy["rebalance"] = rebalance
	}
	if allowed == nil {
		allowed = []string{}
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
	r := platform.Row{"user_id": owner, "node_id": nodeID, "image_id": imageID, "instance_plan_id": nil, "logical_name": spec.Name, "project_name": binding["project_name"], "incus_name": incusName, "image_type": spec.InstanceType, "architecture": image["architecture"], "image_fingerprint": image["fingerprint"], "vcpus": spec.Resources.VCPUs, "memory_bytes": memory, "disk_bytes": disk, "storage_pool": node["default_storage_pool"], "pricing_version_id": pricingID, "cpu_hourly_pt": pricing["cpu_hourly_pt"], "memory_gib_hourly_pt": pricing["memory_gib_hourly_pt"], "disk_gib_hourly_pt": pricing["disk_gib_hourly_pt"], "credit_policy_version_id": creditID, "billing_mode": billingMode, "desired_state": "running", "observed_state": "pending", "provisioning_state": "pending", "auto_release_gpu": false, "auto_rebind_gpu": false, "idle_policy_version_id": idleID, "max_gpu_hourly_pt": spec.GPU.MaxHourlyPT, "init_template_version_id": nil, "init_parameters": jsonText(spec.InitParameters), "incus_config_snapshot": "{}", "product_type": product, "allowed_user_actions": jsonText(allowed), "gpu_user_permissions": jsonText(permissions), "policy_versions": jsonText(versions), "gpu_policy": jsonText(gpuPolicy), "revision": 1, "created_at": a.Time(), "updated_at": a.Time()}
	idle := object(gpuPolicy["idle"])
	r["auto_release_gpu"] = platform.Bool(idle["enabled"]) && platform.Text(idle["action"]) == "detach_gpu"
	r["auto_rebind_gpu"] = platform.Bool(object(gpuPolicy["rebalance"])["enabled"])
	if !custom {
		r["instance_plan_id"] = plan["id"]
		r["plan_snapshot"] = jsonText(platform.Row{"revision": platform.Text(plan["revision"]), "vcpus": platform.Uint(plan["vcpus"]), "memory_bytes": platform.Text(plan["memory_bytes"]), "disk_bytes": platform.Text(plan["disk_bytes"]), "allowed_overrides": object(plan["allowed_overrides"])})
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
	if native == nil {
		if e = StorageCapacity(ctx, a, nodeID, platform.Text(r["storage_pool"]), platform.Text(r["project_name"]), disk); e != nil {
			return nil, e
		}
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
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return nil, e
		}
		var registered, available int64
		if e = db.Table("ip_addresses").Where("network_pool_id=?", pool["id"]).Count(&registered).Error; e != nil {
			return nil, e
		}
		if e = db.Table("ip_addresses").Where("network_pool_id=? AND status='available'", pool["id"]).Count(&available).Error; e != nil {
			return nil, e
		}
		message := "地址库存不足：已登记地址均不可分配；配置地址池范围不会自动登记地址。"
		if registered == 0 {
			message = "地址池尚未登记任何地址：配置的地址范围不等于可分配库存。"
		} else if native != nil && available > 0 {
			message = "导入实例指定的 IP 未登记为可分配地址，不能使用其他库存地址代替。"
		}
		return nil, platform.EDetails(409, "ip_capacity_unavailable", message+fmt.Sprintf("已用 %d + 本次申请 1，登记库存上限 %d，当前可用 %d（count）。", registered-available, registered, available), platform.Row{"scope": "ip_pool", "resource": "addresses", "used": strconv.FormatInt(registered-available, 10), "requested": "1", "limit": strconv.FormatInt(registered, 10), "available": strconv.FormatInt(available, 10), "unit": "count", "action": "请管理员在地址池允许范围内登记经确认可供平台使用的地址；若已有地址被占用，请释放或补充库存。ARP 探测不能证明地址可供分配。"})
	}
	if native != nil {
		uuids := []string{}
		for _, v := range rows(native["_gpu_uuids"]) {
			uuids = append(uuids, platform.Text(v))
		}
		spec.GPU.FixedUUIDs = uuids
		if spec.GPU.Count != len(uuids) {
			return nil, bad("批准的 GPU 数量与导入实例的实际配置不一致，请核对配置。")
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
		return platform.E(403, "owner_inactive", "实例所属用户未启用，请管理员检查用户状态。")
	}
	limits, e := queryRow(locked(db), "user_limits", "user_id=?", owner)
	if e != nil {
		if api, ok := e.(*platform.HTTPError); ok && api.Status == 404 {
			return conflict("quota_not_configured", "用户尚未配置资源配额，请联系管理员配置。")
		}
		return e
	}
	user, e := reservationUsage(db, r, false)
	if e != nil {
		return e
	}
	v, m, d := platform.Uint(r["vcpus"]), platform.Uint(r["memory_bytes"]), platform.Uint(r["disk_bytes"])
	if replacement != nil {
		v = replacement.VCPUs
		m = mustUint(replacement.MemoryBytes)
		d = mustUint(replacement.DiskBytes)
	}
	for _, check := range []struct {
		resource, label, unit string
		used, want, max       uint64
	}{
		{"instances", "实例数量", "count", platform.Uint(user["instances"]), 1, platform.Uint(limits["max_instances"])},
		{"vcpus", "CPU 核数", "count", platform.Uint(user["vcpus"]), v, platform.Uint(limits["max_vcpus"])},
		{"memory_bytes", "内存", "bytes", platform.Uint(user["memory_bytes"]), m, platform.Uint(limits["max_memory_bytes"])},
		{"disk_bytes", "磁盘", "bytes", platform.Uint(user["disk_bytes"]), d, platform.Uint(limits["max_disk_bytes"])},
	} {
		if check.want > check.max || check.used > check.max-check.want {
			return admissionCapacityError("quota_exceeded", "user_quota", check.resource, check.label, check.unit, check.used, check.want, check.max, "请减少申请量、删除不再需要的实例，或联系管理员提高用户配额；已有实例也计入配额。")
		}
	}
	if platform.Text(r["product_type"]) == "custom" && platform.Uint(user["resident"])+1 > platform.Uint(limits["max_resident_instances"]) {
		return admissionCapacityError("quota_exceeded", "user_quota", "resident_instances", "常驻实例数量", "count", platform.Uint(user["resident"]), 1, platform.Uint(limits["max_resident_instances"]), "请删除不再需要的常驻实例，或联系管理员提高常驻实例配额。")
	}
	n, e := platform.Get(locked(db), "nodes", platform.Uint(r["node_id"]), true)
	if e != nil {
		return e
	}
	if e = admissionNodeAvailable(a, n); e != nil {
		return e
	}
	node, e := reservationUsage(db, r, true)
	if e != nil {
		return e
	}
	for _, check := range []struct {
		resource, label, unit string
		used, want, max       uint64
	}{
		{"vcpus", "节点 CPU 核数", "count", platform.Uint(node["vcpus"]), v, platform.Uint(n["allocatable_vcpus"])},
		{"memory_bytes", "节点内存", "bytes", platform.Uint(node["memory_bytes"]), m, platform.Uint(n["allocatable_memory_bytes"])},
		{"disk_bytes", "节点磁盘", "bytes", platform.Uint(node["disk_bytes"]), d, platform.Uint(n["allocatable_disk_bytes"])},
	} {
		if check.want > check.max || check.used > check.max-check.want {
			return admissionCapacityError("node_capacity_unavailable", "node_capacity", check.resource, check.label, check.unit, check.used, check.want, check.max, "请减少申请量或选择其他节点；管理员可检查节点可分配容量与已有实例占用。")
		}
	}
	if platform.Text(r["product_type"]) == "custom" {
		for _, c := range []struct {
			resource, label, unit string
			used, want, max       uint64
		}{
			{"vcpus", "常驻 CPU 预算", "count", platform.Uint(node["resident_vcpus"]), v, platform.Uint(n["resident_vcpu_budget"])},
			{"memory_bytes", "常驻内存预算", "bytes", platform.Uint(node["resident_memory"]), m, platform.Uint(n["resident_memory_budget_bytes"])},
			{"disk_bytes", "常驻磁盘预算", "bytes", platform.Uint(node["resident_disk"]), d, platform.Uint(n["resident_disk_budget_bytes"])},
		} {
			if c.want > c.max || c.used > c.max-c.want {
				return admissionCapacityError("resident_capacity_unavailable", "resident_capacity", c.resource, c.label, c.unit, c.used, c.want, c.max, "请减少常驻资源申请、选择其他节点，或联系管理员调整常驻资源预算。")
			}
		}
	}
	return nil
}

func admissionCapacityError(code, scope, resource, label, unit string, used, requested, limit uint64, action string) error {
	available := uint64(0)
	if used < limit {
		available = limit - used
	}
	return platform.EDetails(409, code, label+"不足，本次申请将超过可分配上限。", platform.Row{
		"scope": scope, "resource": resource, "used": strconv.FormatUint(used, 10), "requested": strconv.FormatUint(requested, 10), "limit": strconv.FormatUint(limit, 10), "available": strconv.FormatUint(available, 10), "unit": unit, "action": action,
	})
}
