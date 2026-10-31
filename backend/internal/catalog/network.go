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
	out := platform.Public(r, "id", "name", "address_family", "status", "revision", "created_at", "updated_at")
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
func ValidateNetworkOption(db *gorm.DB, id uint64, instanceType string) (platform.Row, error) {
	if instanceType != "container" && instanceType != "virtual-machine" {
		return nil, networkError("不支持的实例类型，请使用 container 或 virtual-machine。")
	}
	var rows []platform.Row
	q := db.Table("node_network_bindings b").Select("b.*").Joins("JOIN network_pools p ON p.id=b.network_pool_id").Where("b.id=? AND b.status='active' AND b.validation_status='verified' AND p.status='active'", id)
	if instanceType == "virtual-machine" {
		q = q.Where("b.mode <> 'macvlan'")
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, platform.WrapDB(err)
	}
	if len(rows) != 1 {
		return nil, platform.E(409, "network_unavailable", "网络选项未启用、未通过验证，或不支持所选实例类型，请重新选择或联系管理员验证。")
	}
	return rows[0], nil
}
func registerNetworks(a *platform.App, user, admin *gin.RouterGroup) {
	a.RegisterJob("network.validate", networkValidateJob)
	a.RegisterReconciler("network.validate", networkReconcileJob)
	user.GET("/catalog/network-options", platform.Handle(func(c *gin.Context) (any, error) {
		typ := c.Query("instance_type")
		if typ != "container" && typ != "virtual-machine" {
			return nil, networkError("请指定有效的 instance_type：container 或 virtual-machine。")
		}
		q := platform.DB(c, a).Table("node_network_bindings b").Select("b.id,b.node_id,b.mode,b.created_at").Joins("JOIN network_pools p ON p.id=b.network_pool_id").Where("p.status='active' AND b.status='active' AND b.validation_status='verified'")
		if s := c.Query("node_id"); s != "" {
			node, e := platform.IDString(s)
			if e != nil {
				return nil, e
			}
			q = q.Where("b.node_id=?", node)
		}
		if typ == "virtual-machine" {
			q = q.Where("b.mode <> 'macvlan'")
		}
		return platform.List(c, a, q, []string{"node_id", "instance_type"})
	}))
	admin.GET("/network-pools", platform.Handle(func(c *gin.Context) (any, error) {
		return listDTO(c, a, platform.DB(c, a).Table("network_pools"), nil, func(r platform.Row) platform.Row { return networkPublic(r, true) })
	}))
	admin.POST("/network-pools", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
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
		db := platform.DB(c, a)
		p, e := netip.ParsePrefix(in.CIDR)
		if e != nil || p.Addr().Is4In6() || p != p.Masked() {
			return nil, networkError("cidr 必须为标准子网网络地址，例如 192.0.2.0/24。")
		}
		start, e := netip.ParseAddr(in.Start)
		if e != nil {
			return nil, networkError("range_start 不是有效的 IP 地址。")
		}
		end, e := netip.ParseAddr(in.End)
		if e != nil || !p.Contains(start) || !p.Contains(end) || start.Compare(end) > 0 || start.Is4In6() || end.Is4In6() {
			return nil, networkError("地址范围必须位于子网内，起始地址不得大于结束地址，且不能使用 IPv4 映射 IPv6 格式。")
		}
		if !networkUsable(p, start) || !networkUsable(p, end) {
			return nil, networkError("地址范围包含子网网络地址、广播地址或其他保留地址，请调整范围。")
		}
		var gateway any
		if in.Gateway != "" {
			g, e := netip.ParseAddr(in.Gateway)
			if e != nil || !p.Contains(g) || !networkUsable(p, g) || g.Compare(start) >= 0 && g.Compare(end) <= 0 {
				return nil, networkError("gateway 必须为子网内可用地址，且位于实例分配范围之外。")
			}
			gateway = g.AsSlice()
		}
		if strings.TrimSpace(in.Name) == "" || len(in.Name) > 128 {
			return nil, networkError("名称不能为空且不能超过 128 字节。")
		}
		dns, e := networkDNS(in.DNS, p.Addr().Is4())
		if e != nil {
			return nil, e
		}
		var existing []platform.Row
		if e = db.Table("network_pools").Where("address_family=?", networkFamily(p.Addr())).Find(&existing).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		for _, r := range existing {
			if networkPrefix(r).Overlaps(p) {
				return nil, networkError("地址池与已有子网重叠，请使用不重叠的子网。")
			}
		}
		r := platform.Row{"name": in.Name, "address_family": networkFamily(p.Addr()), "network_address": p.Addr().AsSlice(), "prefix_length": p.Bits(), "range_start": start.AsSlice(), "range_end": end.AsSlice(), "gateway": gateway, "dns_servers": dns, "status": "active", "revision": 1}
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
			Start   *string   `json:"range_start"`
			End     *string   `json:"range_end"`
		}
		if e = platform.Decode(c, &in); e != nil {
			return nil, e
		}
		changes := platform.Row{}
		if in.Name != nil {
			if strings.TrimSpace(*in.Name) == "" || len(*in.Name) > 128 {
				return nil, networkError("名称不能为空且不能超过 128 字节。")
			}
			changes["name"] = *in.Name
		}
		if in.Status != nil {
			if *in.Status != "active" && *in.Status != "disabled" {
				return nil, networkError("status 必须为 active 或 disabled。")
			}
			changes["status"] = *in.Status
		}
		p := networkPrefix(r)
		start, end := networkAddr(r["range_start"]), networkAddr(r["range_end"])
		rangeChanged := in.Start != nil || in.End != nil
		if rangeChanged {
			if in.Start != nil {
				start, e = netip.ParseAddr(*in.Start)
				if e != nil {
					return nil, networkError("range_start 不是有效的 IP 地址。")
				}
			}
			if in.End != nil {
				end, e = netip.ParseAddr(*in.End)
				if e != nil {
					return nil, networkError("range_end 不是有效的 IP 地址。")
				}
			}
			if start.Is4In6() || end.Is4In6() || !p.Contains(start) || !p.Contains(end) || start.Compare(end) > 0 {
				return nil, networkError("地址分配范围必须位于现有子网内，且起始地址不得大于结束地址。")
			}
			if !networkUsable(p, start) || !networkUsable(p, end) {
				return nil, networkError("allocation 地址范围包含子网网络地址、广播地址或其他保留地址，请调整范围。")
			}
			var outside int64
			if e = platform.DB(c, a).Table("ip_addresses").Where("network_pool_id=? AND (address < ? OR address > ?)", r["id"], start.AsSlice(), end.AsSlice()).Count(&outside).Error; e != nil {
				return nil, platform.WrapDB(e)
			}
			if outside > 0 {
				return nil, platform.E(409, "pool_range_in_use", "缩小范围不能排除已登记的 IP 地址，请保留完整库存范围。")
			}
			changes["range_start"], changes["range_end"] = start.AsSlice(), end.AsSlice()
		}
		if rangeChanged || in.Gateway != nil {
			gateway := networkAddr(r["gateway"])
			if in.Gateway != nil {
				gateway, e = netip.ParseAddr(*in.Gateway)
				if e != nil || gateway.Is4In6() || !p.Contains(gateway) || !networkUsable(p, gateway) {
					return nil, networkError("gateway 必须为子网内可用的标准 IP 地址。")
				}
			}
			if gateway.IsValid() && gateway.Compare(start) >= 0 && gateway.Compare(end) <= 0 {
				return nil, networkError("gateway 不能位于实例地址分配范围内，请调整网关或分配范围。")
			}
		}
		if in.Gateway != nil || in.DNS != nil {
			var count int64
			if e = platform.DB(c, a).Table("ip_addresses").Where("network_pool_id=? AND status <> 'available'", r["id"]).Count(&count).Error; e != nil {
				return nil, e
			}
			if count > 0 {
				return nil, platform.E(409, "pool_in_use", "已有地址被分配时不能修改网关或 DNS，请先安全释放分配。")
			}
			if in.Gateway != nil {
				g, err := netip.ParseAddr(*in.Gateway)
				if err != nil || !p.Contains(g) || !networkUsable(p, g) || g.Compare(start) >= 0 && g.Compare(end) <= 0 {
					return nil, networkError("gateway 必须为子网内可用的标准 IP 地址。")
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
		rows, e := networkAddressRows(r, in.Addresses)
		if e != nil {
			return nil, e
		}
		db := platform.DB(c, a)
		addresses := make([][]byte, 0, len(rows))
		for _, row := range rows {
			addresses = append(addresses, row["address"].([]byte))
		}
		var existing []platform.Row
		if e = db.Table("ip_addresses").Select("address").Where("network_pool_id=? AND address IN ?", r["id"], addresses).Find(&existing).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		registered := map[netip.Addr]bool{}
		for _, row := range existing {
			registered[networkAddr(row["address"])] = true
		}
		for i, row := range rows {
			if registered[networkAddr(row["address"])] {
				return nil, networkAddressError(r, i, in.Addresses[i], "already_registered", "该地址已在此地址池登记，本批次未写入任何地址", "请从本批次移除已登记地址；无需重复登记。", 409, "configuration_conflict")
			}
		}
		if e = db.Table("ip_addresses").Create(&rows).Error; e != nil {
			if api, ok := platform.WrapDB(e).(*platform.HTTPError); ok && api.Code == "configuration_conflict" {
				return nil, platform.EDetails(409, "configuration_conflict", "地址登记发生并发冲突，本批次未写入任何地址。", platform.Row{"field": "addresses", "reason": "already_registered", "action": "请刷新地址库存并移除已登记地址后重新提交。"})
			}
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
		return "", networkError("dns_servers 最多允许 16 个 DNS 地址。")
	}
	seen := map[netip.Addr]bool{}
	for i, s := range dns {
		ip, e := netip.ParseAddr(s)
		if e != nil || ip.Is4In6() || ip.Is4() != v4 || !ip.IsGlobalUnicast() || seen[ip] {
			return "", networkError("DNS 地址无效、重复或地址族与子网不一致，请检查 dns_servers。")
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

func networkAddressError(pool platform.Row, index int, submitted, reason, message, action string, status int, code string) error {
	return platform.EDetails(status, code, message+"："+submitted+"。", platform.Row{
		"field": "addresses[" + strconv.Itoa(index) + "]", "address": submitted, "reason": reason,
		"range_start": networkAddr(pool["range_start"]).String(), "range_end": networkAddr(pool["range_end"]).String(), "subnet": networkPrefix(pool).String(), "action": action,
	})
}

func networkAddressRows(pool platform.Row, addresses []string) ([]platform.Row, error) {
	if len(addresses) < 1 || len(addresses) > 1000 {
		return nil, platform.EDetails(422, "invalid_network", "addresses 必须包含 1–1000 个地址。", platform.Row{"field": "addresses", "action": "请调整本批次地址数量。"})
	}
	seen := map[netip.Addr]bool{}
	rows := make([]platform.Row, 0, len(addresses))
	for i, submitted := range addresses {
		ip, err := netip.ParseAddr(submitted)
		fail := func(reason, message, action string) ([]platform.Row, error) {
			return nil, networkAddressError(pool, i, submitted, reason, message, action, 422, "invalid_network")
		}
		if err != nil || ip.Is4In6() {
			return fail("invalid_address", "IP 地址格式无效（不接受 IPv4 映射 IPv6 格式）", "请提交标准 IPv4 或 IPv6 地址。")
		}
		if !networkPrefix(pool).Contains(ip) {
			return fail("outside_subnet", "地址不属于地址池子网", "请使用详情 subnet 内的地址。")
		}
		if ip == networkAddr(pool["gateway"]) {
			return fail("gateway", "网关地址不能登记为实例地址", "请排除网关地址，并使用地址池允许范围内的地址。")
		}
		if !networkUsable(networkPrefix(pool), ip) {
			return fail("reserved_address", "地址为子网网络地址、广播地址或其他保留地址", "请使用子网内可供实例使用的单播地址。")
		}
		if ip.Compare(networkAddr(pool["range_start"])) < 0 || ip.Compare(networkAddr(pool["range_end"])) > 0 {
			return fail("outside_range", "地址超出地址池允许登记范围", "请使用详情 range_start 至 range_end 内的地址，或先调整地址池范围。")
		}
		if seen[ip] {
			return fail("duplicate_in_batch", "本批次重复提交了同一地址", "请删除本批次内重复的地址后重新提交；本批次未写入任何地址。")
		}
		seen[ip] = true
		rows = append(rows, platform.Row{"network_pool_id": pool["id"], "address_family": pool["address_family"], "address": ip.AsSlice(), "status": "available", "revision": 1})
	}
	return rows, nil
}
