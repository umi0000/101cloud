package instances

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/catalog"
	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

type customConfigFields struct {
	InstanceType          string         `json:"instance_type"`
	ImageID               string         `json:"image_id"`
	Resources             Resources      `json:"resources"`
	NetworkOptionID       string         `json:"network_option_id"`
	InitParameters        map[string]any `json:"init_parameters"`
	InitTemplateVersionID string         `json:"init_template_version_id,omitempty"`
}
type customGPUConfig struct {
	Count       *int    `json:"count"`
	Model       *string `json:"model"`
	AccessMode  string  `json:"access_mode"`
	MaxHourlyPT string  `json:"max_hourly_pt"`
}
type customProvisioningGPU struct {
	customGPUConfig
	FixedUUIDs []string `json:"fixed_uuids,omitempty"`
}
type customProvisioningConfig struct {
	customConfigFields
	GPU                customProvisioningGPU `json:"gpu"`
	NodeID             string                `json:"node_id,omitempty"`
	AllowedUserActions []string              `json:"allowed_user_actions"`
	GPUUserPermissions map[string]bool       `json:"gpu_user_permissions"`
}

var customActions = map[string]bool{"start": true, "stop": true, "restart": true, "resize": true, "delete": true, "gpu_attach": true, "gpu_detach": true, "gpu_rebind": true, "gpu_policy": true, "console": true}
var customGPUPermissions = map[string]bool{"idle": true, "idle_timeout": true, "idle_action": true, "rebalance": true, "allow_live": true, "max_hourly_pt": true}
var maximumPolicyPrice = decimal.RequireFromString("99999999999999.999999")

