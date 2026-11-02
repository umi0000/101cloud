package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Resources struct {
	VCPUs       uint64 `json:"vcpus"`
	MemoryBytes string `json:"memory_bytes"`
	DiskBytes   string `json:"disk_bytes"`
}
type GPURequest struct {
	Count       uint64  `json:"count"`
	Model       *string `json:"model"`
	AccessMode  string  `json:"access_mode"`
	MaxHourlyPT string  `json:"max_hourly_pt"`
}
type CreateElasticSpec struct {
	Name            string         `json:"name"`
	PlanID          string         `json:"plan_id"`
	PlanRevision    string         `json:"plan_revision,omitempty"`
	ImageID         string         `json:"image_id"`
	NetworkOptionID string         `json:"network_option_id"`
	NodeID          string         `json:"node_id,omitempty"`
	Resources       Resources      `json:"resources"`
	GPU             GPURequest     `json:"gpu"`
	InitParameters  map[string]any `json:"init_parameters"`
}
type ValidatedSpec struct {
	Plan, Image, Network                                         platform.Row
	PolicyVersions                                               map[string]string
	PricingVersionID, CreditPolicyVersionID, IdlePolicyVersionID uint64
}

func validResources(r Resources) error {
	if r.VCPUs == 0 || r.VCPUs > 4294967295 {
		return invalid("vcpus 必须是大于零的 uint32 整数。")
	}
	if _, e := unsigned(r.MemoryBytes, true); e != nil {
		return platform.EDetails(422, "validation_failed", "resources.memory_bytes 必须为大于零的十进制整数字符串。", platform.Row{"field": "resources.memory_bytes", "action": "请以字节为单位填写正整数，不要附带 GiB 等单位。"})
	}
	if _, e := unsigned(r.DiskBytes, true); e != nil {
		return platform.EDetails(422, "validation_failed", "resources.disk_bytes 必须为大于零的十进制整数字符串。", platform.Row{"field": "resources.disk_bytes", "action": "请以字节为单位填写正整数，不要附带 GiB 等单位。"})
	}
	return nil
}

// validGPUTerms checks the GPU fields shared by plan offerings and requests.
func validGPUTerms(g GPURequest) error {
	if !enum(g.AccessMode, "shared", "exclusive") {
		return invalid("请指定 gpu.access_mode 为 shared 或 exclusive。")
	}
	if _, e := planMoney(g.MaxHourlyPT); e != nil {
		return e
	}
	if g.Count > 4294967295 {
		return invalid("gpu.count 必须为 uint32 整数。")
	}
	if g.Model != nil && !textValid(*g.Model, 255) {
		return invalid("gpu.model 必须包含 1–255 个字符。")
	}
	return nil
}

// validGPU checks an elastic request. Count 0 opts out of GPUs and needs no model.
func validGPU(g GPURequest, typ string) error {
	if e := validGPUTerms(g); e != nil {
		return e
	}
	if typ == "virtual-machine" && g.Count != 0 {
		return invalid("虚拟机不支持 GPU，请将 GPU 数量设为零。")
	}
	if g.Count == 0 && g.Model != nil {
		return invalid("GPU 数量为零时，gpu.model 必须为 null。")
	}
	if g.Count > 0 && g.Model == nil {
		return invalid("申请 GPU 时必须指定 gpu.model。")
	}
	return nil
}

