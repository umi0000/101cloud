package catalog

import (
	"101cloud/backend/internal/platform"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"net/netip"
	"net/url"
	"strings"
)

func networkRegisterBindings(a *platform.App, admin *gin.RouterGroup) {
	admin.GET("/node-network-bindings", platform.Handle(func(c *gin.Context) (any, error) {
		q := platform.DB(c, a).Table("node_network_bindings")
		for param, col := range map[string]string{"node_id": "node_id", "pool_id": "network_pool_id"} {
			if s := c.Query(param); s != "" {
				id, e := platform.IDString(s)
				if e != nil {
					return nil, e
				}
				q = q.Where(col+"=?", id)
			}
		}
		return listDTO(c, a, q, []string{"node_id", "pool_id"}, func(r platform.Row) platform.Row { return networkPublic(r, false) })
	}))
	admin.POST("/node-network-bindings", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			Node    string `json:"node_id"`
			Pool    string `json:"pool_id"`
			Project string `json:"project_name"`
			Mode    string `json:"mode"`
			Parent  string `json:"parent_interface"`
			Network string `json:"incus_network_name"`
		}
		if e := platform.Decode(c, &in); e != nil {
			return nil, e
		}
		node, e := platform.IDString(in.Node)
		if e != nil {
			return nil, e
		}
		pool, e := platform.IDString(in.Pool)
		if e != nil {
			return nil, e
		}
		db := platform.DB(c, a)
		if _, e = platform.Get(db, "nodes", node, true); e != nil {
			return nil, e
		}
		if _, e = platform.Get(db, "network_pools", pool, true); e != nil {
			return nil, e
		}
		if !networkName(in.Project) || ((in.Mode == "bridge" || in.Mode == "macvlan") && (!networkName(in.Parent) || in.Network != "")) || (in.Mode == "managed_bridge" && (!networkName(in.Network) || in.Parent != "")) || (in.Mode != "bridge" && in.Mode != "macvlan" && in.Mode != "managed_bridge") {
			return nil, networkError("网络绑定目标、项目或模式无效：bridge/macvlan 需要父接口，managed_bridge 需要 Incus 网络名。")
		}
		r := platform.Row{"node_id": node, "network_pool_id": pool, "project_name": in.Project, "mode": in.Mode, "status": "disabled", "validation_status": "pending", "validation_details": "{}", "revision": 1}
		if in.Parent != "" {
			r["parent_interface"] = in.Parent
		}
		if in.Network != "" {
			r["incus_network_name"] = in.Network
		}
		if e = db.Table("node_network_bindings").Create(&r).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		r, e = platform.Get(db, "node_network_bindings", platform.Uint(r["@id"]), false)
		if e != nil {
			return nil, e
		}
		platform.Reply(c, 201, networkPublic(r, false))
		return nil, nil
	}))
	admin.GET("/node-network-bindings/:id", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := networkGet(c, a, "node_network_bindings", false)
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		return networkPublic(r, false), nil
	}))
	admin.PATCH("/node-network-bindings/:id", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := networkGet(c, a, "node_network_bindings", true)
		if e != nil {
			return nil, e
		}
		var in struct {
			Status string `json:"status"`
		}
		if e = platform.Decode(c, &in); e != nil {
			return nil, e
		}
		if in.Status != "active" && in.Status != "disabled" {
			return nil, networkError("status 必须为 active 或 disabled。")
		}
		if in.Status == "active" {
			p, e := platform.Get(platform.DB(c, a), "network_pools", platform.Uint(r["network_pool_id"]), true)
			if e != nil {
				return nil, e
			}
			if platform.Text(r["validation_status"]) != "verified" || platform.Text(p["status"]) != "active" {
				return nil, platform.E(409, "network_unverified", "请先完成网络绑定的只读验证，并启用对应地址池，再启用绑定。")
			}
		}
		r, e = platform.Save(c, a, "node_network_bindings", r, platform.Row{"status": in.Status})
		if e != nil {
			return nil, e
		}
		return networkPublic(r, false), nil
	}))
	admin.POST("/node-network-bindings/:id/validate", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := networkGet(c, a, "node_network_bindings", true)
		if e != nil {
			return nil, e
		}
		var in struct{}
		if e = platform.Decode(c, &in); e != nil {
			return nil, e
		}
		r, e = platform.Save(c, a, "node_network_bindings", r, platform.Row{"status": "disabled", "validation_status": "pending", "validated_at": nil})
		if e != nil {
			return nil, e
		}
		task, e := platform.Enqueue(platform.DB(c, a), "network.validate", platform.Current(c).ID, platform.Uint(r["node_id"]), 0, false, platform.Row{"binding_id": r["id"], "revision": r["revision"]})
		if e != nil {
			return nil, e
		}
		platform.Reply(c, 202, task)
		return nil, nil
	}))
}
func networkName(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return s != "." && s != ".."
}
func networkMap(v any) map[string]any { r, _ := v.(map[string]any); return r }
func networkArray(v any) []any        { r, _ := v.([]any); return r }
func networkMetadata(r map[string]any) map[string]any {
	if m := networkMap(r["metadata"]); m != nil {
		return m
	}
	return r
}
func networkValidateJob(ctx context.Context, a *platform.App, j platform.Job) error {
	return networkRunValidation(ctx, a, j, false)
}
func networkReconcileJob(ctx context.Context, a *platform.App, j platform.Job) error {
	return networkRunValidation(ctx, a, j, true)
}
func networkRunValidation(ctx context.Context, a *platform.App, j platform.Job, reconcile bool) error {
	var raw map[string]any
	if e := json.Unmarshal(j.Parameters, &raw); e != nil {
		return e
	}
	id := platform.Uint(raw["binding_id"])
	revision := platform.Uint(raw["revision"])
	b, e := platform.Get(a.DB.WithContext(ctx), "node_network_bindings", id, false)
	if e != nil {
		return e
	}
	if platform.Uint(b["node_id"]) != j.NodeID {
		return networkError("验证任务节点与网络绑定节点不匹配，请重新提交验证。")
	}
	if reconcile {
		revision = platform.Uint(b["revision"])
	} else if revision != platform.Uint(b["revision"]) || platform.Text(b["validation_status"]) != "pending" {
		return platform.E(409, "stale_validation", "网络绑定在验证前已变更，请刷新并重新验证。")
	}
	p, e := platform.Get(a.DB.WithContext(ctx), "network_pools", platform.Uint(b["network_pool_id"]), false)
	if e != nil {
		return e
	}
	detail, validationErr := networkInspect(ctx, a, j.NodeID, b, p)
	status := "verified"
	if validationErr != nil {
		status = "failed"
		detail = platform.Row{"error": "network could not be verified as direct, reachable, and outside host/DHCP allocations"}
	}
	encoded, e := json.Marshal(detail)
	if e != nil {
		return e
	}
	q := a.DB.WithContext(ctx).Table("node_network_bindings").Where("id=? AND revision=? AND EXISTS (SELECT 1 FROM network_pools WHERE id=? AND revision=?)", id, revision, p["id"], p["revision"])
	if !reconcile {
		q = q.Where("validation_status='pending'")
	}
	result := q.Updates(platform.Row{"validation_status": status, "validated_at": a.Time(), "validation_details": string(encoded), "status": "disabled", "revision": gorm.Expr("revision + 1")})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return platform.E(409, "stale_validation", "网络绑定或地址池在验证期间已变更，请刷新并重新验证。")
	}
	if e = a.TaskResult(ctx, j, platform.Row{"binding_id": fmt.Sprint(id), "validation_status": status}); e != nil {
		return e
	}
	return validationErr
}
func networkInspect(ctx context.Context, a *platform.App, node uint64, b, p platform.Row) (platform.Row, error) {
	fail := func(message string) (platform.Row, error) {
		return nil, platform.EDetails(422, "invalid_network", message, platform.Row{"stage": "network_validation", "action": "请管理员修正对应网络配置后重新执行只读验证；探测结果不代表地址可分配，仍须登记地址库存。"})
	}
	resources, e := a.Agent(ctx, node, "GET", "/agent/v1/resources", nil)
	if e != nil {
		return nil, e
	}
	if errors := networkArray(resources["network_errors"]); len(errors) > 0 {
		return fail("节点报告网络采集异常，请检查节点接口采集状态。")
	}
	interfaces, ok := resources["network_interfaces"].([]any)
	if !ok {
		return fail("缺少完整的网络接口库存，请等待节点采集。")
	}
	start, end := networkAddr(p["range_start"]), networkAddr(p["range_end"])
	prefix := networkPrefix(p)
	target := platform.Text(b["parent_interface"])
	mode := platform.Text(b["mode"])
	if mode == "managed_bridge" {
		target = platform.Text(b["incus_network_name"])
	}
	reachable, found := false, false
	for _, value := range interfaces {
		iface := networkMap(value)
		if iface == nil {
			return fail("网络接口库存结构不完整，请检查节点采集。")
		}
		for _, v := range networkArray(iface["addresses"]) {
			address := networkMap(v)
			ip, err := netip.ParseAddr(platform.Text(address["address"]))
			if err != nil {
				return fail("接口地址观测格式无效，请管理员检查节点地址配置。")
			}
			if ip.BitLen() == start.BitLen() && ip.Compare(start) >= 0 && ip.Compare(end) <= 0 {
				return fail("实例分配地址范围包含节点自身地址，请调整地址池范围排除主机地址。")
			}
			if platform.Text(iface["name"]) == target && prefix.Contains(ip) {
				reachable = true
			}
		}
		if platform.Text(iface["name"]) == target {
			found = platform.Bool(iface["up"]) && !platform.Bool(iface["loopback"]) && platform.Text(iface["error"]) == ""
		}
	}
	if !found {
		return fail("目标接口不存在、未启用、为回环接口或存在观测错误，请检查绑定接口。")
	}
	project := url.QueryEscape(platform.Text(b["project_name"]))
	response, e := a.Agent(ctx, node, "GET", "/1.0/networks?recursion=1&project="+project, nil)
	if e != nil {
		return nil, e
	}
	networks, ok := response["metadata"].([]any)
	if !ok {
		return fail("缺少完整的 Incus 网络库存，请检查节点网络读取权限。")
	}
	targetSeen := false
	for _, v := range networks {
		n := networkMap(v)
		if n == nil {
			return fail("Incus 网络观测结构不完整，请检查节点网络采集。")
		}
		config := networkMap(n["config"])
		name := platform.Text(n["name"])
		isTarget := name == target
		if isTarget {
			targetSeen = true
			if mode == "macvlan" {
				if platform.Bool(n["managed"]) {
					return fail("macvlan 绑定目标不能是 Incus 托管网络，请选择主机接口。")
				}
			} else if platform.Text(n["type"]) != "bridge" {
				return fail("桥接模式的目标必须为 bridge 网络，请检查绑定模式。")
			}
			if mode == "managed_bridge" && !platform.Bool(n["managed"]) {
				return fail("managed_bridge 模式需要 Incus 托管的桥接网络。")
			}
		}
		if platform.Bool(n["managed"]) {
			leases, e := a.Agent(ctx, node, "GET", "/1.0/networks/"+url.PathEscape(name)+"/leases?project="+project, nil)
			if e != nil {
				return nil, e
			}
			entries, ok := leases["metadata"].([]any)
			if !ok {
				return fail("无法读取完整的 DHCP 租约信息，不能确认地址范围安全。")
			}
			for _, entry := range entries {
				lease := networkMap(entry)
				ip, err := netip.ParseAddr(platform.Text(lease["address"]))
				if err != nil {
					return fail("DHCP 租约地址格式无效，不能确认地址范围安全。")
				}
				if ip.BitLen() == start.BitLen() && ip.Compare(start) >= 0 && ip.Compare(end) <= 0 {
					return fail("分配范围与现有 DHCP 租约冲突，请排除租约地址或调整地址范围。")
				}
			}
		}
		if !platform.Bool(n["managed"]) {
			continue
		}
		for _, family := range []string{"ipv4", "ipv6"} {
			addr := platform.Text(config[family+".address"])
			subnet, err := netip.ParsePrefix(addr)
			if err != nil {
				continue
			}
			overlaps := subnet.Overlaps(prefix)
			if isTarget && overlaps {
				if platform.Text(config[family+".nat"]) == "true" || platform.Text(config[family+".nat"]) == "1" {
					return fail("目标网络启用了 NAT，不符合直连网络要求，请检查网络配置。")
				}
				if mode == "managed_bridge" {
					if platform.Text(config[family+".routing"]) == "false" {
						return fail("托管桥接网络禁用了路由，请检查网络路由配置。")
					}
					reachable = reachable && subnet.Contains(networkAddr(p["gateway"]))
				}
			}
			if overlaps && platform.Text(config[family+".dhcp"]) != "false" {
				ranges := platform.Text(config[family+".dhcp.ranges"])
				if ranges == "" {
					return fail("重叠子网启用了 DHCP 但未声明明确范围，请配置不与实例范围重叠的 DHCP 范围。")
				}
				for _, r := range strings.Split(ranges, ",") {
					parts := strings.Split(strings.TrimSpace(r), "-")
					if len(parts) != 2 {
						return fail("DHCP 范围格式无效，请管理员检查网络配置。")
					}
					lo, e1 := netip.ParseAddr(strings.TrimSpace(parts[0]))
					hi, e2 := netip.ParseAddr(strings.TrimSpace(parts[1]))
					if e1 != nil || e2 != nil || lo.BitLen() != start.BitLen() || lo.Compare(hi) > 0 || lo.Compare(end) <= 0 && hi.Compare(start) >= 0 {
						return fail("DHCP 范围无效或与实例地址范围冲突，请调整范围。")
					}
				}
			}
		}
	}
	if !targetSeen || !reachable {
		return fail("目标网络缺失或未确认子网可达，请检查接口地址、网关及绑定目标。")
	}
	return platform.Row{"direct": true, "nat": false, "host_addresses_excluded": true, "dhcp_ranges_excluded": true, "reachable": true, "mode": mode}, nil
}
