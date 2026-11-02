package catalog

import (
	"encoding/json"
	"errors"
	"strings"

	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type PlanIdlePolicy struct {
	Enabled        bool   `json:"enabled"`
	TimeoutSeconds uint64 `json:"timeout_seconds"`
	Action         string `json:"action"`
}
type PlanRebalancePolicy struct {
	Enabled     bool   `json:"enabled"`
	AllowLive   bool   `json:"allow_live"`
	MaxHourlyPT string `json:"max_hourly_pt"`
}
type PlanGPUPolicy struct {
	Idle      PlanIdlePolicy      `json:"idle"`
	Rebalance PlanRebalancePolicy `json:"rebalance"`
}
type planGPUPolicyInput struct {
	Idle *struct {
		Enabled        *bool   `json:"enabled"`
		TimeoutSeconds *uint64 `json:"timeout_seconds"`
		Action         string  `json:"action"`
	} `json:"idle"`
	Rebalance *struct {
		Enabled     *bool  `json:"enabled"`
		AllowLive   *bool  `json:"allow_live"`
		MaxHourlyPT string `json:"max_hourly_pt"`
	} `json:"rebalance"`
}

var maximumPlanPrice = decimal.RequireFromString("99999999999999.999999")

// planMoney matches the instance policy's exact DECIMAL(20,6) representation.
func planMoney(s string) (decimal.Decimal, error) {
	if s == "" {
		return decimal.Zero, invalid("GPU 价格上限必须为非负十进制字符串。")
	}
	dot, digits, fraction := false, 0, 0
	for _, ch := range s {
		if ch == '.' && !dot {
			dot = true
			continue
		}
		if ch < '0' || ch > '9' {
			return decimal.Zero, invalid("GPU 价格上限必须为非负十进制字符串。")
		}
		digits++
		if dot {
			fraction++
		}
	}
	if digits == 0 || fraction > 6 || strings.HasSuffix(s, ".") {
		return decimal.Zero, invalid("GPU 价格上限最多允许六位小数。")
	}
	price, e := decimal.NewFromString(s)
	if e != nil || price.IsNegative() || price.GreaterThan(maximumPlanPrice) {
		return decimal.Zero, invalid("GPU 价格上限超出 DECIMAL(20,6) 范围。")
	}
	return price, nil
}

func planGPUConfiguration(b planInput, policies map[string]platform.Row) (PlanGPUPolicy, []string, map[string]bool, error) {
	var idle billing.IdleConfig
	if e := json.Unmarshal([]byte(platform.Text(policies["gpu-idle"]["config"])), &idle); e != nil || idle.Min <= 0 || idle.Max < idle.Min || idle.Default < idle.Min || idle.Default > idle.Max || len(idle.Actions) == 0 {
		return PlanGPUPolicy{}, nil, nil, platform.E(500, "invalid_stored_policy", "套餐绑定的 GPU 空闲策略配置无效。")
	}
	price, e := planMoney(b.GPU.MaxHourlyPT)
	if e != nil {
		return PlanGPUPolicy{}, nil, nil, e
	}
	action := idle.Actions[0]
	if action == "release_gpu" {
		action = "detach_gpu"
	}
	cfg := PlanGPUPolicy{Idle: PlanIdlePolicy{TimeoutSeconds: uint64(idle.Default), Action: action}, Rebalance: PlanRebalancePolicy{MaxHourlyPT: price.StringFixed(6)}}
	if in := b.GPUPolicy; in != nil {
		if in.Idle == nil || in.Rebalance == nil || in.Idle.Enabled == nil || in.Idle.TimeoutSeconds == nil || in.Rebalance.Enabled == nil || in.Rebalance.AllowLive == nil {
			return cfg, nil, nil, invalid("gpu_policy.idle 和 rebalance 必须包含全部配置字段。")
		}
		cfg.Idle = PlanIdlePolicy{Enabled: *in.Idle.Enabled, TimeoutSeconds: *in.Idle.TimeoutSeconds, Action: in.Idle.Action}
		cap, err := planMoney(in.Rebalance.MaxHourlyPT)
		if err != nil {
			return cfg, nil, nil, err
		}
		if !cap.Equal(price) {
			return cfg, nil, nil, invalid("gpu.max_hourly_pt 必须与 gpu_policy.rebalance.max_hourly_pt 一致。")
		}
		cfg.Rebalance = PlanRebalancePolicy{Enabled: *in.Rebalance.Enabled, AllowLive: *in.Rebalance.AllowLive, MaxHourlyPT: cap.StringFixed(6)}
	}
	if cfg.Idle.TimeoutSeconds < uint64(idle.Min) || cfg.Idle.TimeoutSeconds > uint64(idle.Max) {
		return cfg, nil, nil, invalid("gpu_policy.idle.timeout_seconds 超出套餐绑定策略范围。")
	}
	if !enum(cfg.Idle.Action, "detach_gpu", "stop_instance") {
		return cfg, nil, nil, invalid("gpu_policy.idle.action 必须为 detach_gpu 或 stop_instance。")
	}
	allowedAction := false
	for _, action := range idle.Actions {
		if action == cfg.Idle.Action || (action == "release_gpu" && cfg.Idle.Action == "detach_gpu") {
			allowedAction = true
		}
	}
	if !allowedAction || (cfg.Idle.Enabled && !idle.Enabled) {
		return cfg, nil, nil, invalid("套餐绑定策略未允许所选 GPU 空闲动作。")
	}
	actions := b.AllowedUserActions
	if actions == nil {
		actions = []string{"start", "stop", "restart", "console"}
	}
	permissions := b.GPUUserPermissions
	if permissions == nil {
		permissions = map[string]bool{}
	}
	seen := make(map[string]bool, len(actions))
	for _, action := range actions {
		if !enum(action, "start", "stop", "restart", "resize", "delete", "gpu_attach", "gpu_detach", "gpu_rebind", "gpu_policy", "console") || seen[action] {
			return cfg, nil, nil, invalid("allowed_user_actions 包含未知或重复的动作。")
		}
		if b.InstanceType == "virtual-machine" && strings.HasPrefix(action, "gpu_") {
			return cfg, nil, nil, invalid("虚拟机套餐不支持 GPU 用户动作。")
		}
		seen[action] = true
	}
	for key, enabled := range permissions {
		if !enum(key, "idle", "idle_timeout", "idle_action", "rebalance", "allow_live", "max_hourly_pt") {
			return cfg, nil, nil, invalid("gpu_user_permissions 包含未知权限。")
		}
		if b.InstanceType == "virtual-machine" && enabled {
			return cfg, nil, nil, invalid("虚拟机套餐不能授权 GPU 策略编辑。")
		}
	}
	if b.InstanceType == "virtual-machine" && (cfg.Idle.Enabled || cfg.Rebalance.Enabled || cfg.Rebalance.AllowLive) {
		return cfg, nil, nil, invalid("虚拟机套餐不能启用 GPU 空闲回收、重平衡或在线迁移。")
	}
	return cfg, actions, permissions, nil
}

// Every offered image/network combination must resolve in its exact node project.
// Drafts may reference preparation targets; publication requires native readiness.
func validatePlanPlacement(c *gin.Context, a *platform.App, b planInput, publish bool) error {
	if len(b.NodeIDs) == 0 || len(b.ImageIDs) == 0 || len(b.NetworkOptionIDs) == 0 {
		return invalid("套餐必须选择 node_ids、image_ids 和 network_option_ids。")
	}
	db := platform.DB(c, a)
	nodes := make(map[uint64]platform.Row, len(b.NodeIDs))
	for _, value := range b.NodeIDs {
		id, e := platform.IDString(value)
		if e != nil {
			return e
		}
		if nodes[id] != nil {
			return invalid("node_ids 包含重复节点。")
		}
		node, e := platform.Get(db, "nodes", id, true)
		if e != nil {
			return e
		}
		if publish && (platform.Text(node["administrative_state"]) != "active" || platform.Text(node["health"]) != "online" || platform.Stamp(node["connection_verified_at"]).IsZero()) {
			return missingReference("active verified node")
		}
		nodes[id] = node
	}
	images := make([]platform.Row, 0, len(b.ImageIDs))
	seen := make(map[uint64]bool, len(b.ImageIDs))
	for _, value := range b.ImageIDs {
		id, e := platform.IDString(value)
		if e != nil {
			return e
		}
		if seen[id] {
			return invalid("image_ids 包含重复镜像。")
		}
		seen[id] = true
		image, e := platform.Get(db, "images", id, true)
		if e != nil {
			return e
		}
		if platform.Text(image["image_type"]) != b.InstanceType || (publish && platform.Text(image["status"]) != "active") {
			return missingReference("compatible image")
		}
		images = append(images, image)
	}
	seen = make(map[uint64]bool, len(b.NetworkOptionIDs))
	covered := make(map[uint64]bool, len(nodes))
	for _, value := range b.NetworkOptionIDs {
		id, e := platform.IDString(value)
		if e != nil {
			return e
		}
		if seen[id] {
			return invalid("network_option_ids 包含重复网络。")
		}
		seen[id] = true
		binding, e := platform.Get(db, "node_network_bindings", id, true)
		if e != nil {
			return e
		}
		nodeID := platform.Uint(binding["node_id"])
		node := nodes[nodeID]
		if node == nil {
			return invalid("所选网络必须属于 node_ids 中的节点。")
		}
		if b.InstanceType == "virtual-machine" && platform.Text(binding["mode"]) == "macvlan" {
			return invalid("虚拟机套餐不支持 macvlan 网络。")
		}
		if publish {
			binding, e = ValidateNetworkOption(db, id, b.InstanceType)
			if e != nil {
				return e
			}
		}
		for _, image := range images {
			var target platform.Row
			if publish {
				target, e = ResolveNodeImage(c.Request.Context(), db, image, nodeID, platform.Text(binding["project_name"]))
			} else {
				e = db.Table("image_nodes").Where("image_id=? AND node_id=? AND project_name=?", image["id"], nodeID, binding["project_name"]).Take(&target).Error
				if errors.Is(e, gorm.ErrRecordNotFound) {
					return missingReference("image target in selected node/project")
				}
				if e != nil {
					return platform.WrapDB(e)
				}
			}
			if e != nil {
				return e
			}
			if arch := platform.Text(object(node["resources"])["architecture"]); arch != "" && arch != platform.Text(target["architecture"]) {
				return invalid("所选节点架构与镜像不兼容。")
			}
			if b.GPU.Model != nil && !platform.Bool(target["gpu_ready"]) {
				return missingReference("GPU-ready image target")
			}
		}
		covered[nodeID] = true
	}
	for id := range nodes {
		if !covered[id] {
			return invalid("node_ids 中每个节点至少需要一个兼容的所选网络。")
		}
	}
	return nil
}