func validCustomText(s string, max int) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= max
}
func policyMoney(s string) (decimal.Decimal, error) {
	// Prices are exact DECIMAL(20,6), not floats or exponent notation.
	if s == "" || strings.TrimSpace(s) != s {
		return decimal.Zero, bad("price must be a nonnegative decimal string")
	}
	dot := false
	fraction := 0
	digits := 0
	for _, ch := range s {
		if ch == '.' && !dot {
			dot = true
			continue
		}
		if ch < '0' || ch > '9' {
			return decimal.Zero, bad("price must be a nonnegative decimal string")
		}
		digits++
		if dot {
			fraction++
		}
	}
	if digits == 0 || fraction > 6 || strings.HasSuffix(s, ".") {
		return decimal.Zero, bad("price must have at most six fractional digits")
	}
	n, e := decimal.NewFromString(s)
	if e != nil || n.IsNegative() || n.GreaterThan(maximumPolicyPrice) {
		return decimal.Zero, bad("price exceeds DECIMAL(20,6) range")
	}
	return n, nil
}
func validateCustomPermissions(actions []string, permissions map[string]bool) error {
	seen := make(map[string]bool, len(actions))
	for _, action := range actions {
		if !customActions[action] || seen[action] {
			return bad("allowed_user_actions contains an unknown or duplicate action")
		}
		seen[action] = true
	}
	for k := range permissions {
		if !customGPUPermissions[k] {
			return bad("gpu_user_permissions contains an unknown permission")
		}
	}
	return nil
}
func validateCustomFields(c *gin.Context, a *platform.App, f customConfigFields, g customGPUConfig, nodeID string) error {
	if f.InstanceType != "container" && f.InstanceType != "virtual-machine" {
		return bad("instance_type must be container or virtual-machine")
	}
	if _, _, e := validResources(f.Resources); e != nil {
		return e
	}
	if nodeID == "" {
		return bad("approved_config.node_id is required")
	}
	node, e := platform.IDString(nodeID)
	if e != nil {
		return e
	}
	image, e := platform.IDString(f.ImageID)
	if e != nil {
		return e
	}
	network, e := platform.IDString(f.NetworkOptionID)
	if e != nil {
		return e
	}
	if g.Count == nil || *g.Count < 0 || uint64(*g.Count) > 4294967295 {
		return bad("gpu.count must be a nonnegative integer")
	}
	if f.InstanceType == "virtual-machine" && *g.Count != 0 {
		return bad("virtual machines cannot have GPUs")
	}
	if g.AccessMode != "shared" && g.AccessMode != "exclusive" {
		return bad("gpu.access_mode must be shared or exclusive")
	}
	if _, e = policyMoney(g.MaxHourlyPT); e != nil {
		return e
	}
	if (*g.Count == 0 && g.Model != nil) || (*g.Count > 0 && (g.Model == nil || !validCustomText(*g.Model, 255))) {
		return bad("gpu.model must be null for zero GPUs and a model string otherwise")
	}
	db := platform.DB(c, a)
	nr, e := platform.Get(db, "nodes", node, false)
	if e != nil {
		return e
	}
	ir, e := platform.Get(db, "images", image, false)
	if e != nil {
		return e
	}
	binding, e := catalog.ValidateNetworkOption(db, network, f.InstanceType)
	if e != nil {
		return e
	}
	if platform.Uint(binding["node_id"]) != node {
		return bad("节点与网络选项不匹配，请重新选择。")
	}
	ir, e = catalog.ResolveNodeImage(c.Request.Context(), db, ir, node, platform.Text(binding["project_name"]))
	if e != nil {
		return e
	}
	if platform.Text(ir["status"]) != "active" || platform.Text(ir["image_type"]) != f.InstanceType || (*g.Count > 0 && !platform.Bool(ir["gpu_ready"])) {
		return bad("image is not compatible with the requested instance")
	}
	if arch := platform.Text(object(nr["resources"])["architecture"]); arch != "" && arch != platform.Text(ir["architecture"]) {
		return bad("节点架构与镜像不兼容，请选择相同架构的镜像或节点。")
	}
	var templateID uint64
	if f.InitTemplateVersionID != "" {
		templateID, e = platform.IDString(f.InitTemplateVersionID)
		if e != nil {
			return e
		}
	}
	return catalog.ValidateInit(c.Request.Context(), db, templateID, f.InitParameters)
}
func customProvisioningSpec(name string, cfg customProvisioningConfig) Spec {
	model := ""
	if cfg.GPU.Model != nil {
		model = *cfg.GPU.Model
	}
	actions := cfg.AllowedUserActions
	if actions == nil {
		actions = []string{}
	}
	permissions := cfg.GPUUserPermissions
	if permissions == nil {
		permissions = map[string]bool{}
	}
	return Spec{Name: name, InstanceType: cfg.InstanceType, ImageID: cfg.ImageID, NetworkOptionID: cfg.NetworkOptionID, NodeID: cfg.NodeID, Resources: cfg.Resources, GPU: GPURequest{Count: *cfg.GPU.Count, Model: model, AccessMode: cfg.GPU.AccessMode, MaxHourlyPT: cfg.GPU.MaxHourlyPT, FixedUUIDs: cfg.GPU.FixedUUIDs}, InitParameters: cfg.InitParameters, InitTemplateVersionID: cfg.InitTemplateVersionID, AllowedUserActions: actions, GPUUserPermissions: permissions}
}
func validateCustomProvisioning(c *gin.Context, a *platform.App, cfg customProvisioningConfig) error {
	if e := validateCustomFields(c, a, cfg.customConfigFields, cfg.GPU.customGPUConfig, cfg.NodeID); e != nil {
		return e
	}
	if e := validateCustomPermissions(cfg.AllowedUserActions, cfg.GPUUserPermissions); e != nil {
		return e
	}
	if cfg.GPU.FixedUUIDs != nil && len(cfg.GPU.FixedUUIDs) != *cfg.GPU.Count {
		return bad("fixed_uuids length must equal gpu.count")
	}
	seen := make(map[string]bool, len(cfg.GPU.FixedUUIDs))
	for _, id := range cfg.GPU.FixedUUIDs {
		if !validCustomText(id, 128) || strings.TrimSpace(id) != id || seen[id] {
			return bad("fixed_uuids must contain unique GPU UUIDs")
		}
		seen[id] = true
	}
	return nil
}
func activeOwner(c *gin.Context, a *platform.App, s string) (uint64, error) {
	id, e := platform.IDString(s)
	if e != nil {
		return 0, e
	}
	r, e := platform.Get(platform.DB(c, a), "users", id, true)
	if e != nil {
		return 0, e
	}
	if platform.Text(r["status"]) != "active" {
		return 0, conflict("owner_inactive", "instance owner must be active")
	}
	return id, nil
}

