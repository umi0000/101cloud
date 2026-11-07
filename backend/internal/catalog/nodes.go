package catalog

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Connection credentials are write-only. Only the token is encrypted; the pin is
// the administrator-supplied trust anchor, never learned from a probe.
type nodeConnection struct {
	Endpoint         string `json:"endpoint"`
	Token            string `json:"token"`
	ServerCertSHA256 string `json:"server_cert_sha256"`
}
type nodeDefaults struct {
	StoragePool string `json:"storage_pool"`
	ProjectName string `json:"project_name"`
}
type nodeAllocatable struct {
	VCPUs               *uint64 `json:"vcpus"`
	MemoryBytes         string  `json:"memory_bytes"`
	DiskBytes           string  `json:"disk_bytes"`
	ResidentVCPUs       *uint64 `json:"resident_vcpus"`
	ResidentMemoryBytes string  `json:"resident_memory_bytes"`
	ResidentDiskBytes   string  `json:"resident_disk_bytes"`
}

const nodeGPUColumns = "node_gpus.id, node_gpus.node_id, node_gpus.uuid, node_gpus.model, node_gpus.pci_address, node_gpus.memory_mib, node_gpus.max_allocations, node_gpus.status, node_gpus.last_seen_at, node_gpus.revision, node_gpus.created_at, node_gpus.updated_at, (SELECT COUNT(*) FROM gpu_allocations ga WHERE ga.gpu_id=node_gpus.id AND ga.active_slot=1) AS allocation_count, (SELECT COUNT(*) FROM gpu_allocations ga WHERE ga.gpu_id=node_gpus.id AND ga.status='reserved') AS reservation_count"

