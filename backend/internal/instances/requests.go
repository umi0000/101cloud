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
	"gorm.io/gorm"
)

// Separate submission and approval types keep administrative fields out of the
// user decoder, including fields whose zero value would otherwise hide misuse.
type customConfigFields struct {
	InstanceType          string         `json:"instance_type"`
	ZoneID                string         `json:"zone_id"`
	ImageID               string         `json:"image_id"`
	Resources             Resources      `json:"resources"`
	NetworkOptionID       string         `json:"network_option_id"`
	InitParameters        map[string]any `json:"init_parameters"`
	InitTemplateVersionID string         `json:"init_template_version_id,omitempty"`
}
type customRequestGPU struct {
	Count       *int    `json:"count"`
	Model       *string `json:"model"`
	AccessMode  string  `json:"access_mode"`
	MaxHourlyPT string  `json:"max_hourly_pt"`
}
type customRequestConfig struct {
	customConfigFields
	GPU customRequestGPU `json:"gpu"`
}
type customApprovedGPU struct {
	customRequestGPU
	FixedUUIDs []string `json:"fixed_uuids,omitempty"`
}
type customApprovedConfig struct {
	customConfigFields
	GPU                customApprovedGPU `json:"gpu"`
	NodeID             string            `json:"node_id,omitempty"`
	AllowedUserActions []string          `json:"allowed_user_actions"`
	GPUUserPermissions map[string]bool   `json:"gpu_user_permissions"`
}

var customActions = map[string]bool{"start": true, "stop": true, "restart": true, "resize": true, "delete": true, "gpu_attach": true, "gpu_detach": true, "gpu_rebind": true, "gpu_policy": true, "console": true}
var customGPUPermissions = map[string]bool{"idle": true, "idle_timeout": true, "idle_action": true, "rebalance": true, "allow_live": true, "max_hourly_pt": true}
var maximumPolicyPrice = decimal.RequireFromString("99999999999999.999999")

