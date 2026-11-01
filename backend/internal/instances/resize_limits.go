package instances

import (
	"strconv"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// reservationUsage is shared with admission so previews count exactly the same
// reservations, including released creation failures and stopped elastic guests.
func reservationUsage(db *gorm.DB, r platform.Row, node bool) (platform.Row, error) {
	column, id := "user_id", r["user_id"]
	selection := "COUNT(*) AS instances,COALESCE(SUM(vcpus),0) AS vcpus,COALESCE(SUM(memory_bytes),0) AS memory_bytes,COALESCE(SUM(disk_bytes),0) AS disk_bytes,COALESCE(SUM(product_type='custom'),0) AS resident"
	if node {
		column, id = "node_id", r["node_id"]
		selection = "COALESCE(SUM(CASE WHEN product_type='custom' OR desired_state<>'stopped' OR observed_state<>'stopped' OR provisioning_state<>'ready' THEN vcpus ELSE 0 END),0) AS vcpus,COALESCE(SUM(CASE WHEN product_type='custom' OR desired_state<>'stopped' OR observed_state<>'stopped' OR provisioning_state<>'ready' THEN memory_bytes ELSE 0 END),0) AS memory_bytes,COALESCE(SUM(disk_bytes),0) AS disk_bytes,COALESCE(SUM(CASE WHEN product_type='custom' THEN vcpus ELSE 0 END),0) AS resident_vcpus,COALESCE(SUM(CASE WHEN product_type='custom' THEN memory_bytes ELSE 0 END),0) AS resident_memory,COALESCE(SUM(CASE WHEN product_type='custom' THEN disk_bytes ELSE 0 END),0) AS resident_disk"
	}
	var usage platform.Row
	err := db.Table("instances").Select(selection).Where(column+"=? AND deleted_at IS NULL AND NOT (provisioning_state='failed' AND incus_uuid IS NULL AND COALESCE(JSON_EXTRACT(incus_config_snapshot,'$.creation_reservations_released') = true, 0)) AND id<>?", id, platform.Uint(r["id"])).Take(&usage).Error
	return usage, err
}

func admissionNodeAvailable(a *platform.App, n platform.Row) error {
	if platform.Text(n["administrative_state"]) != "active" || platform.Text(n["health"]) != "online" || platform.Stamp(n["connection_verified_at"]).IsZero() || a.Time().Sub(platform.Stamp(n["resources_sampled_at"])) > time.Minute {
		return conflict("node_unavailable", "资源准入需要已验证、在线且资源观测未过期的节点，请管理员检查节点状态。")
	}
	return nil
}

func resizeLimits(c *gin.Context, a *platform.App) (any, error) {
	r, err := owned(c, a)
	if err != nil {
		return nil, err
	}
	if err = permitted(c, r, "resize"); err != nil {
		return nil, err
	}
	max, err := resizeMaxResources(a, platform.DB(c, a), r)
	if err != nil {
		return nil, err
	}
	return platform.Row{"max_resources": max}, nil
}

// resizeMaxResources only reads a capacity snapshot; the final resize continues
// to perform authoritative admission under the existing write-side locks.
func resizeMaxResources(a *platform.App, db *gorm.DB, r platform.Row) (Resources, error) {
	var result Resources
	owner, err := platform.Get(db, "users", platform.Uint(r["user_id"]), false)
	if err != nil {
		return result, err
	}
	if platform.Text(owner["status"]) != "active" {
		return result, platform.E(403, "owner_inactive", "实例所属用户未启用，请管理员检查用户状态。")
	}
	limits, err := queryRow(db, "user_limits", "user_id=?", r["user_id"])
	if err != nil {
		if api, ok := err.(*platform.HTTPError); ok && api.Status == 404 {
			return result, conflict("quota_not_configured", "用户尚未配置资源配额，请联系管理员配置。")
		}
		return result, err
	}
	user, err := reservationUsage(db, r, false)
	if err != nil {
		return result, err
	}
	if platform.Uint(user["instances"]) >= platform.Uint(limits["max_instances"]) {
		return result, conflict("quota_exceeded", "实例数量已超过用户配额，请联系管理员调整配额。")
	}
	if platform.Text(r["product_type"]) == "custom" && platform.Uint(user["resident"]) >= platform.Uint(limits["max_resident_instances"]) {
		return result, conflict("quota_exceeded", "常驻实例数量已超过用户配额，请联系管理员调整配额。")
	}
	n, err := platform.Get(db, "nodes", platform.Uint(r["node_id"]), false)
	if err != nil {
		return result, err
	}
	if err = admissionNodeAvailable(a, n); err != nil {
		return result, err
	}
	node, err := reservationUsage(db, r, true)
	if err != nil {
		return result, err
	}
	remaining := func(limit, used any) uint64 {
		l, u := platform.Uint(limit), platform.Uint(used)
		if u >= l {
			return 0
		}
		return l - u
	}
	max := func(resource, residentUsage, residentBudget string) uint64 {
		value := min(remaining(limits["max_"+resource], user[resource]), remaining(n["allocatable_"+resource], node[resource]))
		if platform.Text(r["product_type"]) == "custom" {
			value = min(value, remaining(n[residentBudget], node[residentUsage]))
		}
		return value
	}
	result.VCPUs = min(uint64(65535), max("vcpus", "resident_vcpus", "resident_vcpu_budget"))
	result.MemoryBytes = strconv.FormatUint(max("memory_bytes", "resident_memory", "resident_memory_budget_bytes"), 10)
	result.DiskBytes = strconv.FormatUint(max("disk_bytes", "resident_disk", "resident_disk_budget_bytes"), 10)
	return result, nil
}
