package billing

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type PricingConfig struct {
	CPU    string `json:"cpu_hourly_pt"`
	Memory string `json:"memory_gib_hourly_pt"`
	Disk   string `json:"disk_gib_hourly_pt"`
}
type GPUPriceConfig struct {
	Model  string `json:"gpu_model"`
	Access string `json:"access_mode"`
	Rate   string `json:"gpu_hourly_pt"`
}
type CreditConfig struct {
	Daily     string `json:"daily_allowance_pt"`
	Timezone  string `json:"timezone"`
	GrantTime string `json:"grant_time"`
	Validity  uint64 `json:"daily_validity_seconds"`
	Window    uint64 `json:"billing_window_seconds"`
	Grace     uint64 `json:"shutdown_grace_seconds"`
	GraceMode string `json:"grace_charge_mode"`
	Restore   string `json:"restore_action"`
	Storage   string `json:"storage_charge_mode"`
	Offline   string `json:"offline_charge_mode"`
}
type SchedulingConfig struct {
	Poll            int     `json:"poll_interval_seconds"`
	MaxAge          int     `json:"metrics_max_age_seconds"`
	SM              float64 `json:"sm_weight"`
	Memory          float64 `json:"memory_weight"`
	High            float64 `json:"source_high_load_pct"`
	Low             float64 `json:"target_low_load_pct"`
	Window          int     `json:"evaluation_window_seconds"`
	Improvement     float64 `json:"min_improvement_pct"`
	Cooldown        int     `json:"cooldown_seconds"`
	RebindingWindow int     `json:"rebindings_window_seconds"`
	MaxRebindings   int     `json:"max_rebindings_per_window"`
	Concurrency     int     `json:"max_concurrent_per_node"`
	AllowIncrease   bool    `json:"allow_price_increase"`
}
type IdleConfig struct {
	Enabled     bool     `json:"enabled"`
	Default     int      `json:"default_timeout_seconds"`
	Min         int      `json:"min_timeout_seconds"`
	Max         int      `json:"max_timeout_seconds"`
	Actions     []string `json:"allowed_actions"`
	Util        float64  `json:"gpu_utilization_threshold_pct"`
	Memory      float64  `json:"gpu_memory_threshold_pct"`
	StopTimeout int      `json:"graceful_stop_timeout_seconds"`
	Force       bool     `json:"allow_force_stop"`
}
type MetricsConfig struct {
	Raw         int  `json:"raw_retention_days"`
	Rollup      int  `json:"rollup_retention_days"`
	Bucket      int  `json:"bucket_seconds"`
	Collection  int  `json:"collection_interval_seconds"`
	Timeout     *int `json:"collection_timeout_seconds,omitempty"`
	Concurrency *int `json:"max_concurrent_nodes,omitempty"`
}

