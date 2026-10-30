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
		q := platform.DB(c, a).Table("node_network_bindings").Select("id,node_id,network_pool_id,mode,parent_interface,incus_network_name,project_name,mtu,status,validation_status,validated_at,revision,created_at,updated_at")
		for param, col := range map[string]string{"node_id": "node_id", "pool_id": "network_pool_id"} {
			if s := c.Query(param); s != "" {
				id, e := platform.IDString(s)
				if e != nil {
					return nil, e
				}
				q = q.Where(col+"=?", id)
			}
		}
		return platform.List(c, a, q, []string{"node_id", "pool_id"})
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
		n, e := platform.Get(db, "nodes", node, true)
		if e != nil {
			return nil, e
		}
		p, e := platform.Get(db, "network_pools", pool, true)
		if e != nil {
			return nil, e
		}
		if platform.Uint(n["availability_zone_id"]) != platform.Uint(p["availability_zone_id"]) {
			return nil, networkError("node and pool zones differ")
		}
		if !networkName(in.Project) || ((in.Mode == "bridge" || in.Mode == "macvlan") && (!networkName(in.Parent) || in.Network != "")) || (in.Mode == "managed_bridge" && (!networkName(in.Network) || in.Parent != "")) || (in.Mode != "bridge" && in.Mode != "macvlan" && in.Mode != "managed_bridge") {
			return nil, networkError("invalid binding target, project or mode")
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
			return nil, networkError("invalid status")
		}
		if in.Status == "active" {
			p, e := platform.Get(platform.DB(c, a), "network_pools", platform.Uint(r["network_pool_id"]), true)
			if e != nil {
				return nil, e
			}
			if platform.Text(r["validation_status"]) != "verified" || platform.Text(p["status"]) != "active" {
				return nil, platform.E(409, "network_unverified", "binding requires successful read-only validation and an active pool")
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
		return networkError("validation node mismatch")
	}
	if reconcile {
		revision = platform.Uint(b["revision"])
	} else if revision != platform.Uint(b["revision"]) || platform.Text(b["validation_status"]) != "pending" {
		return platform.E(409, "stale_validation", "binding changed before validation")
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
		return platform.E(409, "stale_validation", "binding or pool changed during validation")
	}
	if e = a.TaskResult(ctx, j, platform.Row{"binding_id": fmt.Sprint(id), "validation_status": status}); e != nil {
		return e
	}
	return validationErr
}
func networkInspect(ctx context.Context, a *platform.App, node uint64, b, p platform.Row) (platform.Row, error) {
	fail := func() (platform.Row, error) { return nil, networkError("read-only network verification failed") }
	resources, e := a.Agent(ctx, node, "GET", "/agent/v1/resources", nil)
	if e != nil {
		return nil, e
	}
	if errors := networkArray(resources["network_errors"]); len(errors) > 0 {
		return fail()
	}
	interfaces, ok := resources["network_interfaces"].([]any)
	if !ok {
		return fail()
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
			return fail()
		}
		for _, v := range networkArray(iface["addresses"]) {
			address := networkMap(v)
			ip, err := netip.ParseAddr(platform.Text(address["address"]))
			if err != nil {
				return fail()
			}
			if ip.BitLen() == start.BitLen() && ip.Compare(start) >= 0 && ip.Compare(end) <= 0 {
				return fail()
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
		return fail()
	}
	project := url.QueryEscape(platform.Text(b["project_name"]))
	response, e := a.Agent(ctx, node, "GET", "/1.0/networks?recursion=1&project="+project, nil)
	if e != nil {
		return nil, e
	}
	networks, ok := response["metadata"].([]any)
	if !ok {
		return fail()
	}
	targetSeen := false
	for _, v := range networks {
		n := networkMap(v)
		if n == nil {
			return fail()
		}
		config := networkMap(n["config"])
		name := platform.Text(n["name"])
		isTarget := name == target
		if isTarget {
			targetSeen = true
			if mode == "macvlan" {
				if platform.Bool(n["managed"]) {
					return fail()
				}
			} else if platform.Text(n["type"]) != "bridge" {
				return fail()
			}
			if mode == "managed_bridge" && !platform.Bool(n["managed"]) {
				return fail()
			}
		}
		if platform.Bool(n["managed"]) {
			leases, e := a.Agent(ctx, node, "GET", "/1.0/networks/"+url.PathEscape(name)+"/leases?project="+project, nil)
			if e != nil {
				return nil, e
			}
			entries, ok := leases["metadata"].([]any)
			if !ok {
				return fail()
			}
			for _, entry := range entries {
				lease := networkMap(entry)
				ip, err := netip.ParseAddr(platform.Text(lease["address"]))
				if err != nil {
					return fail()
				}
				if ip.BitLen() == start.BitLen() && ip.Compare(start) >= 0 && ip.Compare(end) <= 0 {
					return fail()
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
					return fail()
				}
				if mode == "managed_bridge" {
					if platform.Text(config[family+".routing"]) == "false" {
						return fail()
					}
					reachable = reachable && subnet.Contains(networkAddr(p["gateway"]))
				}
			}
			if overlaps && platform.Text(config[family+".dhcp"]) != "false" {
				ranges := platform.Text(config[family+".dhcp.ranges"])
				if ranges == "" {
					return fail()
				}
				for _, r := range strings.Split(ranges, ",") {
					parts := strings.Split(strings.TrimSpace(r), "-")
					if len(parts) != 2 {
						return fail()
					}
					lo, e1 := netip.ParseAddr(strings.TrimSpace(parts[0]))
					hi, e2 := netip.ParseAddr(strings.TrimSpace(parts[1]))
					if e1 != nil || e2 != nil || lo.BitLen() != start.BitLen() || lo.Compare(hi) > 0 || lo.Compare(end) <= 0 && hi.Compare(start) >= 0 {
						return fail()
					}
				}
			}
		}
	}
	if !targetSeen || !reachable {
		return fail()
	}
	return platform.Row{"direct": true, "nat": false, "host_addresses_excluded": true, "dhcp_ranges_excluded": true, "reachable": true, "mode": mode}, nil
}