func registerProvisioning(a *platform.App, user, admin *gin.RouterGroup) {
	for _, g := range []*gin.RouterGroup{user, admin} {
		g.GET("/instances/:id/gpu-policy", platform.Handle(func(c *gin.Context) (any, error) {
			r, e := owned(c, a)
			if e != nil {
				return nil, e
			}
			return gpuPolicyResponse(c, a, r)
		}))
		g.PUT("/instances/:id/gpu-policy", platform.Handle(func(c *gin.Context) (any, error) { return putGPUPolicy(c, a) }))
	}
	admin.PUT("/instances/:id/gpu-pricing-policy", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := owned(c, a)
		if e != nil {
			return nil, e
		}
		if e = platform.Revision(c, r); e != nil {
			return nil, e
		}
		versions := map[string]string{}
		for kind, value := range object(r["policy_versions"]) {
			versions[kind] = platform.Text(value)
		}
		if platform.Text(r["product_type"]) != "custom" || versions["gpu-pricing"] != "" {
			return nil, conflict("policy_binding_frozen", "Only custom instances without a GPU pricing binding can be configured.")
		}
		var count int64
		if e = platform.DB(c, a).Table("gpu_allocations").Where("instance_id=?", r["id"]).Count(&count).Error; e != nil {
			return nil, e
		}
		if count != 0 {
			return nil, conflict("policy_binding_frozen", "Historical GPU allocations prevent changing the instance pricing binding.")
		}
		var input struct {
			PolicyVersionID string `json:"policy_version_id"`
		}
		if e = platform.Decode(c, &input); e != nil {
			return nil, e
		}
		if _, _, e = platform.PolicySource(platform.DB(c, a), "gpu-pricing", input.PolicyVersionID, true); e != nil {
			return nil, e
		}
		versions["gpu-pricing"] = input.PolicyVersionID
		r, e = platform.Save(c, a, "instances", r, platform.Row{"policy_versions": jsonText(versions)})
		if e != nil {
			return nil, e
		}
		if e = platform.Audit(c.Request.Context(), platform.DB(c, a), platform.Current(c).ID, 0, platform.Uint(r["id"]), "instance.gpu_pricing_policy.configured", platform.Row{"policy_version_id": input.PolicyVersionID}); e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		return platform.Row{"policy_versions": versions, "revision": platform.Text(r["revision"])}, nil
	}))
	admin.POST("/custom-instances", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			OwnerUserID    string                   `json:"owner_user_id"`
			Name           string                   `json:"name"`
			ApprovedConfig customProvisioningConfig `json:"approved_config"`
			PolicyVersions map[string]string        `json:"policy_versions"`
		}
		if e := platform.Decode(c, &in); e != nil {
			return nil, e
		}
		if !validCustomText(in.Name, 128) {
			return nil, bad("name must contain 1–128 characters")
		}
		if e := validateCustomProvisioning(c, a, in.ApprovedConfig); e != nil {
			return nil, e
		}
		owner, e := activeOwner(c, a, in.OwnerUserID)
		if e != nil {
			return nil, e
		}
		out, e := createInstance(c.Request.Context(), a, platform.DB(c, a), owner, customProvisioningSpec(in.Name, in.ApprovedConfig), true, in.PolicyVersions)
		if e != nil {
			return nil, e
		}
		taskAccepted(c, out)
		return nil, nil
	}))
	admin.POST("/nodes/:id/instance-imports", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			IncusUUID      string                   `json:"incus_uuid"`
			OwnerUserID    string                   `json:"owner_user_id"`
			ApprovedConfig customProvisioningConfig `json:"approved_config"`
			PolicyVersions map[string]string        `json:"policy_versions"`
		}
		if e := platform.Decode(c, &in); e != nil {
			return nil, e
		}
		if !validIncusUUID(in.IncusUUID) {
			return nil, bad("incus_uuid must be a canonical UUID")
		}
		node, e := platform.ID(c, "id")
		if e != nil {
			return nil, e
		}
		if in.ApprovedConfig.NodeID != "" && in.ApprovedConfig.NodeID != strconv.FormatUint(node, 10) {
			return nil, bad("approved node_id must match the route node")
		}
		in.ApprovedConfig.NodeID = strconv.FormatUint(node, 10)
		if e = validateCustomProvisioning(c, a, in.ApprovedConfig); e != nil {
			return nil, e
		}
		owner, e := activeOwner(c, a, in.OwnerUserID)
		if e != nil {
			return nil, e
		}
		var existing int64
		if e = platform.DB(c, a).Table("instances").Where("node_id = ? AND incus_uuid = ?", node, in.IncusUUID).Count(&existing).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		if existing > 0 {
			return nil, conflict("instance_already_managed", "Incus UUID is already managed")
		}
		// Discovery and exact native identity/config verification happen in the worker,
		// before reserveImported adopts and reserves actual IP/GPU inventory atomically.
		spec := customProvisioningSpec("Imported "+in.IncusUUID, in.ApprovedConfig)
		if e = validateCustomVersions(c.Request.Context(), a, platform.DB(c, a), in.PolicyVersions, spec); e != nil {
			return nil, e
		}
		out, e := platform.Enqueue(platform.DB(c, a), "instance.import", owner, node, 0, false, platform.Row{"owner_user_id": strconv.FormatUint(owner, 10), "incus_uuid": in.IncusUUID, "approved_config": spec, "policy_versions": in.PolicyVersions})
		if e != nil {
			return nil, e
		}
		taskAccepted(c, out)
		return nil, nil
	}))
}
func taskAccepted(c *gin.Context, out platform.Row) {
	prefix := "/api/v1/tasks/"
	if c.GetBool("admin") {
		prefix = "/api/v1/admin/tasks/"
	}
	c.Header("Location", prefix+platform.Text(out["task_id"]))
	platform.Reply(c, http.StatusAccepted, out)
}

func validIncusUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, ch := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if ch != '-' {
				return false
			}
			continue
		}
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

type storedIdlePolicy struct {
	Enabled        bool   `json:"enabled"`
	TimeoutSeconds uint64 `json:"timeout_seconds"`
	Action         string `json:"action"`
}
type storedRebalancePolicy struct {
	Enabled     bool   `json:"enabled"`
	AllowLive   bool   `json:"allow_live"`
	MaxHourlyPT string `json:"max_hourly_pt"`
}
type instanceGPUPolicy struct {
	Idle      storedIdlePolicy      `json:"idle"`
	Rebalance storedRebalancePolicy `json:"rebalance"`
}
type idlePolicyInput struct {
	Enabled        *bool   `json:"enabled"`
	TimeoutSeconds *uint64 `json:"timeout_seconds"`
	Action         string  `json:"action"`
}
type rebalancePolicyInput struct {
	Enabled     *bool  `json:"enabled"`
	AllowLive   *bool  `json:"allow_live"`
	MaxHourlyPT string `json:"max_hourly_pt"`
}
type gpuPolicyInput struct {
	Idle      *idlePolicyInput      `json:"idle"`
	Rebalance *rebalancePolicyInput `json:"rebalance"`
}
type adminGPUPolicyInput struct {
	gpuPolicyInput
	AllowedUserActions *[]string        `json:"allowed_user_actions"`
	GPUUserPermissions *map[string]bool `json:"gpu_user_permissions"`
}
type gpuPolicyBounds struct {
	Idle       billing.IdleConfig
	Scheduling billing.SchedulingConfig
	Versions   map[string]string
	Binding    platform.Row
}

func policyBounds(c *gin.Context, a *platform.App, instance platform.Row) (gpuPolicyBounds, error) {
	out := gpuPolicyBounds{Versions: map[string]string{}}
	for _, kind := range []string{"gpu-idle", "gpu-scheduling"} {
		policy, e := a.InstancePolicy(c.Request.Context(), instance, kind)
		if e != nil {
			return out, e
		}
		out.Versions[kind] = platform.Text(object(instance["policy_versions"])[kind])
		var dst any
		if kind == "gpu-idle" {
			dst = &out.Idle
		} else {
			dst = &out.Scheduling
		}
		if e = json.Unmarshal([]byte(jsonText(policy)), dst); e != nil {
			return out, platform.E(500, "invalid_stored_policy", "stored GPU policy is invalid")
		}
	}
	if out.Idle.Min <= 0 || out.Idle.Max < out.Idle.Min || out.Idle.Default < out.Idle.Min || out.Idle.Default > out.Idle.Max || len(out.Idle.Actions) == 0 {
		return out, platform.E(500, "invalid_stored_policy", "stored GPU idle bounds are invalid")
	}
	var e error
	out.Binding, e = gpuPolicyBinding(c, a, instance)
	return out, e
}

