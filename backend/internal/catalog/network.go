package catalog

import (
	"101cloud/backend/internal/platform"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"net/netip"
	"strconv"
	"strings"
)

func networkError(message string) error { return platform.E(422, "invalid_network", message) }
func networkAddr(v any) netip.Addr {
	b, ok := v.([]byte)
	if !ok {
		b = []byte(platform.Text(v))
	}
	a, _ := netip.AddrFromSlice(b)
	return a.Unmap()
}
func networkPrefix(r platform.Row) netip.Prefix {
	return netip.PrefixFrom(networkAddr(r["network_address"]), int(platform.Uint(r["prefix_length"])))
}
func networkPublic(r platform.Row, pool bool) platform.Row {
	if !pool {
		out := platform.Public(r, "id", "node_id", "network_pool_id", "mode", "parent_interface", "incus_network_name", "project_name", "mtu", "status", "validation_status", "validated_at", "revision", "created_at", "updated_at")
		out["validation_details"] = jsonValue(r["validation_details"])
		return out
	}
	out := platform.Public(r, "id", "availability_zone_id", "name", "address_family", "status", "revision", "created_at", "updated_at")
	out["cidr"] = networkPrefix(r).String()
	for _, k := range []string{"range_start", "range_end", "gateway"} {
		if r[k] != nil {
			out[k] = networkAddr(r[k]).String()
		} else {
			out[k] = nil
		}
	}
	var dns []string
	_ = json.Unmarshal([]byte(platform.Text(r["dns_servers"])), &dns)
	out["dns_servers"] = dns
	return out
}
func ValidateNetworkOption(db *gorm.DB, id, zoneID uint64, instanceType string) (platform.Row, error) {
	if instanceType != "container" && instanceType != "virtual-machine" {
		return nil, networkError("unsupported instance type")
	}
	var rows []platform.Row
	q := db.Table("node_network_bindings b").Select("b.*, p.availability_zone_id").Joins("JOIN network_pools p ON p.id=b.network_pool_id").Joins("JOIN nodes n ON n.id=b.node_id").Where("b.id=? AND p.availability_zone_id=? AND n.availability_zone_id=? AND b.status='active' AND b.validation_status='verified' AND p.status='active'", id, zoneID, zoneID)
	if instanceType == "virtual-machine" {
		q = q.Where("b.mode <> 'macvlan'")
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, platform.WrapDB(err)
	}
	if len(rows) != 1 {
		return nil, platform.E(409, "network_unavailable", "network option is not active and verified for this zone and instance type")
	}
	return rows[0], nil
}
func registerNetworks(a *platform.App, user, admin *gin.RouterGroup) {
	a.RegisterJob("network.validate", networkValidateJob)
	a.RegisterReconciler("network.validate", networkReconcileJob)
	user.GET("/catalog/network-options", platform.Handle(func(c *gin.Context) (any, error) {
		zone, e := platform.IDString(c.Query("zone_id"))
		if e != nil {
			return nil, e
		}
		typ := c.Query("instance_type")
		if typ != "container" && typ != "virtual-machine" {
			return nil, networkError("instance_type required")
		}
		q := platform.DB(c, a).Table("node_network_bindings b").Select("b.id,b.mode,b.created_at").Joins("JOIN network_pools p ON p.id=b.network_pool_id").Joins("JOIN nodes n ON n.id=b.node_id").Where("p.availability_zone_id=? AND n.availability_zone_id=? AND p.status='active' AND b.status='active' AND b.validation_status='verified'", zone, zone)
		if typ == "virtual-machine" {
			q = q.Where("b.mode <> 'macvlan'")
		}
		return platform.List(c, a, q, []string{"zone_id", "instance_type"})
	}))
	admin.GET("/network-pools", platform.Handle(func(c *gin.Context) (any, error) {
		q := platform.DB(c, a).Table("network_pools")
		if s := c.Query("zone_id"); s != "" {
			id, e := platform.IDString(s)
			if e != nil {
				return nil, e
			}
			q = q.Where("availability_zone_id=?", id)
		}
		return listDTO(c, a, q, []string{"zone_id"}, func(r platform.Row) platform.Row { return networkPublic(r, true) })
	}))
	admin.POST("/network-pools", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			ZoneID  string   `json:"zone_id"`
			Name    string   `json:"name"`
			CIDR    string   `json:"cidr"`
			Start   string   `json:"range_start"`
			End     string   `json:"range_end"`
			Gateway string   `json:"gateway"`
			DNS     []string `json:"dns_servers"`
		}
		if e := platform.Decode(c, &in); e != nil {
			return nil, e
		}
		zone, e := platform.IDString(in.ZoneID)
		if e != nil {
			return nil, e
		}
		db := platform.DB(c, a)
		if _, e = platform.Get(db, "availability_zones", zone, true); e != nil {
			return nil, e
		}
		p, e := netip.ParsePrefix(in.CIDR)
		if e != nil || p.Addr().Is4In6() || p != p.Masked() {
			return nil, networkError("cidr must be a canonical network")
		}
		start, e := netip.ParseAddr(in.Start)
		if e != nil {
			return nil, networkError("invalid range start")
		}
		end, e := netip.ParseAddr(in.End)
		if e != nil || !p.Contains(start) || !p.Contains(end) || start.Compare(end) > 0 || start.Is4In6() || end.Is4In6() {
			return nil, networkError("invalid address range")
		}
		if !networkUsable(p, start) || !networkUsable(p, end) {
			return nil, networkError("range includes reserved subnet addresses")
		}
		var gateway any
		if in.Gateway != "" {
			g, e := netip.ParseAddr(in.Gateway)
			if e != nil || !p.Contains(g) || !networkUsable(p, g) || g.Compare(start) >= 0 && g.Compare(end) <= 0 {
				return nil, networkError("gateway must be in subnet and outside allocation range")
			}
			gateway = g.AsSlice()
		}
		if strings.TrimSpace(in.Name) == "" || len(in.Name) > 128 {
			return nil, networkError("invalid name")
		}
		dns, e := networkDNS(in.DNS, p.Addr().Is4())
		if e != nil {
			return nil, e
		}
		var existing []platform.Row
		if e = db.Table("network_pools").Where("availability_zone_id=? AND address_family=?", zone, networkFamily(p.Addr())).Find(&existing).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		for _, r := range existing {
			if networkPrefix(r).Overlaps(p) {
				return nil, networkError("pool overlaps an existing subnet")
			}
		}
		r := platform.Row{"availability_zone_id": zone, "name": in.Name, "address_family": networkFamily(p.Addr()), "network_address": p.Addr().AsSlice(), "prefix_length": p.Bits(), "range_start": start.AsSlice(), "range_end": end.AsSlice(), "gateway": gateway, "dns_servers": dns, "status": "active", "revision": 1}
		if e = db.Table("network_pools").Create(&r).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		r, e = platform.Get(db, "network_pools", platform.Uint(r["@id"]), false)
		if e != nil {
			return nil, e
		}
		platform.Reply(c, 201, networkPublic(r, true))
		return nil, nil
	}))
	admin.GET("/network-pools/:id", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := networkGet(c, a, "network_pools", false)
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		return networkPublic(r, true), nil
	}))
	admin.PATCH("/network-pools/:id", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := networkGet(c, a, "network_pools", true)
		if e != nil {
			return nil, e
		}
		var in struct {
			Name    *string   `json:"name"`
			Status  *string   `json:"status"`
			Gateway *string   `json:"gateway"`
			DNS     *[]string `json:"dns_servers"`
		}
		if e = platform.Decode(c, &in); e != nil {
			return nil, e
		}
		changes := platform.Row{}
		if in.Name != nil {
			if strings.TrimSpace(*in.Name) == "" || len(*in.Name) > 128 {
				return nil, networkError("invalid name")
			}
			changes["name"] = *in.Name
		}
		if in.Status != nil {
			if *in.Status != "active" && *in.Status != "disabled" {
				return nil, networkError("invalid status")
			}
			changes["status"] = *in.Status
		}
		if in.Gateway != nil || in.DNS != nil {
			var count int64
			if e = platform.DB(c, a).Table("ip_addresses").Where("network_pool_id=? AND status <> 'available'", r["id"]).Count(&count).Error; e != nil {
				return nil, e
			}
			if count > 0 {
				return nil, platform.E(409, "pool_in_use", "network semantics cannot change while addresses are allocated")
			}
			p := networkPrefix(r)
			if in.Gateway != nil {
				g, err := netip.ParseAddr(*in.Gateway)
				if err != nil || !p.Contains(g) || !networkUsable(p, g) || g.Compare(networkAddr(r["range_start"])) >= 0 && g.Compare(networkAddr(r["range_end"])) <= 0 {
					return nil, networkError("invalid gateway")
				}
				changes["gateway"] = g.AsSlice()
			}
			if in.DNS != nil {
				d, err := networkDNS(*in.DNS, p.Addr().Is4())
				if err != nil {
					return nil, err
				}
				changes["dns_servers"] = d
			}
			if e = platform.DB(c, a).Table("node_network_bindings").Where("network_pool_id=?", r["id"]).Updates(platform.Row{"status": "disabled", "validation_status": "pending", "validated_at": nil, "revision": gorm.Expr("revision + 1")}).Error; e != nil {
				return nil, e
			}
		}
		r, e = platform.Save(c, a, "network_pools", r, changes)
		if e != nil {
			return nil, e
		}
		return networkPublic(r, true), nil
	}))
	admin.GET("/network-pools/:id/addresses", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := networkGet(c, a, "network_pools", false)
		if e != nil {
			return nil, e
		}
		q := platform.DB(c, a).Table("ip_addresses").Select("id,network_pool_id,address_family,INET6_NTOA(address) AS address,status,node_id,node_network_binding_id,instance_id,reservation_task_id,revision,created_at,updated_at").Where("network_pool_id=?", r["id"])
		if s := c.Query("status"); s != "" {
			q = q.Where("status=?", s)
		}
		return platform.List(c, a, q, []string{"status"})
	}))
	admin.POST("/network-pools/:id/addresses", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := networkGet(c, a, "network_pools", true)
		if e != nil {
			return nil, e
		}
		if e = platform.Revision(c, r); e != nil {
			return nil, e
		}
		var in struct {
			Addresses []string `json:"addresses"`
		}
		if e = platform.Decode(c, &in); e != nil {
			return nil, e
		}
		if len(in.Addresses) < 1 || len(in.Addresses) > 1000 {
			return nil, networkError("addresses requires 1–1000 entries")
		}
		seen := map[netip.Addr]bool{}
		rows := make([]platform.Row, 0, len(in.Addresses))
		for _, s := range in.Addresses {
			ip, err := netip.ParseAddr(s)
			if err != nil || ip.Is4In6() || !networkPrefix(r).Contains(ip) || !networkUsable(networkPrefix(r), ip) || ip.Compare(networkAddr(r["range_start"])) < 0 || ip.Compare(networkAddr(r["range_end"])) > 0 || ip == networkAddr(r["gateway"]) || seen[ip] {
				return nil, networkError("invalid, reserved or repeated address")
			}
			seen[ip] = true
			rows = append(rows, platform.Row{"network_pool_id": r["id"], "address_family": r["address_family"], "address": ip.AsSlice(), "status": "available", "revision": 1})
		}
		if e = platform.DB(c, a).Table("ip_addresses").Create(&rows).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		if _, e = platform.Save(c, a, "network_pools", r, platform.Row{}); e != nil {
			return nil, e
		}
		platform.Reply(c, 201, platform.Row{"registered": strconv.Itoa(len(rows))})
		return nil, nil
	}))
	networkRegisterBindings(a, admin)
}
func networkGet(c *gin.Context, a *platform.App, table string, lock bool) (platform.Row, error) {
	id, e := platform.ID(c, "id")
	if e != nil {
		return nil, e
	}
	return platform.Get(platform.DB(c, a), table, id, lock)
}
func networkFamily(a netip.Addr) int {
	if a.Is4() {
		return 4
	}
	return 6
}
func networkUsable(p netip.Prefix, a netip.Addr) bool {
	if !a.IsGlobalUnicast() || a == p.Addr() {
		return false
	}
	if a.Is4() && p.Bits() < 31 {
		b := a.As4()
		n := p.Addr().As4()
		x := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		y := uint32(n[0])<<24 | uint32(n[1])<<16 | uint32(n[2])<<8 | uint32(n[3])
		return x != y|(^uint32(0)>>p.Bits())
	}
	return true
}
func networkDNS(dns []string, v4 bool) (string, error) {
	if len(dns) > 16 {
		return "", networkError("too many DNS servers")
	}
	seen := map[netip.Addr]bool{}
	for i, s := range dns {
		ip, e := netip.ParseAddr(s)
		if e != nil || ip.Is4In6() || ip.Is4() != v4 || !ip.IsGlobalUnicast() || seen[ip] {
			return "", networkError("invalid DNS address or family")
		}
		seen[ip] = true
		dns[i] = ip.String()
	}
	if dns == nil {
		dns = []string{}
	}
	b, e := json.Marshal(dns)
	return string(b), e
}