func registerNodes(a *platform.App, admin *gin.RouterGroup) {
	a.RegisterJob("node.probe", nodeInventoryJob)
	a.RegisterJob("node.drain", nodeInventoryJob)
	a.RegisterReconciler("node.probe", nodeInventoryJob)
	a.RegisterReconciler("node.drain", nodeInventoryJob)
	a.AddPoller("node-inventory", platform.NodeInventoryPollInterval, pollNodeInventory)
	a.AddPoller("node-gpu-load", time.Second, pollNodeGPULoad)
	admin.GET("/nodes", platform.Handle(func(c *gin.Context) (any, error) {
		q := platform.DB(c, a).Table("nodes")
		for _, f := range []string{"administrative_state", "health"} {
			if s := c.Query(f); s != "" {
				if (f == "administrative_state" && !enum(s, "active", "disabled", "draining", "maintenance")) || (f == "health" && !enum(s, "unknown", "online", "offline")) {
					return nil, invalid("节点筛选值无效，请检查 administrative_state 或 health。")
				}
				q = q.Where(f+" = ?", s)
			}
		}
		return listDTO(c, a, q, []string{"administrative_state", "health"}, nodeDTO)
	}))
	admin.POST("/nodes", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			Name            string           `json:"name"`
			Connection      *nodeConnection  `json:"connection"`
			Defaults        *nodeDefaults    `json:"defaults"`
			Allocatable     *nodeAllocatable `json:"allocatable"`
			MetricsPolicyID string           `json:"metrics_policy_id"`
		}
		if err := platform.Decode(c, &in); err != nil {
			return nil, err
		}
		if !textValid(in.Name, 128) || in.Connection == nil || in.Defaults == nil || in.Allocatable == nil {
			return nil, invalid("请填写节点名称、连接配置、默认配置及可分配容量。")
		}
		if !textValid(in.Defaults.StoragePool, 255) || !codeValid(in.Defaults.ProjectName) || len(in.Defaults.ProjectName) > 63 {
			return nil, invalid("默认存储池或项目名无效，请检查 defaults。")
		}
		metricsPolicyID, err := nodeMetricsPolicy(platform.DB(c, a), a, in.MetricsPolicyID)
		if err != nil {
			return nil, err
		}
		row, err := nodeCapacityChanges(*in.Allocatable)
		if err != nil {
			return nil, err
		}
		conn, err := nodeConnectionChanges(a, *in.Connection)
		if err != nil {
			return nil, err
		}
		for k, v := range conn {
			row[k] = v
		}
		row["name"] = in.Name
		row["metrics_policy_id"] = metricsPolicyID
		row["default_storage_pool"] = in.Defaults.StoragePool
		row["default_project_name"] = in.Defaults.ProjectName
		row["status"] = "maintenance"
		row["administrative_state"] = "maintenance"
		row["health"] = "unknown"
		row["revision"] = 1
		created, err := insert(c, a, "nodes", row)
		if err != nil {
			return nil, err
		}
		platform.SetETag(c, created)
		platform.Reply(c, 201, nodeDTO(created))
		return nil, nil
	}))
	admin.GET("/nodes/:id", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := detail(c, a, "nodes")
		if e != nil {
			return nil, e
		}
		return nodeDTO(r), nil
	}))
	admin.PATCH("/nodes/:id", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			Name                *string          `json:"name"`
			AdministrativeState *string          `json:"administrative_state"`
			Allocatable         *nodeAllocatable `json:"allocatable"`
			MetricsPolicyID     json.RawMessage  `json:"metrics_policy_id"`
		}
		if err := platform.Decode(c, &in); err != nil {
			return nil, err
		}
		id, err := platform.ID(c, "id")
		if err != nil {
			return nil, err
		}
		r, err := platform.Get(platform.DB(c, a), "nodes", id, true)
		if err != nil {
			return nil, err
		}
		changes := platform.Row{}
		if len(in.MetricsPolicyID) != 0 {
			var value string
			if err := json.Unmarshal(in.MetricsPolicyID, &value); err != nil {
				return nil, invalid("metrics_policy_id 必须为已发布的 metrics 策略 ID。")
			}
			policyID, err := nodeMetricsPolicy(platform.DB(c, a), a, value)
			if err != nil {
				return nil, err
			}
			changes["metrics_policy_id"] = policyID
		}
		if in.Name != nil {
			if !textValid(*in.Name, 128) {
				return nil, invalid("节点名称不能为空且不能超过 128 个字符。")
			}
			changes["name"] = *in.Name
		}
		if in.AdministrativeState != nil {
			state := *in.AdministrativeState
			if !enum(state, "active", "disabled", "draining", "maintenance") {
				return nil, invalid("administrative_state 必须为 active、disabled、draining 或 maintenance。")
			}
			if state == "active" && (platform.Stamp(r["connection_verified_at"]).IsZero() || platform.Text(r["health"]) != "online" || platform.Stamp(r["resources_sampled_at"]).IsZero()) {
				return nil, platform.E(409, "node_unavailable", "请先成功探测当前连接并获取在线资源观测，再启用节点。")
			}
			changes["administrative_state"] = state
			changes["status"] = state
		}
		if in.Allocatable != nil {
			capacity, e := nodeCapacityChanges(*in.Allocatable)
			if e != nil {
				return nil, e
			}
			if e = nodeCheckCommitments(platform.DB(c, a), id, capacity); e != nil {
				return nil, e
			}
			for k, v := range capacity {
				changes[k] = v
			}
		}
		if len(changes) == 0 {
			return nil, invalid("请至少提交一个要更新的节点字段。")
		}
		saved, err := platform.Save(c, a, "nodes", r, changes)
		if err != nil {
			return nil, err
		}
		platform.SetETag(c, saved)
		return nodeDTO(saved), nil
	}))
	admin.PUT("/nodes/:id/connection", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			Connection *nodeConnection `json:"connection"`
		}
		if err := platform.Decode(c, &in); err != nil {
			return nil, err
		}
		if in.Connection == nil {
			return nil, invalid("请提供 connection 连接配置。")
		}
		id, err := platform.ID(c, "id")
		if err != nil {
			return nil, err
		}
		r, err := platform.Get(platform.DB(c, a), "nodes", id, true)
		if err != nil {
			return nil, err
		}
		changes, err := nodeConnectionChanges(a, *in.Connection)
		if err != nil {
			return nil, err
		}
		changes["connection_verified_at"] = nil
		changes["health"] = "unknown"
		changes["resources"] = nil
		changes["resources_sampled_at"] = nil
		changes["last_heartbeat_at"] = nil
		// Replacing a trust document cannot leave the node eligible for placement.
		if platform.Text(r["administrative_state"]) == "active" {
			changes["administrative_state"] = "maintenance"
			changes["status"] = "maintenance"
		}
		saved, err := platform.Save(c, a, "nodes", r, changes)
		if err != nil {
			return nil, err
		}
		if err = platform.DB(c, a).Table("node_gpu_load_snapshots").Where("node_id=?", id).Delete(&platform.Row{}).Error; err != nil {
			return nil, platform.WrapDB(err)
		}
		platform.SetETag(c, saved)
		return nodeDTO(saved), nil
	}))
	admin.POST("/nodes/:id/probe", platform.Handle(func(c *gin.Context) (any, error) {
		if err := requiredBody(c); err != nil {
			return nil, err
		}
		return nodeQueue(c, a, false)
	}))
	admin.POST("/nodes/:id/drain", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			Mode string `json:"mode"`
		}
		if err := platform.Decode(c, &in); err != nil {
			return nil, err
		}
		if in.Mode != "prevent_new_allocations" {
			return nil, invalid("mode 必须为 prevent_new_allocations，以阻止新增分配。")
		}
		return nodeQueue(c, a, true)
	}))
	admin.GET("/nodes/:id/resources", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := detail(c, a, "nodes")
		if e != nil {
			return nil, e
		}
		return platform.Row{"resources": nodeResources(r["resources"]), "observed_at": nodeTime(r["resources_sampled_at"]), "quality": nodeQuality(r), "health": platform.Text(r["health"])}, nil
	}))
	admin.GET("/nodes/:id/gpus", platform.Handle(func(c *gin.Context) (any, error) {
		id, e := platform.ID(c, "id")
		if e != nil {
			return nil, e
		}
		n, e := platform.Get(platform.DB(c, a), "nodes", id, false)
		if e != nil {
			return nil, e
		}
		db := platform.DB(c, a)
		q := db.Table("node_gpus").Select(nodeGPUColumns).Where("node_gpus.node_id = ?", id)
		resources := nodeResources(n["resources"])
		return platform.ListMap(c, a, q, nil, func(r platform.Row) (platform.Row, error) { return nodeGPUDTO(r, n, resources) })
	}))
	admin.GET("/nodes/:id/gpus/:gpu_id", platform.Handle(func(c *gin.Context) (any, error) {
		r, n, e := nodeGPU(c, a, false)
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		return nodeGPUDTO(r, n, nodeResources(n["resources"]))
	}))
	admin.PATCH("/nodes/:id/gpus/:gpu_id", platform.Handle(func(c *gin.Context) (any, error) {
		var in struct {
			Status         *string `json:"status"`
			MaxAllocations *uint64 `json:"max_allocations"`
		}
		if e := platform.Decode(c, &in); e != nil {
			return nil, e
		}
		r, n, e := nodeGPU(c, a, true)
		if e != nil {
			return nil, e
		}
		changes := platform.Row{}
		if in.Status != nil {
			if !enum(*in.Status, "available", "disabled", "maintenance") {
				return nil, invalid("GPU status 必须为 available、disabled 或 maintenance。")
			}
			changes["status"] = *in.Status
		}
		max := platform.Uint(r["max_allocations"])
		if in.MaxAllocations != nil {
			if *in.MaxAllocations == 0 || *in.MaxAllocations > 4294967295 {
				return nil, invalid("max_allocations 必须是大于零的 uint32 整数。")
			}
			max = *in.MaxAllocations
			changes["max_allocations"] = max
		}
		var used int64
		if e := platform.DB(c, a).Table("gpu_allocations").Where("gpu_id = ? AND active_slot = 1", platform.Uint(r["id"])).Count(&used).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		if max < uint64(used) {
			return nil, catalogCapacityError("gpu_in_use", "gpu", "gpus", "GPU 分配上限", "count", uint64(used), 0, max, "max_allocations", "请先安全释放有效、预留、释放中或状态未知的 GPU 分配，再降低上限。")
		}
		state := platform.Text(r["status"])
		if in.Status != nil {
			state = *in.Status
		}
		if state == "available" {
			if platform.Text(r["status"]) == "missing" || platform.Text(r["status"]) == "error" || platform.Text(n["health"]) != "online" || platform.Stamp(n["connection_verified_at"]).IsZero() || !nodeGPUObservedHealthy(r, n) {
				return nil, platform.E(409, "observation_incomplete", "启用 GPU 前需要已验证在线的节点及最新健康 GPU 观测，请检查节点与 GPU 状态。")
			}
		}
		if len(changes) == 0 {
			return nil, invalid("请至少提交一个要更新的 GPU 字段。")
		}
		saved, e := platform.Save(c, a, "node_gpus", r, changes)
		if e != nil {
			return nil, e
		}
		saved["allocation_count"] = uint64(used)
		var reserved int64
		if e := platform.DB(c, a).Table("gpu_allocations").Where("gpu_id = ? AND status = 'reserved'", platform.Uint(r["id"])).Count(&reserved).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		saved["reservation_count"] = uint64(reserved)
		platform.SetETag(c, saved)
		return nodeGPUDTO(saved, n, nodeResources(n["resources"]))
	}))
	admin.GET("/nodes/:id/unmanaged-instances", platform.Handle(func(c *gin.Context) (any, error) {
		id, e := platform.ID(c, "id")
		if e != nil {
			return nil, e
		}
		if _, e = platform.Get(platform.DB(c, a), "nodes", id, false); e != nil {
			return nil, e
		}
		q := platform.DB(c, a).Table("node_unmanaged_instances").Select("id,node_id,project_name,incus_name,incus_uuid,instance_type,architecture,observed_state,config,observed_at,revision,created_at,updated_at").Where("node_id = ?", id)
		q = q.Where("NOT EXISTS (SELECT 1 FROM instances i WHERE i.node_id=node_unmanaged_instances.node_id AND i.deleted_at IS NULL AND ((i.project_name=node_unmanaged_instances.project_name AND i.incus_name=node_unmanaged_instances.incus_name) OR (i.incus_uuid IS NOT NULL AND i.incus_uuid=node_unmanaged_instances.incus_uuid)))")
		return listDTO(c, a, q, nil, func(r platform.Row) platform.Row {
			out := platform.Public(r)
			out["config"] = jsonValue(r["config"])
			return out
		})
	}))
}