func requestText(s string, max int) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= max
}
func requestReason(s string) error {
	if !requestText(s, 4000) {
		return bad("reason must contain 1–4000 characters")
	}
	return nil
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
func validateCustomFields(c *gin.Context, a *platform.App, f customConfigFields, g customRequestGPU) error {
	if f.InstanceType != "container" && f.InstanceType != "virtual-machine" {
		return bad("instance_type must be container or virtual-machine")
	}
	if _, _, e := validResources(f.Resources); e != nil {
		return e
	}
	zone, e := platform.IDString(f.ZoneID)
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
	if (*g.Count == 0 && g.Model != nil) || (*g.Count > 0 && (g.Model == nil || !requestText(*g.Model, 255))) {
		return bad("gpu.model must be null for zero GPUs and a model string otherwise")
	}
	db := platform.DB(c, a)
	zr, e := platform.Get(db, "availability_zones", zone, false)
	if e != nil {
		return e
	}
	if platform.Text(zr["status"]) != "active" {
		return conflict("zone_unavailable", "availability zone is not active")
	}
	ir, e := platform.Get(db, "images", image, false)
	if e != nil {
		return e
	}
	if platform.Text(ir["status"]) != "active" || platform.Text(ir["image_type"]) != f.InstanceType || (*g.Count > 0 && !platform.Bool(ir["gpu_ready"])) {
		return bad("image is not compatible with the requested instance")
	}
	if _, e = catalog.ValidateNetworkOption(db, network, zone, f.InstanceType); e != nil {
		return e
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
func approvedSpec(name string, cfg customApprovedConfig) Spec {
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
	return Spec{Name: name, InstanceType: cfg.InstanceType, ZoneID: cfg.ZoneID, ImageID: cfg.ImageID, NetworkOptionID: cfg.NetworkOptionID, NodeID: cfg.NodeID, Resources: cfg.Resources, GPU: GPURequest{Count: *cfg.GPU.Count, Model: model, AccessMode: cfg.GPU.AccessMode, MaxHourlyPT: cfg.GPU.MaxHourlyPT, FixedUUIDs: cfg.GPU.FixedUUIDs}, InitParameters: cfg.InitParameters, InitTemplateVersionID: cfg.InitTemplateVersionID, AllowedUserActions: actions, GPUUserPermissions: permissions}
}
func validateApproved(c *gin.Context, a *platform.App, cfg customApprovedConfig) error {
	if e := validateCustomFields(c, a, cfg.customConfigFields, cfg.GPU.customRequestGPU); e != nil {
		return e
	}
	if e := validateCustomPermissions(cfg.AllowedUserActions, cfg.GPUUserPermissions); e != nil {
		return e
	}
	if cfg.NodeID != "" {
		if _, e := platform.IDString(cfg.NodeID); e != nil {
			return e
		}
	}
	if cfg.GPU.FixedUUIDs != nil && len(cfg.GPU.FixedUUIDs) != *cfg.GPU.Count {
		return bad("fixed_uuids length must equal gpu.count")
	}
	seen := make(map[string]bool, len(cfg.GPU.FixedUUIDs))
	for _, id := range cfg.GPU.FixedUUIDs {
		if !requestText(id, 128) || strings.TrimSpace(id) != id || seen[id] {
			return bad("fixed_uuids must contain unique GPU UUIDs")
		}
		seen[id] = true
	}
	return nil
}
func requestDTO(r platform.Row) platform.Row {
	out := platform.Public(r, "id", "user_id", "name", "reason", "status", "reviewed_by_user_id", "review_note", "reviewed_at", "instance_id", "delivery_task_id", "revision", "created_at", "updated_at")
	out["requested_config"] = object(r["requested_config"])
	out["approved_config"] = nil
	if r["approved_config"] != nil {
		out["approved_config"] = object(r["approved_config"])
	}
	return out
}
func requestRecord(c *gin.Context, a *platform.App, lock bool) (platform.Row, error) {
	id, e := platform.ID(c, "id")
	if e != nil {
		return nil, e
	}
	r, e := platform.Get(platform.DB(c, a), "custom_instance_requests", id, lock)
	if e != nil {
		return nil, e
	}
	if !c.GetBool("admin") && platform.Uint(r["user_id"]) != platform.Current(c).ID {
		return nil, platform.E(404, "not_found", "custom instance request not found")
	}
	return r, nil
}
func requestList(c *gin.Context, a *platform.App) (any, error) {
	q := platform.DB(c, a).Table("custom_instance_requests")
	filters := []string{"status"}
	if c.GetBool("admin") {
		filters = append(filters, "user_id")
		if s := c.Query("user_id"); s != "" {
			id, e := platform.IDString(s)
			if e != nil {
				return nil, e
			}
			q = q.Where("user_id = ?", id)
		}
	} else {
		q = q.Where("user_id = ?", platform.Current(c).ID)
	}
	if status := c.Query("status"); status != "" {
		switch status {
		case "pending", "approved", "rejected", "cancelled":
		default:
			return nil, bad("invalid request status")
		}
		q = q.Where("status = ?", status)
	}
	return platform.ListMap(c, a, q, filters, func(r platform.Row) (platform.Row, error) { return requestDTO(r), nil })
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

func registerRequests(a *platform.App, user, admin *gin.RouterGroup) {
	user.POST("/custom-instance-requests", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			Name            string              `json:"name"`
			Reason          string              `json:"reason"`
			RequestedConfig customRequestConfig `json:"requested_config"`
		}
		if e := platform.Decode(c, &in); e != nil {
			return nil, e
		}
		if !requestText(in.Name, 128) {
			return nil, bad("name must contain 1–128 characters")
		}
		if e := requestReason(in.Reason); e != nil {
			return nil, e
		}
		if e := validateCustomFields(c, a, in.RequestedConfig.customConfigFields, in.RequestedConfig.GPU); e != nil {
			return nil, e
		}
		db := platform.DB(c, a)
		r := platform.Row{"user_id": platform.Current(c).ID, "name": in.Name, "reason": in.Reason, "requested_config": jsonText(in.RequestedConfig), "status": "pending", "revision": uint64(1), "created_at": a.Time(), "updated_at": a.Time()}
		if e := db.Table("custom_instance_requests").Create(&r).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		if _, e := platform.InsertedID(r); e != nil {
			return nil, e
		}
		r, e := platform.Get(db, "custom_instance_requests", platform.Uint(r["id"]), false)
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		platform.Reply(c, http.StatusCreated, requestDTO(r))
		return nil, nil
	}))
	for _, g := range []*gin.RouterGroup{user, admin} {
		g.GET("/custom-instance-requests", platform.Handle(func(c *gin.Context) (any, error) { return requestList(c, a) }))
		g.GET("/custom-instance-requests/:id", platform.Handle(func(c *gin.Context) (any, error) {
			r, e := requestRecord(c, a, false)
			if e != nil {
				return nil, e
			}
			platform.SetETag(c, r)
			return requestDTO(r), nil
		}))
		g.GET("/instances/:id/gpu-policy", platform.Handle(func(c *gin.Context) (any, error) {
			r, e := owned(c, a)
			if e != nil {
				return nil, e
			}
			return gpuPolicyResponse(c, a, r)
		}))
		g.PUT("/instances/:id/gpu-policy", platform.Handle(func(c *gin.Context) (any, error) { return putGPUPolicy(c, a) }))
	}
	user.POST("/custom-instance-requests/:id/cancel", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct{}
		if e := platform.Decode(c, &in); e != nil {
			return nil, e
		}
		r, e := requestRecord(c, a, true)
		if e != nil {
			return nil, e
		}
		if platform.Text(r["status"]) != "pending" {
			return nil, conflict("request_not_pending", "only pending requests can be cancelled")
		}
		r, e = platform.Save(c, a, "custom_instance_requests", r, platform.Row{"status": "cancelled"})
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		return requestDTO(r), nil
	}))
	admin.POST("/custom-instance-requests/:id/reject", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			ReviewNote string `json:"review_note"`
		}
		if e := platform.Decode(c, &in); e != nil {
			return nil, e
		}
		if e := requestReason(in.ReviewNote); e != nil {
			return nil, e
		}
		r, e := requestRecord(c, a, true)
		if e != nil {
			return nil, e
		}
		if platform.Text(r["status"]) != "pending" {
			return nil, conflict("request_not_pending", "only pending requests can be rejected")
		}
		r, e = platform.Save(c, a, "custom_instance_requests", r, platform.Row{"status": "rejected", "reviewed_by_user_id": platform.Current(c).ID, "review_note": in.ReviewNote, "reviewed_at": a.Time()})
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		return requestDTO(r), nil
	}))
	admin.POST("/custom-instance-requests/:id/approve", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			ApprovedConfig customApprovedConfig `json:"approved_config"`
			ReviewNote     string               `json:"review_note"`
		}
		if e := platform.Decode(c, &in); e != nil {
			return nil, e
		}
		if e := requestReason(in.ReviewNote); e != nil {
			return nil, e
		}
		r, e := requestRecord(c, a, true)
		if e != nil {
			return nil, e
		}
		if e = platform.Revision(c, r); e != nil {
			return nil, e
		}
		if platform.Text(r["status"]) != "pending" {
			return nil, conflict("request_not_pending", "only pending requests can be approved")
		}
		if e = validateApproved(c, a, in.ApprovedConfig); e != nil {
			return nil, e
		}
		owner, e := activeOwner(c, a, strconv.FormatUint(platform.Uint(r["user_id"]), 10))
		if e != nil {
			return nil, e
		}
		out, e := createInstance(c.Request.Context(), a, platform.DB(c, a), owner, approvedSpec(platform.Text(r["name"]), in.ApprovedConfig), true, nil, in.ReviewNote)
		if e != nil {
			return nil, e
		}
		// Approval and immutable approved snapshot commit with the delivery task. The
		// worker owns delivery status only; a failed delivery never reverses review.
		r, e = platform.Save(c, a, "custom_instance_requests", r, platform.Row{"status": "approved", "approved_config": jsonText(in.ApprovedConfig), "reviewed_by_user_id": platform.Current(c).ID, "review_note": in.ReviewNote, "reviewed_at": a.Time(), "instance_id": platform.Uint(out["instance_id"]), "delivery_task_id": platform.Uint(out["task_id"])})
		if e != nil {
			return nil, e
		}
		out["request"] = requestDTO(r)
		platform.SetETag(c, r)
		requestAccepted(c, out)
		return nil, nil
	}))
	admin.POST("/custom-instances", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			OwnerUserID    string               `json:"owner_user_id"`
			Name           string               `json:"name"`
			ApprovedConfig customApprovedConfig `json:"approved_config"`
			Reason         string               `json:"reason"`
		}
		if e := platform.Decode(c, &in); e != nil {
			return nil, e
		}
		if !requestText(in.Name, 128) {
			return nil, bad("name must contain 1–128 characters")
		}
		if e := requestReason(in.Reason); e != nil {
			return nil, e
		}
		if e := validateApproved(c, a, in.ApprovedConfig); e != nil {
			return nil, e
		}
		owner, e := activeOwner(c, a, in.OwnerUserID)
		if e != nil {
			return nil, e
		}
		out, e := createInstance(c.Request.Context(), a, platform.DB(c, a), owner, approvedSpec(in.Name, in.ApprovedConfig), true, nil, in.Reason)
		if e != nil {
			return nil, e
		}
		requestAccepted(c, out)
		return nil, nil
	}))
	admin.POST("/nodes/:id/instance-imports", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			IncusUUID      string               `json:"incus_uuid"`
			OwnerUserID    string               `json:"owner_user_id"`
			ApprovedConfig customApprovedConfig `json:"approved_config"`
			Reason         string               `json:"reason"`
		}
		if e := platform.Decode(c, &in); e != nil {
			return nil, e
		}
		if !validIncusUUID(in.IncusUUID) {
			return nil, bad("incus_uuid must be a canonical UUID")
		}
		if e := requestReason(in.Reason); e != nil {
			return nil, e
		}
		node, e := platform.ID(c, "id")
		if e != nil {
			return nil, e
		}
		nr, e := platform.Get(platform.DB(c, a), "nodes", node, true)
		if e != nil {
			return nil, e
		}
		if in.ApprovedConfig.NodeID != "" && in.ApprovedConfig.NodeID != strconv.FormatUint(node, 10) {
			return nil, bad("approved node_id must match the route node")
		}
		in.ApprovedConfig.NodeID = strconv.FormatUint(node, 10)
		if e = validateApproved(c, a, in.ApprovedConfig); e != nil {
			return nil, e
		}
		if platform.Uint(nr["availability_zone_id"]) != mustUint(in.ApprovedConfig.ZoneID) {
			return nil, bad("approved zone must match the node")
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
		spec := approvedSpec("Imported "+in.IncusUUID, in.ApprovedConfig)
		out, e := platform.Enqueue(platform.DB(c, a), "instance.import", owner, node, 0, false, platform.Row{"owner_user_id": strconv.FormatUint(owner, 10), "incus_uuid": in.IncusUUID, "approved_config": spec, "reason": in.Reason})
		if e != nil {
			return nil, e
		}
		requestAccepted(c, out)
		return nil, nil
	}))
}
func requestAccepted(c *gin.Context, out platform.Row) {
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
	Reason             string           `json:"reason"`
}
type gpuPolicyBounds struct {
	Idle       billing.IdleConfig
	Scheduling billing.SchedulingConfig
	Versions   map[string]string
}