func strictConfig(raw json.RawMessage, dst any, keys ...string) error {
	var obj map[string]json.RawMessage
	if e := json.Unmarshal(raw, &obj); e != nil || obj == nil {
		return invalid("config must be an object")
	}
	for k, v := range obj {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return invalid("config field cannot be null: " + k)
		}
	}
	for _, k := range keys {
		if _, ok := obj[k]; !ok {
			return invalid("config requires explicit " + k)
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(dst); e != nil {
		return invalid("invalid or unsupported policy config field")
	}
	return nil
}
func in(s string, choices ...string) bool {
	for _, v := range choices {
		if s == v {
			return true
		}
	}
	return false
}
func positive(v ...int) bool {
	for _, n := range v {
		if n <= 0 || n > 31536000 {
			return false
		}
	}
	return true
}
func percent(v ...float64) bool {
	for _, n := range v {
		if n < 0 || n > 100 {
			return false
		}
	}
	return true
}
func validatePolicy(kind string, raw json.RawMessage) (any, error) {
	switch kind {
	case "pricing":
		var p PricingConfig
		if e := strictConfig(raw, &p, "cpu_hourly_pt", "memory_gib_hourly_pt", "disk_gib_hourly_pt"); e != nil {
			return nil, e
		}
		for _, s := range []string{p.CPU, p.Memory, p.Disk} {
			if _, e := amount(s, false); e != nil {
				return nil, e
			}
		}
		return p, nil
	case "gpu-pricing":
		var p GPUPriceConfig
		if e := strictConfig(raw, &p, "gpu_model", "access_mode", "gpu_hourly_pt"); e != nil {
			return nil, e
		}
		if len(p.Model) == 0 || len(p.Model) > 255 || !in(p.Access, "shared", "exclusive") {
			return nil, invalid("invalid GPU model or access mode")
		}
		_, e := amount(p.Rate, false)
		return p, e
	case "credit":
		var p CreditConfig
		if e := strictConfig(raw, &p, "daily_allowance_pt", "timezone", "grant_time", "daily_validity_seconds", "billing_window_seconds", "shutdown_grace_seconds", "grace_charge_mode", "restore_action", "storage_charge_mode", "offline_charge_mode"); e != nil {
			return nil, e
		}
		if _, e := amount(p.Daily, false); e != nil {
			return nil, e
		}
		if _, e := time.LoadLocation(p.Timezone); e != nil {
			return nil, invalid("invalid IANA timezone")
		}
		if _, e := time.Parse("15:04:05", p.GrantTime); e != nil {
			return nil, invalid("grant_time must use HH:MM:SS")
		}
		if p.Validity == 0 || p.Validity > 315360000 || p.Window == 0 || p.Window > 86400 || p.Grace == 0 || p.Grace > 86400 || !in(p.GraceMode, "waived", "accrue") || !in(p.Restore, "cancel_pending_stop", "keep_deadline") || !in(p.Storage, "retained", "running_only", "free") || !in(p.Offline, "defer", "continue") {
			return nil, invalid("credit policy requires explicit economic choices and a shutdown grace between 1 and 86400 seconds")
		}
		return p, nil
	case "gpu-scheduling":
		var p SchedulingConfig
		if e := strictConfig(raw, &p, "poll_interval_seconds", "metrics_max_age_seconds", "sm_weight", "memory_weight", "source_high_load_pct", "target_low_load_pct", "evaluation_window_seconds", "min_improvement_pct", "cooldown_seconds", "rebindings_window_seconds", "max_rebindings_per_window", "max_concurrent_per_node", "allow_price_increase"); e != nil {
			return nil, e
		}
		sum := decimal.NewFromFloat(p.SM).Add(decimal.NewFromFloat(p.Memory))
		if !positive(p.Poll, p.MaxAge, p.Window, p.Cooldown, p.RebindingWindow, p.MaxRebindings, p.Concurrency) || !percent(p.High, p.Low, p.Improvement) || p.Low >= p.High || p.SM < 0 || p.Memory < 0 || !sum.Equal(decimal.NewFromInt(1)) {
			return nil, invalid("invalid scheduling intervals, thresholds or weights")
		}
		return p, nil
	case "gpu-idle":
		var p IdleConfig
		if e := strictConfig(raw, &p, "enabled", "default_timeout_seconds", "min_timeout_seconds", "max_timeout_seconds", "allowed_actions", "gpu_utilization_threshold_pct", "gpu_memory_threshold_pct", "graceful_stop_timeout_seconds", "allow_force_stop"); e != nil {
			return nil, e
		}
		if !positive(p.Min, p.Default, p.Max, p.StopTimeout) || p.Default < p.Min || p.Default > p.Max || !percent(p.Util, p.Memory) || len(p.Actions) == 0 || len(p.Actions) > 2 {
			return nil, invalid("invalid GPU idle policy bounds")
		}
		seen := map[string]bool{}
		for _, s := range p.Actions {
			if !in(s, "release_gpu", "stop_instance") || seen[s] {
				return nil, invalid("invalid or repeated GPU idle action")
			}
			seen[s] = true
		}
		return p, nil
	case "metrics":
		var p MetricsConfig
		if e := strictConfig(raw, &p, "raw_retention_days", "rollup_retention_days", "bucket_seconds", "collection_interval_seconds"); e != nil {
			return nil, e
		}
		if !positive(p.Raw, p.Rollup, p.Bucket, p.Collection) || p.Bucket < p.Collection || p.Rollup < p.Raw {
			return nil, invalid("invalid metrics retention or collection intervals")
		}
		if p.Timeout != nil && (*p.Timeout < 1 || *p.Timeout > 120) {
			return nil, invalid("collection timeout must be 1 through 120 seconds")
		}
		if p.Concurrency != nil && (*p.Concurrency < 1 || *p.Concurrency > 32) {
			return nil, invalid("max_concurrent_nodes must be 1 through 32")
		}
		return p, nil
	}
	return nil, invalid("unsupported policy kind")
}
func economicTable(kind string) string {
	return map[string]string{"pricing": "pricing_versions", "gpu-pricing": "gpu_price_versions", "credit": "credit_policy_versions", "gpu-idle": "idle_policy_versions"}[kind]
}
func sourceFields(kind string, p any) row {
	switch v := p.(type) {
	case PricingConfig:
		return row{"cpu_hourly_pt": v.CPU, "memory_gib_hourly_pt": v.Memory, "disk_gib_hourly_pt": v.Disk}
	case GPUPriceConfig:
		return row{"name": v.Model, "gpu_model": v.Model, "access_mode": v.Access, "gpu_hourly_pt": v.Rate}
	case CreditConfig:
		return row{"daily_allowance_pt": v.Daily, "bonus_cap_pt": "99999999999999.999999", "timezone": v.Timezone, "reset_time": v.GrantTime, "redemption_validity_seconds": nil, "billing_window_seconds": v.Window, "daily_validity_seconds": v.Validity, "shutdown_grace_seconds": v.Grace, "grace_charge_mode": v.GraceMode, "restore_action": v.Restore, "storage_charge_mode": v.Storage, "offline_charge_mode": v.Offline}
	case IdleConfig:
		return row{"sample_interval_seconds": 1, "evaluation_window_seconds": v.Default, "consecutive_idle_windows": 1, "cpu_utilization_threshold_pct": 0, "gpu_utilization_threshold_pct": v.Util, "gpu_memory_threshold_pct": v.Memory, "network_bytes_threshold": 0, "action": v.Actions[0]}
	}
	return nil
}
func policyDTO(r row) row {
	out := public(r)
	// source_id is the internal legacy row; the API identifies versions by id only.
	delete(out, "source_id")
	out["config"] = decoded(r["config"])
	return out
}
func policyItem(r row) (row, error) { return policyDTO(r), nil }
func policyRow(c *gin.Context, a *platform.App, lock bool) (row, error) {
	id, e := requireID(c)
	if e != nil {
		return nil, e
	}
	r, e := platform.Get(platform.DB(c, a), "policy_versions", id, lock)
	if e != nil {
		return nil, e
	}
	if platform.Text(r["kind"]) != c.Param("kind") {
		return nil, platform.E(404, "not_found", "policy version not found")
	}
	return r, nil
}

var requiredPolicyKinds = [...]string{"pricing", "gpu-pricing", "credit", "gpu-scheduling", "gpu-idle", "metrics"}

func validateBindingShape(bindings map[string]string) error {
	if len(bindings) != len(requiredPolicyKinds) {
		return invalid("bindings must provide the complete pricing, gpu-pricing, credit, gpu-scheduling, gpu-idle and metrics mapping")
	}
	for _, kind := range requiredPolicyKinds {
		s, ok := bindings[kind]
		if !ok {
			return invalid("missing required policy binding: " + kind)
		}
		if _, e := platform.IDString(s); e != nil {
			return e
		}
	}
	return nil
}
func validateBindingVersion(kind string, p row, at time.Time) error {
	effective := stamp(p["effective_at"])
	if platform.Text(p["kind"]) != kind || platform.Text(p["status"]) != "active" || effective.IsZero() || effective.After(at) {
		return invalid("each binding must reference an already effective published policy of the same kind")
	}
	return nil
}
func registerPolicies(a *platform.App, g *gin.RouterGroup) {
	g.GET("/policies/:kind/versions", platform.Handle(func(c *gin.Context) (any, error) {
		if _, ok := map[string]bool{"pricing": true, "gpu-pricing": true, "credit": true, "gpu-idle": true, "gpu-scheduling": true, "metrics": true}[c.Param("kind")]; !ok {
			return nil, invalid("unsupported policy kind")
		}
		return platform.ListMap(c, a, platform.DB(c, a).Table("policy_versions").Where("kind = ?", c.Param("kind")), nil, policyItem)
	}))
	g.POST("/policies/:kind/versions", platform.Handle(func(c *gin.Context) (any, error) {
		var b struct {
			Code    string          `json:"code"`
			Version string          `json:"version"`
			Config  json.RawMessage `json:"config"`
		}
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		if len(b.Code) == 0 || len(b.Code) > 64 || len(b.Version) == 0 || len(b.Version) > 64 || strings.TrimSpace(b.Code) != b.Code {
			return nil, invalid("invalid policy code or version")
		}
		typed, e := validatePolicy(c.Param("kind"), b.Config)
		if e != nil {
			return nil, e
		}
		db := platform.DB(c, a)
		r := row{"kind": c.Param("kind"), "code": b.Code, "version": b.Version, "config": jsonValue(typed), "status": "draft", "created_by_user_id": platform.Current(c).ID}
		if table := economicTable(c.Param("kind")); table != "" {
			s := sourceFields(c.Param("kind"), typed)
			s["code"] = b.Code
			s["version"] = b.Version
			s["status"] = "draft"
			s["effective_at"] = now()
			s["created_by_user_id"] = platform.Current(c).ID
			source, e := insert(db, table, s)
			if e != nil {
				return nil, e
			}
			r["source_id"] = source["id"]
		}
		created, e := insert(db, "policy_versions", r)
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, created)
		platform.Reply(c, 201, policyDTO(created))
		return nil, nil
	}))
	g.GET("/policies/:kind/versions/:id", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := policyRow(c, a, false)
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		return policyDTO(r), nil
	}))
	g.PATCH("/policies/:kind/versions/:id", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := policyRow(c, a, true)
		if e != nil {
			return nil, e
		}
		if platform.Text(r["status"]) != "draft" {
			return nil, conflict("invalid_state", "only draft config can be changed")
		}
		var b struct {
			Config json.RawMessage `json:"config"`
		}
		if e = platform.Decode(c, &b); e != nil {
			return nil, e
		}
		typed, e := validatePolicy(c.Param("kind"), b.Config)
		if e != nil {
			return nil, e
		}
		r, e = platform.Save(c, a, "policy_versions", r, row{"config": jsonValue(typed)})
		if e != nil {
			return nil, e
		}
		if table := economicTable(c.Param("kind")); table != "" {
			if e = platform.DB(c, a).Table(table).Where("id = ?", r["source_id"]).Updates(sourceFields(c.Param("kind"), typed)).Error; e != nil {
				return nil, e
			}
		}
		platform.SetETag(c, r)
		return policyDTO(r), nil
	}))
	for _, action := range []string{"publish", "retire"} {
		action := action
		g.POST("/policies/:kind/versions/:id/"+action, platform.Handle(func(c *gin.Context) (any, error) {
			r, e := policyRow(c, a, true)
			if e != nil {
				return nil, e
			}
			changes := row{}
			if action == "publish" {
				var b struct {
					Effective string `json:"effective_at"`
				}
				if e = platform.Decode(c, &b); e != nil {
					return nil, e
				}
				if platform.Text(r["status"]) != "draft" {
					return nil, conflict("invalid_state", "only a draft can be published")
				}
				t, e := parseTime(b.Effective)
				if e != nil {
					return nil, e
				}
				if _, e = validatePolicy(c.Param("kind"), json.RawMessage(platform.Text(r["config"]))); e != nil {
					return nil, e
				}
				changes["status"] = "active"
				changes["effective_at"] = t
			} else {
				if e = platform.Decode(c, &struct{}{}); e != nil {
					return nil, e
				}
				if platform.Text(r["status"]) != "active" {
					return nil, conflict("invalid_state", "only an active version can be retired")
				}
				var n int64
				if e = platform.DB(c, a).Table("platform_policy_bindings").Where("policy_id = ?", r["id"]).Count(&n).Error; e != nil {
					return nil, e
				}
				if n > 0 {
					return nil, conflict("configuration_conflict", "replace the platform binding before retiring this policy")
				}
				changes["status"] = "retired"
			}
			r, e = platform.Save(c, a, "policy_versions", r, changes)
			if e != nil {
				return nil, e
			}
			if table := economicTable(c.Param("kind")); table != "" {
				if e = platform.DB(c, a).Table(table).Where("id = ?", r["source_id"]).Updates(changes).Error; e != nil {
					return nil, e
				}
			}
			platform.SetETag(c, r)
			return policyDTO(r), nil
		}))
	}
	g.GET("/platform-policy-bindings", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := platform.Get(platform.DB(c, a), "policy_binding_sets", 1, false)
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		return bindingsDTO(platform.DB(c, a), r)
	}))
	g.PUT("/platform-policy-bindings", platform.Handle(func(c *gin.Context) (any, error) {
		var b map[string]string
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		if e := validateBindingShape(b); e != nil {
			return nil, e
		}
		db := platform.DB(c, a)
		r, e := platform.Get(db, "policy_binding_sets", 1, true)
		if e != nil {
			return nil, e
		}
		if e = platform.Revision(c, r); e != nil {
			return nil, e
		}
		at := now()
		for _, kind := range requiredPolicyKinds {
			id, _ := platform.IDString(b[kind])
			p, e := platform.Get(db, "policy_versions", id, true)
			if e != nil {
				return nil, e
			}
			if e = validateBindingVersion(kind, p, at); e != nil {
				return nil, e
			}
		}
		if e = db.Exec("DELETE FROM platform_policy_bindings").Error; e != nil {
			return nil, e
		}
		for kind, s := range b {
			id, _ := platform.IDString(s)
			if e = db.Table("platform_policy_bindings").Create(&row{"kind": kind, "policy_id": id}).Error; e != nil {
				return nil, e
			}
		}
		r, e = platform.Save(c, a, "policy_binding_sets", r, row{})
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		return bindingsDTO(db, r)
	}))
}
func bindingsDTO(db *gorm.DB, r row) (row, error) {
	bs, e := rows(db, "platform_policy_bindings", "1=1")
	if e != nil {
		return nil, e
	}
	m := map[string]string{}
	for _, b := range bs {
		m[platform.Text(b["kind"])] = sid(b["policy_id"])
	}
	return row{"bindings": m, "revision": sid(r["revision"])}, nil
}