// gpuPolicyBinding exposes only the instance's explicit pricing source, even
// before its first allocation. Never infer a binding from its current plan.
func gpuPolicyBinding(c *gin.Context, a *platform.App, instance platform.Row) (platform.Row, error) {
	version, bound := object(instance["policy_versions"])["gpu-pricing"]
	if !bound {
		return nil, nil
	}
	db := platform.DB(c, a)
	policy, source, e := platform.PolicySource(db, "gpu-pricing", platform.Text(version), false)
	if e != nil {
		return nil, e
	}
	price, e := platform.Get(db, "gpu_price_versions", source, false)
	if e != nil {
		if he, ok := e.(*platform.HTTPError); ok && he.Status == http.StatusNotFound {
			return nil, conflict("policy_not_configured", "instance-bound GPU pricing source is missing")
		}
		return nil, e
	}
	model, access := platform.Text(price["gpu_model"]), platform.Text(price["access_mode"])
	if strings.TrimSpace(model) == "" || access != "shared" && access != "exclusive" {
		return nil, platform.E(500, "invalid_stored_policy", "实例绑定的 GPU 定价型号或访问模式无效。")
	}
	rate, e := policyMoney(platform.Text(price["gpu_hourly_pt"]))
	if e != nil {
		return nil, platform.E(500, "invalid_stored_policy", "实例绑定的 GPU 价格无效。")
	}
	return platform.Row{"policy_version_id": strconv.FormatUint(platform.Uint(policy["id"]), 10), "model": model, "access_mode": access, "gpu_hourly_pt": rate.StringFixed(6)}, nil
}
func policyConfiguration(r platform.Row, b gpuPolicyBounds) (instanceGPUPolicy, error) {
	cfg := instanceGPUPolicy{Idle: storedIdlePolicy{TimeoutSeconds: uint64(b.Idle.Default), Action: "detach_gpu"}, Rebalance: storedRebalancePolicy{MaxHourlyPT: platform.Text(r["max_gpu_hourly_pt"])}}
	if len(b.Idle.Actions) > 0 {
		cfg.Idle.Action = b.Idle.Actions[0]
		if cfg.Idle.Action == "release_gpu" {
			cfg.Idle.Action = "detach_gpu"
		}
	}
	raw := platform.Text(r["gpu_policy"])
	if raw != "" && raw != "{}" && raw != "null" {
		if e := json.Unmarshal([]byte(raw), &cfg); e != nil {
			return cfg, platform.E(500, "invalid_stored_policy", "stored instance GPU policy is invalid")
		}
	}
	if cfg.Rebalance.MaxHourlyPT == "" {
		cfg.Rebalance.MaxHourlyPT = "0.000000"
	}
	return cfg, nil
}
func containsPolicyAction(actions []string, action string) bool {
	for _, v := range actions {
		if v == action || (v == "release_gpu" && action == "detach_gpu") {
			return true
		}
	}
	return false
}
func gpuPolicyResponse(c *gin.Context, a *platform.App, r platform.Row) (any, error) {
	b, e := policyBounds(c, a, r)
	if e != nil {
		return nil, e
	}
	return gpuPolicyDTO(c, a, r, b)
}
func gpuPolicyDTO(c *gin.Context, a *platform.App, r platform.Row, b gpuPolicyBounds) (platform.Row, error) {
	cfg, e := policyConfiguration(r, b)
	if e != nil {
		return nil, e
	}
	effective := cfg
	blocked := []string{}
	if !b.Idle.Enabled {
		effective.Idle.Enabled = false
		blocked = append(blocked, "bound_idle_disabled")
	}
	if cfg.Idle.TimeoutSeconds < uint64(b.Idle.Min) || cfg.Idle.TimeoutSeconds > uint64(b.Idle.Max) {
		effective.Idle.Enabled = false
		blocked = append(blocked, "idle_timeout_outside_current_range")
	}
	if !containsPolicyAction(b.Idle.Actions, cfg.Idle.Action) {
		effective.Idle.Enabled = false
		blocked = append(blocked, "idle_action_not_allowed")
	}
	if platform.Text(r["image_type"]) != "container" {
		effective.Idle.Enabled = false
		effective.Rebalance.Enabled = false
		blocked = append(blocked, "gpu_requires_container")
	}
	if platform.Text(r["desired_state"]) == "deleted" || platform.Text(r["provisioning_state"]) != "ready" {
		effective.Idle.Enabled = false
		effective.Rebalance.Enabled = false
		blocked = append(blocked, "instance_not_ready")
	}
	var enforcement int64
	if platform.Text(r["billing_mode"]) == "credit" {
		if e = platform.DB(c, a).Table("credit_enforcements").Where("user_id = ? AND status IN ('grace','stopping','unknown')", r["user_id"]).Count(&enforcement).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		if enforcement > 0 {
			effective.Rebalance.Enabled = false
			blocked = append(blocked, "credit_enforcement_active")
		}
	}
	editable := map[string]bool{}
	for key := range customGPUPermissions {
		editable[key] = true
	}
	if !c.GetBool("admin") {
		permissions := object(r["gpu_user_permissions"])
		canChange := false
		for _, v := range rows(r["allowed_user_actions"]) {
			if platform.Text(v) == "gpu_policy" {
				canChange = true
			}
		}
		for key := range editable {
			editable[key] = canChange && platform.Bool(permissions[key])
		}
	}
	actions := make([]string, 0, len(b.Idle.Actions))
	for _, action := range b.Idle.Actions {
		if action == "release_gpu" {
			action = "detach_gpu"
		}
		actions = append(actions, action)
	}
	out := platform.Row{"idle": cfg.Idle, "rebalance": cfg.Rebalance, "effective_policy": platform.Row{"idle": effective.Idle, "rebalance": effective.Rebalance, "safety": platform.Row{"complete_fresh_ownership_required": true, "gpu_processes_must_be_zero": true, "gpu_memory_must_be_zero": true, "device_references_must_be_zero": true, "metrics_max_age_seconds": b.Scheduling.MaxAge, "cooldown_seconds": b.Scheduling.Cooldown, "graceful_stop_timeout_seconds": b.Idle.StopTimeout, "allow_force_stop": b.Idle.Force, "allow_price_increase": b.Scheduling.AllowIncrease}}, "allowed_ranges": platform.Row{"idle": platform.Row{"enabled": b.Idle.Enabled, "default_timeout_seconds": b.Idle.Default, "min_timeout_seconds": b.Idle.Min, "max_timeout_seconds": b.Idle.Max, "allowed_actions": actions}, "rebalance": platform.Row{"min_hourly_pt": "0.000000", "max_hourly_pt": "99999999999999.999999", "allow_price_increase": b.Scheduling.AllowIncrease}, "editable": editable}, "policy_versions": b.Versions, "revision": strconv.FormatUint(platform.Uint(r["revision"]), 10), "idle_since": nil, "last_rebound_at": nil, "blocked_reasons": blocked, "allowed_user_actions": rows(r["allowed_user_actions"]), "gpu_user_permissions": object(r["gpu_user_permissions"])}
	out["gpu_binding"] = b.Binding
	for _, key := range []string{"idle_since", "last_rebound_at"} {
		if !platform.Stamp(r[key]).IsZero() {
			out[key] = platform.Stamp(r[key]).In(a.Location).Format(time.RFC3339Nano)
		}
	}
	platform.SetETag(c, r)
	return out, nil
}
func putGPUPolicy(c *gin.Context, a *platform.App) (any, error) {
	r, e := owned(c, a)
	if e != nil {
		return nil, e
	}
	if e = platform.Revision(c, r); e != nil {
		return nil, e
	}
	var input gpuPolicyInput
	var adminInput adminGPUPolicyInput
	if c.GetBool("admin") {
		if e = platform.Decode(c, &adminInput); e != nil {
			return nil, e
		}
		input = adminInput.gpuPolicyInput
	} else {
		if e = permitted(c, r, "gpu_policy"); e != nil {
			return nil, e
		}
		if e = platform.Decode(c, &input); e != nil {
			return nil, e
		}
	}
	if input.Idle == nil || input.Rebalance == nil || input.Idle.Enabled == nil || input.Idle.TimeoutSeconds == nil || input.Rebalance.Enabled == nil || input.Rebalance.AllowLive == nil {
		return nil, bad("idle and rebalance must contain every configuration field")
	}
	b, e := policyBounds(c, a, r)
	if e != nil {
		return nil, e
	}
	cfg := instanceGPUPolicy{Idle: storedIdlePolicy{Enabled: *input.Idle.Enabled, TimeoutSeconds: *input.Idle.TimeoutSeconds, Action: input.Idle.Action}, Rebalance: storedRebalancePolicy{Enabled: *input.Rebalance.Enabled, AllowLive: *input.Rebalance.AllowLive, MaxHourlyPT: input.Rebalance.MaxHourlyPT}}
	if cfg.Idle.TimeoutSeconds < uint64(b.Idle.Min) || cfg.Idle.TimeoutSeconds > uint64(b.Idle.Max) {
		return nil, bad("idle.timeout_seconds is outside the instance-bound policy range")
	}
	if cfg.Idle.Action != "detach_gpu" && cfg.Idle.Action != "stop_instance" {
		return nil, bad("idle.action must be detach_gpu or stop_instance")
	}
	if !containsPolicyAction(b.Idle.Actions, cfg.Idle.Action) {
		return nil, platform.E(403, "idle_action_not_allowed", "idle action is not allowed by the instance-bound policy")
	}
	if cfg.Idle.Enabled && !b.Idle.Enabled {
		return nil, platform.E(403, "idle_policy_disabled", "instance-bound GPU idle actions are disabled")
	}
	price, e := policyMoney(cfg.Rebalance.MaxHourlyPT)
	if e != nil {
		return nil, e
	}
	cfg.Rebalance.MaxHourlyPT = price.StringFixed(6)
	if platform.Text(r["image_type"]) != "container" && (cfg.Idle.Enabled || cfg.Rebalance.Enabled) {
		return nil, conflict("unsupported_configuration", "GPU policies require a container")
	}
	previous, e := policyConfiguration(r, b)
	if e != nil {
		return nil, e
	}
	if !c.GetBool("admin") {
		permissions := object(r["gpu_user_permissions"])
		for key, changed := range map[string]bool{"idle": cfg.Idle.Enabled != previous.Idle.Enabled, "idle_timeout": cfg.Idle.TimeoutSeconds != previous.Idle.TimeoutSeconds, "idle_action": cfg.Idle.Action != previous.Idle.Action, "rebalance": cfg.Rebalance.Enabled != previous.Rebalance.Enabled, "allow_live": cfg.Rebalance.AllowLive != previous.Rebalance.AllowLive, "max_hourly_pt": !price.Equal(decimalValue(previous.Rebalance.MaxHourlyPT))} {
			if changed && !platform.Bool(permissions[key]) {
				return nil, platform.E(403, "gpu_policy_not_authorized", "administrator has not authorized changing "+key)
			}
		}
	}
	changes := platform.Row{"gpu_policy": jsonText(cfg), "auto_release_gpu": cfg.Idle.Enabled && cfg.Idle.Action == "detach_gpu", "auto_rebind_gpu": cfg.Rebalance.Enabled, "max_gpu_hourly_pt": cfg.Rebalance.MaxHourlyPT}
	if cfg.Idle != previous.Idle {
		changes["idle_since"] = nil
	}
	if c.GetBool("admin") {
		actions := []string{}
		for _, v := range rows(r["allowed_user_actions"]) {
			actions = append(actions, platform.Text(v))
		}
		permissions := map[string]bool{}
		for k, v := range object(r["gpu_user_permissions"]) {
			permissions[k] = platform.Bool(v)
		}
		if adminInput.AllowedUserActions != nil {
			if *adminInput.AllowedUserActions == nil {
				return nil, bad("allowed_user_actions must be an array")
			}
			actions = *adminInput.AllowedUserActions
			changes["allowed_user_actions"] = jsonText(actions)
		}
		if adminInput.GPUUserPermissions != nil {
			if *adminInput.GPUUserPermissions == nil {
				return nil, bad("gpu_user_permissions must be an object")
			}
			permissions = *adminInput.GPUUserPermissions
			changes["gpu_user_permissions"] = jsonText(permissions)
		}
		if e = validateCustomPermissions(actions, permissions); e != nil {
			return nil, e
		}
	}
	r, e = platform.Save(c, a, "instances", r, changes)
	if e != nil {
		return nil, e
	}
	if e = platform.Audit(c.Request.Context(), platform.DB(c, a), platform.Current(c).ID, 0, platform.Uint(r["id"]), "instance.gpu_policy.updated", platform.Row{"before": previous, "after": cfg, "allowed_user_actions": rows(r["allowed_user_actions"]), "gpu_user_permissions": object(r["gpu_user_permissions"]), "policy_versions": b.Versions}); e != nil {
		return nil, e
	}
	return gpuPolicyDTO(c, a, r, b)
}
func decimalValue(s string) decimal.Decimal { v, _ := decimal.NewFromString(s); return v }