// validPlanGPU checks a plan's GPU offering. A container plan with a model offers
// GPUs by default (count > 0) or optionally (count 0 with gpu_count override max >= 1).
// VM plans never offer GPUs.
func validPlanGPU(g GPURequest, typ string, overrideMax uint64) error {
	if e := validGPUTerms(g); e != nil {
		return e
	}
	if typ == "virtual-machine" {
		if g.Count != 0 || g.Model != nil || overrideMax > 0 {
			return invalid("虚拟机套餐不能提供 GPU。")
		}
		return nil
	}
	if g.Model == nil {
		if g.Count > 0 || overrideMax > 0 {
			return invalid("套餐提供 GPU 时必须指定 GPU 型号。")
		}
		return nil
	}
	if g.Count == 0 && overrideMax == 0 {
		return invalid("配置 GPU 型号时，gpu.count 必须大于零或允许覆盖的 GPU 数量上限至少为 1。")
	}
	return nil
}
func versionMap(v any) map[string]string {
	out := map[string]string{}
	for k, x := range object(v) {
		out[k] = platform.Text(x)
	}
	return out
}
func resolvePolicies(ctx context.Context, db *gorm.DB, a *platform.App, versions map[string]string, active bool) (map[string]platform.Row, error) {
	out := map[string]platform.Row{}
	for _, k := range []string{"pricing", "credit", "gpu-idle", "gpu-scheduling"} {
		if versions[k] == "" {
			return nil, platform.E(409, "policy_not_configured", "所需策略 "+k+" 尚未配置，请联系管理员发布并绑定策略。")
		}
	}
	for k, s := range versions {
		if !enum(k, "pricing", "credit", "gpu-idle", "gpu-scheduling", "gpu-pricing") {
			return nil, invalid("policy_versions 包含未知的策略类型。")
		}
		id, e := platform.IDString(s)
		if e != nil {
			return nil, e
		}
		r, e := platform.Get(db, "policy_versions", id, false)
		if e != nil {
			return nil, e
		}
		if platform.Text(r["kind"]) != k {
			return nil, platform.EDetails(422, "validation_failed", "策略版本不存在或类型不匹配，请重新获取报价。", platform.Row{"field": "policy_versions." + k, "action": "请使用最新报价返回的对应类型策略版本。"})
		}
		if active && (platform.Text(r["status"]) != "active" || platform.Stamp(r["effective_at"]).After(a.Time())) {
			return nil, platform.E(409, "policy_changed", "所选策略已失效，请重新获取报价。")
		}
		out[k] = r
	}
	return out, nil
}
func ValidateElastic(ctx context.Context, db *gorm.DB, a *platform.App, s CreateElasticSpec, versions map[string]string) (*ValidatedSpec, error) {
	if !textValid(s.Name, 128) {
		return nil, invalid("实例名称必须包含 1–128 个字符。")
	}
	if e := validResources(s.Resources); e != nil {
		return nil, e
	}
	pid, e := platform.IDString(s.PlanID)
	if e != nil {
		return nil, e
	}
	// Hold the plan while reading its relations and admitting the frozen spec.
	plan, e := platform.Get(db.Clauses(clause.Locking{Strength: "SHARE"}), "instance_plans", pid, false)
	if e != nil {
		return nil, e
	}
	if (versions != nil || s.PlanRevision != "") && s.PlanRevision != platform.Text(plan["revision"]) {
		return nil, platform.E(409, "plan_changed", "套餐已变更，请重新获取报价并提交最新 plan_revision。")
	}
	if platform.Text(plan["status"]) != "active" {
		return nil, conflict("套餐未启用，请选择已启用套餐。")
	}
	typ := platform.Text(plan["image_type"])
	if e = validGPU(s.GPU, typ); e != nil {
		return nil, e
	}
	if e = validateOverrides(plan, s.Resources, s.GPU); e != nil {
		return nil, e
	}
	image, e := platform.IDString(s.ImageID)
	if e != nil {
		return nil, e
	}
	var n int64
	if e = db.Table("plan_images").Where("plan_id = ? AND image_id = ?", pid, image).Count(&n).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	if n != 1 {
		return nil, invalid("套餐不允许使用此镜像，请选择套餐允许的镜像。")
	}
	ir, e := platform.Get(db, "images", image, false)
	if e != nil {
		return nil, e
	}
	network, e := platform.IDString(s.NetworkOptionID)
	if e != nil {
		return nil, e
	}
	if e = db.Table("plan_network_options").Where("plan_id = ? AND network_option_id = ?", pid, network).Count(&n).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	if n != 1 {
		return nil, invalid("套餐不允许此网络选项，请选择套餐允许的网络。")
	}
	nr, e := ValidateNetworkOption(db, network, typ)
	if e != nil {
		return nil, e
	}
	nodeID := platform.Uint(nr["node_id"])
	if s.NodeID != "" {
		requested, err := platform.IDString(s.NodeID)
		if err != nil {
			return nil, err
		}
		if requested != nodeID {
			return nil, invalid("node_id 必须与所选网络所属节点一致。")
		}
	}
	if e = db.Table("plan_nodes").Where("plan_id=? AND node_id=?", pid, nodeID).Count(&n).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	if n != 1 {
		return nil, invalid("所选网络节点不在套餐允许的 node_ids 范围内。")
	}
	ir, e = ResolveNodeImage(ctx, db, ir, platform.Uint(nr["node_id"]), platform.Text(nr["project_name"]))
	if e != nil {
		return nil, e
	}
	if platform.Text(ir["status"]) != "active" || platform.Text(ir["image_type"]) != typ || (s.GPU.Count > 0 && !platform.Bool(ir["gpu_ready"])) {
		return nil, invalid("镜像未启用、类型不匹配或不支持所申请的 GPU，请重新选择。")
	}
	node, e := platform.Get(db, "nodes", platform.Uint(nr["node_id"]), false)
	if e != nil {
		return nil, e
	}
	if arch := platform.Text(object(node["resources"])["architecture"]); arch != "" && arch != platform.Text(ir["architecture"]) {
		return nil, invalid("节点架构与镜像不兼容，请选择相同架构的镜像或节点。")
	}
	configured := versionMap(plan["policy_versions"])
	if versions != nil {
		if len(versions) != len(configured) {
			return nil, platform.E(409, "policy_changed", "报价对应策略已变更，请重新获取报价。")
		}
		for k, v := range configured {
			if versions[k] != v {
				return nil, platform.E(409, "policy_changed", "报价对应策略已变更，请重新获取报价。")
			}
		}
	}
	policies, e := resolvePolicies(ctx, db, a, configured, true)
	if e != nil {
		return nil, e
	}
	if s.GPU.Count > 0 && policies["gpu-pricing"] == nil {
		return nil, platform.E(409, "policy_not_configured", "GPU 定价策略尚未配置，请联系管理员发布并绑定策略。")
	}
	if e = ValidateInit(ctx, db, platform.Uint(plan["init_template_version_id"]), s.InitParameters); e != nil {
		return nil, e
	}
	return &ValidatedSpec{Plan: plan, Image: ir, Network: nr, PolicyVersions: configured, PricingVersionID: platform.Uint(policies["pricing"]["source_id"]), CreditPolicyVersionID: platform.Uint(policies["credit"]["source_id"]), IdlePolicyVersionID: platform.Uint(policies["gpu-idle"]["source_id"])}, nil
}
func validateOverrides(plan platform.Row, r Resources, g GPURequest) error {
	bounds := object(plan["allowed_overrides"])
	values := map[string]uint64{"vcpus": r.VCPUs, "memory_bytes": mustUnsigned(r.MemoryBytes), "disk_bytes": mustUnsigned(r.DiskBytes)}
	// gpu.count 0 opts out of the plan's GPU offering and is always allowed.
	if g.Count > 0 {
		if platform.Text(plan["gpu_model"]) == "" || platform.Text(plan["image_type"]) != "container" {
			return invalid("此套餐不提供 GPU，请选择支持 GPU 的套餐。")
		}
		values["gpu_count"] = g.Count
	}
	for k, v := range values {
		if b := object(bounds[k]); b != nil {
			lo, hi := platform.Uint(b["min"]), platform.Uint(b["max"])
			step := platform.Uint(b["step"])
			if lo > hi || v < lo || v > hi || step == 0 || (v-lo)%step != 0 {
				return invalid("资源超出套餐允许的覆盖范围或步长：" + k)
			}
		} else if v != platform.Uint(plan[k]) {
			return invalid("套餐不允许修改此资源：" + k)
		}
	}
	limit, e := planMoney(platform.Text(plan["gpu_max_hourly_pt"]))
	if e != nil {
		return invalid("套餐 GPU 价格上限缺失或无效，请联系管理员检查套餐。")
	}
	price, e := planMoney(g.MaxHourlyPT)
	if e != nil {
		return e
	}
	if price.GreaterThan(limit) {
		return invalid("申请的 GPU 价格上限超过套餐限制，请降低 gpu.max_hourly_pt。")
	}
	if g.Count > 0 && (g.Model == nil || *g.Model != platform.Text(plan["gpu_model"]) || g.AccessMode != platform.Text(plan["gpu_access_mode"])) {
		return invalid("GPU 型号和访问模式必须与套餐一致。")
	}
	return nil
}
func mustUnsigned(s string) uint64 { n, _ := strconv.ParseUint(s, 10, 64); return n }
func Quote(ctx context.Context, db *gorm.DB, a *platform.App, s CreateElasticSpec) (platform.Row, error) {
	v, e := ValidateElastic(ctx, db, a, s, nil)
	if e != nil {
		return nil, e
	}
	price, e := platform.Get(db, "pricing_versions", v.PricingVersionID, false)
	if e != nil {
		return nil, e
	}
	rates := map[string]decimal.Decimal{}
	for _, k := range []string{"cpu_hourly_pt", "memory_gib_hourly_pt", "disk_gib_hourly_pt"} {
		d, e := decimal.NewFromString(platform.Text(price[k]))
		if e != nil {
			return nil, platform.WrapDB(e)
		}
		rates[k] = d
	}
	gib := decimal.NewFromInt(1073741824)
	cpu := rates["cpu_hourly_pt"].Mul(decimal.NewFromBigInt(new(big.Int).SetUint64(s.Resources.VCPUs), 0))
	mem := rates["memory_gib_hourly_pt"].Mul(decimal.NewFromBigInt(new(big.Int).SetUint64(mustUnsigned(s.Resources.MemoryBytes)), 0)).Div(gib)
	disk := rates["disk_gib_hourly_pt"].Mul(decimal.NewFromBigInt(new(big.Int).SetUint64(mustUnsigned(s.Resources.DiskBytes)), 0)).Div(gib)
	gpu := decimal.Zero
	reasons, e := quoteAvailability(ctx, db, a, s, v)
	if e != nil {
		return nil, e
	}
	if s.GPU.Count > 0 {
		pv, e := resolvePolicies(ctx, db, a, v.PolicyVersions, true)
		if e != nil {
			return nil, e
		}
		p, e := platform.Get(db, "gpu_price_versions", platform.Uint(pv["gpu-pricing"]["source_id"]), false)
		if e != nil {
			return nil, e
		}
		if platform.Text(p["gpu_model"]) != *s.GPU.Model || platform.Text(p["access_mode"]) != s.GPU.AccessMode {
			return nil, invalid("GPU 定价的型号或访问模式与申请不匹配，请重新选择。")
		}
		rate, e := decimal.NewFromString(platform.Text(p["gpu_hourly_pt"]))
		if e != nil {
			return nil, platform.WrapDB(e)
		}
		max, _ := decimal.NewFromString(s.GPU.MaxHourlyPT)
		if rate.GreaterThan(max) {
			reasons = append(reasons, "gpu_price_exceeds_limit")
		}
		gpu = rate.Mul(decimal.NewFromBigInt(new(big.Int).SetUint64(s.GPU.Count), 0))
	}
	return platform.Row{"price_details": platform.Row{"cpu_hourly_pt": cpu.StringFixed(6), "memory_hourly_pt": mem.StringFixed(6), "disk_hourly_pt": disk.StringFixed(6), "gpu_hourly_pt": gpu.StringFixed(6), "total_hourly_pt": cpu.Add(mem).Add(disk).Add(gpu).StringFixed(6)}, "plan_revision": platform.Text(v.Plan["revision"]), "policy_versions": v.PolicyVersions, "expires_at": a.Time().Add(5 * time.Minute).Format(time.RFC3339), "admissible": len(reasons) == 0, "blocking_reasons": reasons, "reservation": false}, nil
}
func quoteAvailability(ctx context.Context, db *gorm.DB, a *platform.App, s CreateElasticSpec, v *ValidatedSpec) ([]string, error) {
	reasons := []string{}
	node, e := platform.Get(db, "nodes", platform.Uint(v.Network["node_id"]), false)
	if e != nil {
		return nil, e
	}
	if platform.Text(node["administrative_state"]) != "active" || platform.Text(node["health"]) != "online" || node["connection_verified_at"] == nil {
		reasons = append(reasons, "node_unavailable")
	}
	policies, e := resolvePolicies(ctx, db, a, v.PolicyVersions, true)
	if e != nil {
		return nil, e
	}
	config := object(policies["gpu-scheduling"]["config"])
	maxAge := platform.Uint(config["metrics_max_age_seconds"])
	if maxAge == 0 {
		return nil, platform.E(409, "policy_not_configured", "库存观测有效期策略未配置，请联系管理员检查 GPU 调度策略。")
	}
	sampled := platform.Stamp(node["resources_sampled_at"])
	if sampled.IsZero() || a.Time().Sub(sampled) > time.Duration(maxAge)*time.Second {
		reasons = append(reasons, "inventory_stale")
	}
	var usage platform.Row
	if e = db.Table("instances").Select("COALESCE(SUM(CASE WHEN desired_state='running' OR observed_state IN ('running','unknown','pending','error') OR provisioning_state IN ('pending','creating','initializing','deleting','unknown') THEN vcpus ELSE 0 END),0) AS vcpus, COALESCE(SUM(CASE WHEN desired_state='running' OR observed_state IN ('running','unknown','pending','error') OR provisioning_state IN ('pending','creating','initializing','deleting','unknown') THEN memory_bytes ELSE 0 END),0) AS memory_bytes, COALESCE(SUM(disk_bytes),0) AS disk_bytes").Where("node_id=? AND deleted_at IS NULL AND NOT (provisioning_state='failed' AND incus_uuid IS NULL AND COALESCE(JSON_EXTRACT(incus_config_snapshot,'$.creation_reservations_released') = true, 0))", node["id"]).Scan(&usage).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	for _, x := range []struct {
		key, capacity string
		want          uint64
	}{{"vcpus", "allocatable_vcpus", s.Resources.VCPUs}, {"memory_bytes", "allocatable_memory_bytes", mustUnsigned(s.Resources.MemoryBytes)}, {"disk_bytes", "allocatable_disk_bytes", mustUnsigned(s.Resources.DiskBytes)}} {
		used, capacity := platform.Uint(usage[x.key]), platform.Uint(node[x.capacity])
		if used > capacity || x.want > capacity-used {
			reasons = append(reasons, "insufficient_"+x.key)
		}
	}
	var count int64
	if e = db.Table("ip_addresses").Where("network_pool_id=? AND status='available'", v.Network["network_pool_id"]).Count(&count).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	if count == 0 {
		reasons = append(reasons, "no_registered_address_available")
	}
	if s.GPU.Count > 0 {
		q := db.Table("node_gpus g").Joins("JOIN gpu_price_versions p ON p.id=g.gpu_price_version_id").Where("g.node_id=? AND g.model=? AND g.status='available' AND g.memory_mib IS NOT NULL AND g.last_seen_at>=? AND p.status='active' AND p.effective_at<=? AND p.access_mode=? AND p.gpu_hourly_pt<=? AND p.id=?", node["id"], *s.GPU.Model, a.Time().Add(-time.Duration(maxAge)*time.Second), a.Time(), s.GPU.AccessMode, s.GPU.MaxHourlyPT, platform.Uint(policies["gpu-pricing"]["source_id"]))
		if s.GPU.AccessMode == "exclusive" {
			q = q.Where("NOT EXISTS (SELECT 1 FROM gpu_allocations ga WHERE ga.gpu_id=g.id AND ga.active_slot=1)")
		} else {
			q = q.Where("g.max_allocations > (SELECT COUNT(*) FROM gpu_allocations ga WHERE ga.gpu_id=g.id AND ga.active_slot=1)")
		}
		var eligible []string
		if e = q.Pluck("g.uuid", &eligible).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		observed := map[string]bool{}
		resource := object(node["resources"])
		cards, _ := resource["gpus"].([]any)
		for _, card := range cards {
			m := object(card)
			if m["utilization_percent"] != nil && m["memory_utilization_percent"] != nil && platform.Text(m["error"]) == "" {
				observed[platform.Text(m["uuid"])] = true
			}
		}
		available := uint64(0)
		for _, uuid := range eligible {
			if observed[uuid] {
				available++
			}
		}
		if available < s.GPU.Count {
			reasons = append(reasons, "insufficient_gpu_capacity_or_metrics")
		}
	}
	return reasons, nil
}
func ValidateInit(ctx context.Context, db *gorm.DB, templateID uint64, params map[string]any) error {
	if params == nil {
		return invalid("init_parameters 必须是 JSON 对象。")
	}
	if e := secretFree(params); e != nil {
		return e
	}
	if templateID == 0 {
		if len(params) > 0 {
			return invalid("未配置初始化模板，不能提交初始化参数。")
		}
		return nil
	}
	r, e := platform.Get(db.WithContext(ctx), "init_template_versions", templateID, false)
	if e != nil {
		return e
	}
	if platform.Text(r["status"]) != "active" {
		return platform.EDetails(422, "validation_failed", "初始化模板未启用，请联系管理员发布有效模板。", platform.Row{"field": "init_parameters", "action": "请使用已启用的初始化模板，或不提交初始化参数。"})
	}
	return validateSchema(object(r["parameter_schema"]), params, "init_parameters", 0)
}
func validateSchema(s map[string]any, v any, path string, depth int) error {
	if depth > 16 {
		return invalid("参数结构嵌套超过允许深度，请简化参数。")
	}
	typ := platform.Text(s["type"])
	switch typ {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return invalid(path + " 必须是对象。")
		}
		props := object(s["properties"])
		if req, ok := s["required"].([]any); ok {
			for _, x := range req {
				if _, ok := m[platform.Text(x)]; !ok {
					return invalid(path + " 缺少必填参数。")
				}
			}
		}
		for k, x := range m {
			ps, ok := props[k]
			if !ok {
				if s["additionalProperties"] != true {
					return invalid(path + " 包含未知参数：" + k)
				}
				continue
			}
			if e := validateSchema(object(ps), x, path+"."+k, depth+1); e != nil {
				return e
			}
		}
	case "string":
		x, ok := v.(string)
		if !ok {
			return invalid(path + " 必须是字符串。")
		}
		if n := platform.Uint(s["maxLength"]); n > 0 && uint64(len([]rune(x))) > n {
			return invalid(path + " 长度超过允许上限。")
		}
		if uint64(len([]rune(x))) < platform.Uint(s["minLength"]) {
			return invalid(path + " 长度低于允许下限。")
		}
	case "integer", "number":
		d, e := decimal.NewFromString(fmt.Sprint(v))
		if e != nil {
			return invalid(path + " 必须是数值。")
		}
		if typ == "integer" && !d.Equal(d.Truncate(0)) {
			return invalid(path + " 必须是整数。")
		}
		if n, ok := s["minimum"]; ok {
			b, e := decimal.NewFromString(fmt.Sprint(n))
			if e != nil || d.LessThan(b) {
				return invalid(path + " 低于允许的最小值。")
			}
		}
		if n, ok := s["maximum"]; ok {
			b, e := decimal.NewFromString(fmt.Sprint(n))
			if e != nil || d.GreaterThan(b) {
				return invalid(path + " 超过允许的最大值。")
			}
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return invalid(path + " 必须是布尔值。")
		}
	case "array":
		xs, ok := v.([]any)
		if !ok {
			return invalid(path + " 必须是数组。")
		}
		if max := platform.Uint(s["maxItems"]); max > 0 && uint64(len(xs)) > max {
			return invalid(path + " 数组元素数量超过上限。")
		}
		for _, x := range xs {
			if e := validateSchema(object(s["items"]), x, path+"[]", depth+1); e != nil {
				return e
			}
		}
	default:
		return invalid("初始化模板包含不支持的参数类型，请联系管理员修正模板。")
	}
	if es, ok := s["enum"].([]any); ok {
		found := false
		b, _ := json.Marshal(v)
		for _, x := range es {
			xb, _ := json.Marshal(x)
			if string(b) == string(xb) {
				found = true
				break
			}
		}
		if !found {
			return invalid(path + " 不是允许的选项值。")
		}
	}
	return nil
}
func quoteUserEligibility(ctx context.Context, db *gorm.DB, a *platform.App, userID uint64, s CreateElasticSpec, q platform.Row) error {
	reasons, _ := q["blocking_reasons"].([]string)
	var limits []platform.Row
	if e := db.Table("user_limits").Where("user_id=?", userID).Find(&limits).Error; e != nil {
		return platform.WrapDB(e)
	}
	if len(limits) != 1 {
		reasons = append(reasons, "quota_not_configured")
	} else {
		var used platform.Row
		if e := db.Table("instances").Select("COUNT(*) AS instances,COALESCE(SUM(vcpus),0) AS vcpus,COALESCE(SUM(memory_bytes),0) AS memory_bytes,COALESCE(SUM(disk_bytes),0) AS disk_bytes").Where("user_id=? AND deleted_at IS NULL AND NOT (provisioning_state='failed' AND incus_uuid IS NULL AND COALESCE(JSON_EXTRACT(incus_config_snapshot,'$.creation_reservations_released') = true, 0))", userID).Scan(&used).Error; e != nil {
			return platform.WrapDB(e)
		}
		var gpus int64
		if e := db.Table("gpu_allocations").Where("user_id=? AND active_slot=1", userID).Count(&gpus).Error; e != nil {
			return platform.WrapDB(e)
		}
		used["gpus"] = gpus
		for _, v := range []struct {
			key, limit string
			request    uint64
		}{{"instances", "max_instances", 1}, {"vcpus", "max_vcpus", s.Resources.VCPUs}, {"memory_bytes", "max_memory_bytes", mustUnsigned(s.Resources.MemoryBytes)}, {"disk_bytes", "max_disk_bytes", mustUnsigned(s.Resources.DiskBytes)}, {"gpus", "max_gpu_allocations", s.GPU.Count}} {
			n, max := platform.Uint(used[v.key]), platform.Uint(limits[0][v.limit])
			if n > max || v.request > max-n {
				reasons = append(reasons, "quota_exceeded_"+v.key)
			}
		}
	}
	var accounts []platform.Row
	if e := db.Table("credit_accounts").Where("user_id=?", userID).Find(&accounts).Error; e != nil {
		return platform.WrapDB(e)
	}
	if len(accounts) != 1 || platform.Text(accounts[0]["status"]) != "active" {
		reasons = append(reasons, "credit_account_unavailable")
	} else {
		versions, _ := q["policy_versions"].(map[string]string)
		policies, e := resolvePolicies(ctx, db, a, versions, true)
		if e != nil {
			return e
		}
		cfg := object(policies["credit"]["config"])
		seconds := platform.Uint(cfg["billing_window_seconds"])
		if seconds == 0 {
			return platform.E(409, "policy_not_configured", "信用计费窗口未配置，请联系管理员检查信用策略。")
		}
		prices := object(q["price_details"])
		total, e := decimal.NewFromString(platform.Text(prices["total_hourly_pt"]))
		if e != nil {
			return e
		}
		if platform.Text(cfg["storage_charge_mode"]) == "free" {
			d, e := decimal.NewFromString(platform.Text(prices["disk_hourly_pt"]))
			if e != nil {
				return e
			}
			total = total.Sub(d)
		}
		required := total.Mul(decimal.NewFromBigInt(new(big.Int).SetUint64(seconds), 0)).Div(decimal.NewFromInt(3600)).RoundCeil(6)
		var available struct {
			Amount string `gorm:"column:amount"`
		}
		if e = db.Table("credit_buckets").Select("COALESCE(SUM(remaining_pt-reserved_pt),0) AS amount").Where("account_id=? AND valid_from<=? AND (expires_at IS NULL OR expires_at>?)", accounts[0]["id"], a.Time(), a.Time()).Scan(&available).Error; e != nil {
			return platform.WrapDB(e)
		}
		balance, e := decimal.NewFromString(available.Amount)
		if e != nil {
			return e
		}
		if balance.LessThan(required) {
			reasons = append(reasons, "insufficient_credit")
		}
		q["initial_window_pt"] = required.StringFixed(6)
	}
	q["blocking_reasons"] = reasons
	q["admissible"] = len(reasons) == 0
	return nil
}