func policyBounds(c *gin.Context, a *platform.App) (gpuPolicyBounds, error) {
	out := gpuPolicyBounds{Versions: map[string]string{}}
	for _, kind := range []string{"gpu-idle", "gpu-scheduling"} {
		var r platform.Row
		e := platform.DB(c, a).Table("policy_versions p").Select("p.id,p.config").Joins("JOIN platform_policy_bindings b ON b.policy_id = p.id AND b.kind = p.kind").Where("p.kind = ? AND p.status = 'active' AND p.effective_at <= ?", kind, a.Time()).Take(&r).Error
		if e != nil {
			if e == gorm.ErrRecordNotFound {
				return out, conflict("policy_not_configured", "published "+kind+" policy is required")
			}
			return out, platform.WrapDB(e)
		}
		out.Versions[kind] = strconv.FormatUint(platform.Uint(r["id"]), 10)
		var dst any
		if kind == "gpu-idle" {
			dst = &out.Idle
		} else {
			dst = &out.Scheduling
		}
		if e = json.Unmarshal([]byte(platform.Text(r["config"])), dst); e != nil {
			return out, platform.E(500, "invalid_stored_policy", "stored GPU policy is invalid")
		}
	}
	if out.Idle.Min <= 0 || out.Idle.Max < out.Idle.Min || out.Idle.Default < out.Idle.Min || out.Idle.Default > out.Idle.Max || len(out.Idle.Actions) == 0 {
		return out, platform.E(500, "invalid_stored_policy", "stored GPU idle bounds are invalid")
	}
	return out, nil
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
	b, e := policyBounds(c, a)
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
		blocked = append(blocked, "platform_idle_disabled")
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
	if platform.Text(r["product_type"]) == "custom" && !c.GetBool("admin") {
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
	out := platform.Row{"idle": cfg.Idle, "rebalance": cfg.Rebalance, "effective_policy": platform.Row{"idle": effective.Idle, "rebalance": effective.Rebalance, "safety": platform.Row{"complete_fresh_ownership_required": true, "gpu_processes_must_be_zero": true, "gpu_memory_must_be_zero": true, "device_references_must_be_zero": true, "gpu_utilization_threshold_pct": b.Idle.Util, "gpu_memory_threshold_pct": b.Idle.Memory, "metrics_max_age_seconds": b.Scheduling.MaxAge, "cooldown_seconds": b.Scheduling.Cooldown, "graceful_stop_timeout_seconds": b.Idle.StopTimeout, "allow_force_stop": b.Idle.Force, "allow_price_increase": b.Scheduling.AllowIncrease}}, "allowed_ranges": platform.Row{"idle": platform.Row{"enabled": b.Idle.Enabled, "default_timeout_seconds": b.Idle.Default, "min_timeout_seconds": b.Idle.Min, "max_timeout_seconds": b.Idle.Max, "allowed_actions": actions}, "rebalance": platform.Row{"min_hourly_pt": "0.000000", "max_hourly_pt": "99999999999999.999999", "allow_price_increase": b.Scheduling.AllowIncrease}, "editable": editable}, "policy_versions": b.Versions, "revision": strconv.FormatUint(platform.Uint(r["revision"]), 10), "idle_since": nil, "last_rebound_at": nil, "blocked_reasons": blocked, "allowed_user_actions": rows(r["allowed_user_actions"]), "gpu_user_permissions": object(r["gpu_user_permissions"])}
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
		if e = requestReason(adminInput.Reason); e != nil {
			return nil, e
		}
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
	b, e := policyBounds(c, a)
	if e != nil {
		return nil, e
	}
	cfg := instanceGPUPolicy{Idle: storedIdlePolicy{Enabled: *input.Idle.Enabled, TimeoutSeconds: *input.Idle.TimeoutSeconds, Action: input.Idle.Action}, Rebalance: storedRebalancePolicy{Enabled: *input.Rebalance.Enabled, AllowLive: *input.Rebalance.AllowLive, MaxHourlyPT: input.Rebalance.MaxHourlyPT}}
	if cfg.Idle.TimeoutSeconds < uint64(b.Idle.Min) || cfg.Idle.TimeoutSeconds > uint64(b.Idle.Max) {
		return nil, bad("idle.timeout_seconds is outside the active platform policy range")
	}
	if cfg.Idle.Action != "detach_gpu" && cfg.Idle.Action != "stop_instance" {
		return nil, bad("idle.action must be detach_gpu or stop_instance")
	}
	if !containsPolicyAction(b.Idle.Actions, cfg.Idle.Action) {
		return nil, platform.E(403, "idle_action_not_allowed", "idle action is not allowed by the active platform policy")
	}
	if cfg.Idle.Enabled && !b.Idle.Enabled {
		return nil, platform.E(403, "idle_policy_disabled", "platform GPU idle actions are disabled")
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
	if !c.GetBool("admin") && platform.Text(r["product_type"]) == "custom" {
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
	if e = platform.Audit(c.Request.Context(), platform.DB(c, a), platform.Current(c).ID, 0, platform.Uint(r["id"]), "instance.gpu_policy.updated", platform.Row{"before": previous, "after": cfg, "allowed_user_actions": rows(r["allowed_user_actions"]), "gpu_user_permissions": object(r["gpu_user_permissions"]), "reason": adminInput.Reason, "policy_versions": b.Versions}); e != nil {
		return nil, e
	}
	return gpuPolicyDTO(c, a, r, b)
}
func decimalValue(s string) decimal.Decimal { v, _ := decimal.NewFromString(s); return v }