func nodeMetricsPolicy(db *gorm.DB, a *platform.App, value string) (uint64, error) {
	id, e := platform.IDString(value)
	if e != nil {
		return 0, invalid("metrics_policy_id 必须为已发布的 metrics 策略 ID。")
	}
	policy, e := platform.Get(db, "policy_versions", id, false)
	if e != nil {
		return 0, e
	}
	if platform.Text(policy["kind"]) != "metrics" || platform.Text(policy["status"]) != "active" || platform.Stamp(policy["effective_at"]).After(a.Time()) {
		return 0, invalid("metrics_policy_id 必须引用已发布且生效的 metrics 策略。")
	}
	return id, nil
}

func nodeConnectionChanges(a *platform.App, in nodeConnection) (platform.Row, error) {
	decoded, e := base64.RawURLEncoding.Strict().DecodeString(in.Token)
	if e != nil || len(decoded) != 32 || len(in.Token) != 43 {
		return nil, invalid("token 必须为 32 字节凭据的无填充 base64url 编码。")
	}
	pin, e := hex.DecodeString(in.ServerCertSHA256)
	if e != nil || len(pin) != 32 || strings.ToLower(in.ServerCertSHA256) != in.ServerCertSHA256 {
		return nil, invalid("server_cert_sha256 必须为 64 位小写十六进制 SHA-256 指纹。")
	}
	u, e := url.Parse(in.Endpoint)
	if e != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(in.Endpoint, "#") || strings.ContainsAny(u.Host, "\r\n\t %\\") || strings.HasSuffix(u.Host, ":") {
		return nil, invalid("endpoint 必须是 HTTPS 源地址，不能包含用户信息、路径、查询参数或片段。")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port != "" {
		p, e := strconv.Atoi(port)
		if e != nil || p < 1 || p > 65535 {
			return nil, invalid("endpoint 端口必须在 1–65535 范围内。")
		}
		if p == 443 {
			port = ""
		} else {
			port = strconv.Itoa(p)
		}
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Path = ""
	endpoint := u.String()
	if len(endpoint) > 512 {
		return nil, invalid("endpoint 不能超过 512 字节。")
	}
	if e = a.CheckEndpoint(endpoint); e != nil {
		return nil, e
	}
	ciphertext, e := a.Encrypt([]byte(in.Token))
	if e != nil {
		return nil, e
	}
	return platform.Row{"agent_endpoint": endpoint, "token_ciphertext": ciphertext, "server_cert_sha256": pin, "key_version": 1}, nil
}
func nodeCapacityChanges(in nodeAllocatable) (platform.Row, error) {
	parse := func(field, value string, positive bool) (uint64, error) {
		n, err := unsigned(value, positive)
		if err != nil {
			constraint := "非负"
			if positive {
				constraint = "大于零的"
			}
			return 0, platform.EDetails(422, "validation_failed", "allocatable."+field+" 必须为"+constraint+"十进制整数字符串。", platform.Row{"field": "allocatable." + field, "action": "请以字节为单位填写整数，不要附带 GiB 等单位。"})
		}
		return n, nil
	}
	if in.VCPUs == nil || in.ResidentVCPUs == nil || *in.VCPUs == 0 || *in.VCPUs > 4294967295 || *in.ResidentVCPUs > *in.VCPUs {
		return nil, invalid("vcpus 必须是正 uint32 整数，resident_vcpus 不能超过 vcpus。")
	}
	memory, e := parse("memory_bytes", in.MemoryBytes, true)
	if e != nil {
		return nil, e
	}
	disk, e := parse("disk_bytes", in.DiskBytes, true)
	if e != nil {
		return nil, e
	}
	rm, e := parse("resident_memory_bytes", in.ResidentMemoryBytes, false)
	if e != nil {
		return nil, e
	}
	rd, e := parse("resident_disk_bytes", in.ResidentDiskBytes, false)
	if e != nil {
		return nil, e
	}
	if rm > memory || rd > disk {
		return nil, invalid("常驻内存或磁盘预算不能超过对应可分配容量，请检查 resident_memory_bytes 和 resident_disk_bytes。")
	}
	return platform.Row{"allocatable_vcpus": *in.VCPUs, "allocatable_memory_bytes": memory, "allocatable_disk_bytes": disk, "resident_vcpu_budget": *in.ResidentVCPUs, "resident_memory_budget_bytes": rm, "resident_disk_budget_bytes": rd}, nil
}
func nodeCheckCommitments(db *gorm.DB, id uint64, capacity platform.Row) error {
	var rows []platform.Row
	if e := db.Table("instances").Select("billing_mode,vcpus,memory_bytes,disk_bytes").Where("node_id = ? AND deleted_at IS NULL AND provisioning_state <> 'deleted' AND NOT (provisioning_state='failed' AND incus_uuid IS NULL AND COALESCE(JSON_EXTRACT(incus_config_snapshot,'$.creation_reservations_released') = true, 0))", id).Find(&rows).Error; e != nil {
		return platform.WrapDB(e)
	}
	remaining := platform.Row{}
	for k, v := range capacity {
		remaining[k] = platform.Uint(v)
	}
	for _, r := range rows {
		for _, pair := range [][2]string{{"vcpus", "allocatable_vcpus"}, {"memory_bytes", "allocatable_memory_bytes"}, {"disk_bytes", "allocatable_disk_bytes"}} {
			v := platform.Uint(r[pair[0]])
			left := platform.Uint(remaining[pair[1]])
			if v > left {
				unit := "bytes"
				if pair[0] == "vcpus" {
					unit = "count"
				}
				return catalogCapacityError("capacity_in_use", "node_capacity", pair[0], "节点容量", unit, platform.Uint(capacity[pair[1]])-left, v, platform.Uint(capacity[pair[1]]), "allocatable."+pair[0], "新容量不能低于已承诺实例资源，请先释放资源或提高本次配置值。")
			}
			remaining[pair[1]] = left - v
		}
		if platform.Text(r["billing_mode"]) == "admin_exempt" {
			for _, pair := range [][2]string{{"vcpus", "resident_vcpu_budget"}, {"memory_bytes", "resident_memory_budget_bytes"}, {"disk_bytes", "resident_disk_budget_bytes"}} {
				v := platform.Uint(r[pair[0]])
				left := platform.Uint(remaining[pair[1]])
				if v > left {
					unit := "bytes"
					if pair[0] == "vcpus" {
						unit = "count"
					}
					return catalogCapacityError("capacity_in_use", "resident_capacity", pair[0], "常驻资源预算", unit, platform.Uint(capacity[pair[1]])-left, v, platform.Uint(capacity[pair[1]]), "allocatable.resident_"+pair[0], "新常驻预算不能低于已承诺资源，请先释放常驻资源或提高本次预算。")
				}
				remaining[pair[1]] = left - v
			}
		}
	}
	return nil
}
func nodeDTO(r platform.Row) platform.Row {
	p := platform.Public(r, "id", "name", "metrics_policy_id", "administrative_state", "health", "connection_verified_at", "resources_sampled_at", "last_heartbeat_at", "revision", "created_at", "updated_at")
	p["connection"] = platform.Row{"endpoint": platform.Text(r["agent_endpoint"]), "server_cert_sha256": hex.EncodeToString([]byte(platform.Text(r["server_cert_sha256"]))), "has_credentials": true, "key_version": platform.Uint(r["key_version"])}
	p["defaults"] = platform.Row{"storage_pool": platform.Text(r["default_storage_pool"]), "project_name": platform.Text(r["default_project_name"])}
	p["allocatable"] = platform.Row{"vcpus": platform.Uint(r["allocatable_vcpus"]), "memory_bytes": strconv.FormatUint(platform.Uint(r["allocatable_memory_bytes"]), 10), "disk_bytes": strconv.FormatUint(platform.Uint(r["allocatable_disk_bytes"]), 10), "resident_vcpus": platform.Uint(r["resident_vcpu_budget"]), "resident_memory_bytes": strconv.FormatUint(platform.Uint(r["resident_memory_budget_bytes"]), 10), "resident_disk_bytes": strconv.FormatUint(platform.Uint(r["resident_disk_budget_bytes"]), 10)}
	return p
}
func nodeTime(v any) any {
	t := platform.Stamp(v)
	if t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339Nano)
}
func nodeQuality(r platform.Row) string {
	if platform.Stamp(r["resources_sampled_at"]).IsZero() {
		return "unavailable"
	}
	if platform.Text(r["health"]) != "online" || platform.Stamp(r["connection_verified_at"]).IsZero() || !platform.InventoryFresh(time.Now(), r["resources_sampled_at"]) {
		return "stale"
	}
	return "cached"
}
func nodeQueue(c *gin.Context, a *platform.App, drain bool) (any, error) {
	id, e := platform.ID(c, "id")
	if e != nil {
		return nil, e
	}
	r, e := platform.Get(platform.DB(c, a), "nodes", id, true)
	if e != nil {
		return nil, e
	}
	kind := "node.probe"
	params := platform.Row{}
	if drain {
		if _, e = platform.Save(c, a, "nodes", r, platform.Row{"administrative_state": "draining", "status": "draining"}); e != nil {
			return nil, e
		}
		kind = "node.drain"
		params["mode"] = "prevent_new_allocations"
	}
	task, e := platform.Enqueue(platform.DB(c, a), kind, platform.Current(c).ID, id, 0, false, params)
	if e != nil {
		return nil, e
	}
	platform.Reply(c, 202, task)
	return nil, nil
}
func nodeGPU(c *gin.Context, a *platform.App, lock bool) (platform.Row, platform.Row, error) {
	id, e := platform.ID(c, "id")
	if e != nil {
		return nil, nil, e
	}
	gid, e := platform.ID(c, "gpu_id")
	if e != nil {
		return nil, nil, e
	}
	n, e := platform.Get(platform.DB(c, a), "nodes", id, lock)
	if e != nil {
		return nil, nil, e
	}
	r, e := platform.Get(platform.DB(c, a), "node_gpus", gid, lock)
	if e != nil {
		return nil, nil, e
	}
	if platform.Uint(r["node_id"]) != id {
		return nil, nil, platform.E(404, "not_found", "此节点上未找到指定 GPU，请刷新 GPU 列表。")
	}
	var counts struct {
		AllocationCount  uint64
		ReservationCount uint64
	}
	if e := platform.DB(c, a).Table("gpu_allocations").Select("COUNT(*) AS allocation_count, COALESCE(SUM(status='reserved'),0) AS reservation_count").Where("gpu_id = ? AND active_slot=1", gid).Scan(&counts).Error; e != nil {
		return nil, nil, platform.WrapDB(e)
	}
	r["allocation_count"] = counts.AllocationCount
	r["reservation_count"] = counts.ReservationCount
	return r, n, nil
}
func nodeGPUDTO(r, n platform.Row, resources map[string]any) (platform.Row, error) {
	p := platform.Public(r, "id", "node_id", "uuid", "model", "pci_address", "memory_mib", "max_allocations", "status", "last_seen_at", "revision", "created_at", "updated_at")
	p["allocation_count"] = platform.Uint(r["allocation_count"])
	p["reservation_count"] = platform.Uint(r["reservation_count"])
	p["observed_at"] = nodeTime(n["resources_sampled_at"])
	p["quality"] = nodeQuality(n)
	p["observation"] = nil
	if gpus, ok := resources["gpus"].([]any); ok {
		for _, v := range gpus {
			g, ok := v.(map[string]any)
			if ok && strings.EqualFold(platform.Text(g["uuid"]), platform.Text(r["uuid"])) {
				p["observation"] = g
				break
			}
		}
	}
	p["health"] = nodeGPUHealth(p, n)
	return p, nil
}

func nodeGPUObservedHealthy(r, n platform.Row) bool {
	if platform.Uint(r["memory_mib"]) == 0 || platform.Stamp(r["last_seen_at"]).IsZero() || !platform.Stamp(r["last_seen_at"]).Equal(platform.Stamp(n["resources_sampled_at"])) {
		return false
	}
	resources := nodeResources(n["resources"])
	gpus, _ := resources["gpus"].([]any)
	for _, v := range gpus {
		g, ok := v.(map[string]any)
		if !ok || !strings.EqualFold(platform.Text(g["uuid"]), platform.Text(r["uuid"])) {
			continue
		}
		_, valid := nodeMetric(g["load_percent"])
		return valid && platform.Text(g["error"]) == ""
	}
	return false
}

func nodeGPUHealth(r, n platform.Row) string {
	if platform.Text(n["health"]) != "online" || platform.Stamp(n["connection_verified_at"]).IsZero() {
		return "unknown"
	}
	if r["observation"] == nil {
		return "missing"
	}
	g, _ := r["observation"].(map[string]any)
	if platform.Text(g["error"]) != "" {
		return "error"
	}
	if _, ok := nodeMetric(g["load_percent"]); !ok {
		return "unknown"
	}
	return "healthy"
}

func nodeResources(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	var out map[string]any
	decoder := json.NewDecoder(strings.NewReader(platform.Text(v)))
	decoder.UseNumber()
	if decoder.Decode(&out) != nil {
		return nil
	}
	return out
}

func catalogCapacityError(code, scope, resource, label, unit string, used, requested, limit uint64, field, action string) error {
	available := uint64(0)
	if used < limit {
		available = limit - used
	}
	return platform.EDetails(409, code, label+"不足（"+resource+"）：已用 "+strconv.FormatUint(used, 10)+" + 本次需保留 "+strconv.FormatUint(requested, 10)+"，超过配置上限 "+strconv.FormatUint(limit, 10)+"（"+unit+"）。", platform.Row{"scope": scope, "resource": resource, "used": strconv.FormatUint(used, 10), "requested": strconv.FormatUint(requested, 10), "limit": strconv.FormatUint(limit, 10), "available": strconv.FormatUint(available, 10), "unit": unit, "field": field, "action": action})
}
