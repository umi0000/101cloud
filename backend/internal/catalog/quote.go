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
	ZoneID          string         `json:"zone_id"`
	PlanID          string         `json:"plan_id"`
	ImageID         string         `json:"image_id"`
	NetworkOptionID string         `json:"network_option_id"`
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
		return invalid("vcpus must be a positive uint32")
	}
	if _, e := unsigned(r.MemoryBytes, true); e != nil {
		return e
	}
	_, e := unsigned(r.DiskBytes, true)
	return e
}

// validGPUTerms checks the GPU fields shared by plan offerings and requests.
func validGPUTerms(g GPURequest) error {
	if !enum(g.AccessMode, "shared", "exclusive") {
		return invalid("gpu.access_mode is required")
	}
	n, e := decimal.NewFromString(g.MaxHourlyPT)
	if e != nil || n.IsNegative() || n.Exponent() < -6 {
		return invalid("gpu.max_hourly_pt must be a nonnegative decimal with up to six fractional digits")
	}
	if g.Count > 4294967295 {
		return invalid("gpu.count must be a uint32")
	}
	if g.Model != nil && !textValid(*g.Model, 255) {
		return invalid("gpu.model must contain 1–255 characters")
	}
	return nil
}

// validGPU checks an elastic request. Count 0 opts out of GPUs and needs no model.
func validGPU(g GPURequest, typ string) error {
	if e := validGPUTerms(g); e != nil {
		return e
	}
	if typ == "virtual-machine" && g.Count != 0 {
		return invalid("VM cannot have GPUs")
	}
	if g.Count == 0 && g.Model != nil {
		return invalid("GPU model must be null when count is zero")
	}
	if g.Count > 0 && g.Model == nil {
		return invalid("GPU model required")
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
			return invalid("VM plans cannot offer GPUs")
		}
		return nil
	}
	if g.Model == nil {
		if g.Count > 0 || overrideMax > 0 {
			return invalid("GPU model required when the plan offers GPUs")
		}
		return nil
	}
	if g.Count == 0 && overrideMax == 0 {
		return invalid("GPU model requires gpu.count > 0 or allowed_overrides.gpu_count.max >= 1")
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
			return nil, platform.E(409, "policy_not_configured", "required policy "+k+" is not configured")
		}
	}
	for k, s := range versions {
		if !enum(k, "pricing", "credit", "gpu-idle", "gpu-scheduling", "gpu-pricing") {
			return nil, invalid("unknown policy version kind")
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
			return nil, missingReference(k)
		}
		if active && (platform.Text(r["status"]) != "active" || platform.Stamp(r["effective_at"]).After(a.Time())) {
			return nil, platform.E(409, "policy_changed", "selected policy is no longer effective")
		}
		out[k] = r
	}
	return out, nil
}
func ValidateElastic(ctx context.Context, db *gorm.DB, a *platform.App, s CreateElasticSpec, versions map[string]string) (*ValidatedSpec, error) {
	if !textValid(s.Name, 128) {
		return nil, invalid("name must contain 1–128 characters")
	}
	if e := validResources(s.Resources); e != nil {
		return nil, e
	}
	pid, e := platform.IDString(s.PlanID)
	if e != nil {
		return nil, e
	}
	plan, e := platform.Get(db, "instance_plans", pid, false)
	if e != nil {
		return nil, e
	}
	if platform.Text(plan["status"]) != "active" {
		return nil, conflict("plan is not published")
	}
	zone, e := platform.IDString(s.ZoneID)
	if e != nil {
		return nil, e
	}
	if zone != platform.Uint(plan["availability_zone_id"]) {
		return nil, invalid("plan does not belong to zone")
	}
	zr, e := platform.Get(db, "availability_zones", zone, false)
	if e != nil {
		return nil, e
	}
	if platform.Text(zr["status"]) != "active" {
		return nil, conflict("zone disabled")
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
		return nil, invalid("image is not allowed by plan")
	}
	ir, e := platform.Get(db, "images", image, false)
	if e != nil {
		return nil, e
	}
	if platform.Text(ir["status"]) != "active" || platform.Text(ir["image_type"]) != typ || (s.GPU.Count > 0 && !platform.Bool(ir["gpu_ready"])) {
		return nil, invalid("incompatible image")
	}
	network, e := platform.IDString(s.NetworkOptionID)
	if e != nil {
		return nil, e
	}
	if e = db.Table("plan_network_options").Where("plan_id = ? AND network_option_id = ?", pid, network).Count(&n).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	if n != 1 {
		return nil, invalid("network option is not allowed by plan")
	}
	nr, e := ValidateNetworkOption(db, network, zone, typ)
	if e != nil {
		return nil, e
	}
	configured := versionMap(plan["policy_versions"])
	if versions != nil {
		if len(versions) != len(configured) {
			return nil, platform.E(409, "policy_changed", "quote policies changed")
		}
		for k, v := range configured {
			if versions[k] != v {
				return nil, platform.E(409, "policy_changed", "quote policies changed")
			}
		}
	}
	policies, e := resolvePolicies(ctx, db, a, configured, true)
	if e != nil {
		return nil, e
	}
	if s.GPU.Count > 0 && policies["gpu-pricing"] == nil {
		return nil, platform.E(409, "policy_not_configured", "GPU pricing policy is not configured")
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
			return invalid("plan does not offer GPUs")
		}
		values["gpu_count"] = g.Count
	}
	for k, v := range values {
		if b := object(bounds[k]); b != nil {
			lo, hi := platform.Uint(b["min"]), platform.Uint(b["max"])
			step := platform.Uint(b["step"])
			if lo > hi || v < lo || v > hi || step == 0 || (v-lo)%step != 0 {
				return invalid("resource outside allowed override: " + k)
			}
		} else if v != platform.Uint(plan[k]) {
			return invalid("resource override is not allowed: " + k)
		}
	}
	if g.Count == 0 {
		return nil
	}
	if g.Model == nil || *g.Model != platform.Text(plan["gpu_model"]) || g.AccessMode != platform.Text(plan["gpu_access_mode"]) {
		return invalid("GPU model/access mode cannot override plan")
	}
	limit, e := decimal.NewFromString(platform.Text(plan["gpu_max_hourly_pt"]))
	if e != nil {
		return invalid("plan GPU price limit missing")
	}
	price, _ := decimal.NewFromString(g.MaxHourlyPT)
	if price.GreaterThan(limit) {
		return invalid("GPU price limit exceeds plan")
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
			return nil, invalid("GPU pricing incompatible with specification")
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
	return platform.Row{"price_details": platform.Row{"cpu_hourly_pt": cpu.StringFixed(6), "memory_hourly_pt": mem.StringFixed(6), "disk_hourly_pt": disk.StringFixed(6), "gpu_hourly_pt": gpu.StringFixed(6), "total_hourly_pt": cpu.Add(mem).Add(disk).Add(gpu).StringFixed(6)}, "policy_versions": v.PolicyVersions, "expires_at": a.Time().Add(5 * time.Minute).Format(time.RFC3339), "admissible": len(reasons) == 0, "blocking_reasons": reasons, "reservation": false}, nil
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
		return nil, platform.E(409, "policy_not_configured", "inventory freshness policy is not configured")
	}
	sampled := platform.Stamp(node["resources_sampled_at"])
	if sampled.IsZero() || a.Time().Sub(sampled) > time.Duration(maxAge)*time.Second {
		reasons = append(reasons, "inventory_stale")
	}
	var usage platform.Row
	if e = db.Table("instances").Select("COALESCE(SUM(CASE WHEN desired_state='running' OR observed_state IN ('running','unknown','pending','error') OR provisioning_state IN ('pending','creating','initializing','deleting','unknown') THEN vcpus ELSE 0 END),0) AS vcpus, COALESCE(SUM(CASE WHEN desired_state='running' OR observed_state IN ('running','unknown','pending','error') OR provisioning_state IN ('pending','creating','initializing','deleting','unknown') THEN memory_bytes ELSE 0 END),0) AS memory_bytes, COALESCE(SUM(disk_bytes),0) AS disk_bytes").Where("node_id=? AND deleted_at IS NULL", node["id"]).Scan(&usage).Error; e != nil {
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
		return invalid("init_parameters must be an object")
	}
	if e := secretFree(params); e != nil {
		return e
	}
	if templateID == 0 {
		if len(params) > 0 {
			return invalid("no initialization template configured")
		}
		return nil
	}
	r, e := platform.Get(db.WithContext(ctx), "init_template_versions", templateID, false)
	if e != nil {
		return e
	}
	if platform.Text(r["status"]) != "active" {
		return missingReference("initialization template")
	}
	return validateSchema(object(r["parameter_schema"]), params, "init_parameters", 0)
}
func validateSchema(s map[string]any, v any, path string, depth int) error {
	if depth > 16 {
		return invalid("schema nesting exceeds limit")
	}
	typ := platform.Text(s["type"])
	switch typ {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return invalid(path + " must be an object")
		}
		props := object(s["properties"])
		if req, ok := s["required"].([]any); ok {
			for _, x := range req {
				if _, ok := m[platform.Text(x)]; !ok {
					return invalid(path + " missing required parameter")
				}
			}
		}
		for k, x := range m {
			ps, ok := props[k]
			if !ok {
				if s["additionalProperties"] != true {
					return invalid(path + " unknown parameter " + k)
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
			return invalid(path + " must be a string")
		}
		if n := platform.Uint(s["maxLength"]); n > 0 && uint64(len([]rune(x))) > n {
			return invalid(path + " too long")
		}
		if uint64(len([]rune(x))) < platform.Uint(s["minLength"]) {
			return invalid(path + " too short")
		}
	case "integer", "number":
		d, e := decimal.NewFromString(fmt.Sprint(v))
		if e != nil {
			return invalid(path + " must be numeric")
		}
		if typ == "integer" && !d.Equal(d.Truncate(0)) {
			return invalid(path + " must be an integer")
		}
		if n, ok := s["minimum"]; ok {
			b, e := decimal.NewFromString(fmt.Sprint(n))
			if e != nil || d.LessThan(b) {
				return invalid(path + " below minimum")
			}
		}
		if n, ok := s["maximum"]; ok {
			b, e := decimal.NewFromString(fmt.Sprint(n))
			if e != nil || d.GreaterThan(b) {
				return invalid(path + " above maximum")
			}
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return invalid(path + " must be boolean")
		}
	case "array":
		xs, ok := v.([]any)
		if !ok {
			return invalid(path + " must be array")
		}
		if max := platform.Uint(s["maxItems"]); max > 0 && uint64(len(xs)) > max {
			return invalid(path + " too many items")
		}
		for _, x := range xs {
			if e := validateSchema(object(s["items"]), x, path+"[]", depth+1); e != nil {
				return e
			}
		}
	default:
		return invalid("unsupported template schema type")
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
			return invalid(path + " is not an allowed value")
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
		if e := db.Table("instances").Select("COUNT(*) AS instances,COALESCE(SUM(vcpus),0) AS vcpus,COALESCE(SUM(memory_bytes),0) AS memory_bytes,COALESCE(SUM(disk_bytes),0) AS disk_bytes").Where("user_id=? AND deleted_at IS NULL", userID).Scan(&used).Error; e != nil {
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
			return platform.E(409, "policy_not_configured", "credit billing window missing")
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
