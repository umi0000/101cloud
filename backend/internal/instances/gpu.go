package instances

import (
	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/platform"
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func parseAddress(s string) ([]byte, error) {
	ip, e := netip.ParseAddr(s)
	if e != nil {
		return nil, bad("导入实例必须指定已登记在地址库存中的直连 IP。")
	}
	return ip.AsSlice(), nil
}
func registerGPU(a *platform.App, user, admin *gin.RouterGroup) {
	for _, g := range []*gin.RouterGroup{user, admin} {
		g.GET("/instances/:id/gpu-allocations", platform.Handle(func(c *gin.Context) (any, error) {
			r, e := owned(c, a)
			if e != nil {
				return nil, e
			}
			var records []platform.Row
			e = platform.DB(c, a).Table("gpu_allocations ga").Select("ga.id,g.uuid,g.model,ga.status,ga.gpu_hourly_pt,ga.gpu_price_version_id,ga.configured,ga.observed,ga.release_verified,ga.revision,ga.created_at,ga.released_at").Joins("JOIN node_gpus g ON g.id=ga.gpu_id").Where("ga.instance_id=?", r["id"]).Order("ga.id").Find(&records).Error
			if e != nil {
				return nil, e
			}
			safe := make([]platform.Row, 0, len(records))
			for _, row := range records {
				out := platform.Public(row, "id", "uuid", "model", "status", "gpu_hourly_pt", "revision", "created_at", "released_at")
				for _, key := range []string{"configured", "observed", "release_verified"} {
					out[key] = nil
					if row[key] != nil {
						out[key] = platform.Bool(row[key])
					}
				}
				if out["gpu_price_version_id"], e = platform.PolicyID(platform.DB(c, a), "gpu-pricing", row["gpu_price_version_id"]); e != nil {
					return nil, e
				}
				safe = append(safe, out)
			}
			return safe, nil
		}))
		for path, action := range map[string]string{"gpu-attachments": "attach", "gpu-detachments": "detach", "gpu-rebindings": "rebind"} {
			path, action := path, action
			g.POST("/instances/:id/"+path, platform.Handle(func(c *gin.Context) (any, error) {
				r, e := owned(c, a)
				if e != nil {
					return nil, e
				}
				if e = platform.Revision(c, r); e != nil {
					return nil, e
				}
				if e = permitted(c, r, "gpu_"+action); e != nil {
					return nil, e
				}
				var in GPURequest
				if e = platform.Decode(c, &in); e != nil {
					return nil, e
				}
				if !c.GetBool("admin") && len(in.FixedUUIDs) > 0 {
					return nil, bad("仅管理员可以指定固定 GPU UUID。")
				}
				ids := make([]uint64, 0, len(in.AllocationIDs))
				for _, s := range in.AllocationIDs {
					id, e := platform.IDString(s)
					if e != nil {
						return nil, e
					}
					ids = append(ids, id)
				}
				db := platform.DB(c, a)
				var out platform.Row
				switch action {
				case "attach":
					out, e = queueGPU(c.Request.Context(), a, db, r, "attach", ids, in)
				case "detach":
					out, e = QueueGPUDetach(c.Request.Context(), a, db, platform.Uint(r["id"]), platform.Uint(r["user_id"]), ids, in.AllowLive, "")
				case "rebind":
					out, e = queueGPU(c.Request.Context(), a, db, r, "rebind", ids, in)
				}
				if e != nil {
					return nil, e
				}
				taskAccepted(c, out)
				return nil, nil
			}))
		}
	}
}
func QueueGPUDetach(ctx context.Context, a *platform.App, db *gorm.DB, instanceID, userID uint64, ids []uint64, allowLive bool, reason string) (platform.Row, error) {
	if e := lockOwnerAccount(db, userID); e != nil {
		return nil, e
	}
	r, e := platform.Get(db, "instances", instanceID, true)
	if e != nil {
		return nil, e
	}
	if platform.Uint(r["user_id"]) != userID {
		return nil, platform.E(403, "forbidden", "实例所属用户不匹配，无权操作此实例。")
	}
	return queueGPU(ctx, a, db, r, "detach", ids, GPURequest{AllowLive: allowLive, Reason: reason})
}
func QueueGPURebind(ctx context.Context, a *platform.App, db *gorm.DB, instanceID, userID uint64, ids []uint64, maxHourlyPT string, allowLive bool, reason string) (platform.Row, error) {
	if e := lockOwnerAccount(db, userID); e != nil {
		return nil, e
	}
	r, e := platform.Get(db, "instances", instanceID, true)
	if e != nil {
		return nil, e
	}
	if platform.Uint(r["user_id"]) != userID {
		return nil, platform.E(403, "forbidden", "实例所属用户不匹配，无权操作此实例。")
	}
	return queueGPU(ctx, a, db, r, "rebind", ids, GPURequest{MaxHourlyPT: maxHourlyPT, AllowLive: allowLive, Reason: reason})
}
func queueGPU(ctx context.Context, a *platform.App, db *gorm.DB, r platform.Row, action string, ids []uint64, in GPURequest) (platform.Row, error) {
	r, e := platform.Get(db, "instances", platform.Uint(r["id"]), true)
	if e != nil {
		return nil, e
	}
	if platform.Text(r["image_type"]) != "container" {
		return nil, conflict("unsupported_configuration", "GPU 操作仅支持容器实例。")
	}
	if platform.Text(r["provisioning_state"]) != "ready" {
		return nil, conflict("instance_not_ready", "实例尚未就绪，请等待当前创建或恢复操作完成。")
	}
	state := platform.Text(r["observed_state"])
	if state != "running" && state != "stopped" {
		return nil, conflict("invalid_state", "需要确认实例处于运行或停止状态，请等待最新状态观测。")
	}
	if state == "running" && !in.AllowLive {
		return nil, conflict("unsupported_configuration", "请先停止容器，或明确设置 allow_live 允许在线操作。")
	}
	if a.Time().Sub(platform.Stamp(r["observed_at"])) > time.Minute {
		return nil, conflict("observation_incomplete", "实例状态观测已过期，请等待节点刷新状态。")
	}
	var old []platform.Row
	if action != "attach" {
		if len(ids) == 0 {
			return nil, bad("请通过 allocation_ids 指定要操作的 GPU 分配。")
		}
		seen := map[uint64]bool{}
		for _, id := range ids {
			if seen[id] {
				return nil, bad("allocation_ids 不能包含重复的 GPU 分配 ID。")
			}
			seen[id] = true
		}
		if e = locked(db).Table("gpu_allocations ga").Select("ga.*,g.uuid,g.model,g.memory_mib").Joins("JOIN node_gpus g ON g.id=ga.gpu_id").Where("ga.id IN ? AND ga.instance_id=? AND ga.active_slot=1", ids, r["id"]).Find(&old).Error; e != nil {
			return nil, e
		}
		if len(old) != len(ids) {
			return nil, bad("GPU 分配不属于此实例或已不再有效，请刷新分配列表。")
		}
		if state == "running" {
			if e = storedIdle(ctx, a, db, r, old); e != nil {
				return nil, e
			}
		}
	}
	if action == "attach" || action == "rebind" {
		image, e := platform.Get(db, "images", platform.Uint(r["image_id"]), false)
		if e != nil {
			return nil, e
		}
		if !platform.Bool(image["gpu_ready"]) {
			return nil, conflict("unsupported_configuration", "镜像未获准用于 GPU，请选择 GPU 就绪镜像。")
		}
	}
	if action == "rebind" {
		var required uint64
		for _, source := range old {
			if source["memory_mib"] == nil {
				return nil, conflict("observation_incomplete", "源 GPU 显存容量未知，请等待完整资源观测后重新绑定。")
			}
			if value := platform.Uint(source["memory_mib"]); value > required {
				required = value
			}
		}
		in.MinMemoryMiB = strconv.FormatUint(required, 10)
	}
	var target []uint64
	if action == "attach" || action == "rebind" {
		if action == "rebind" {
			in.Count = len(old)
			if in.Model == "" {
				in.Model = platform.Text(old[0]["model"])
			}
			if in.AccessMode == "" {
				in.AccessMode = platform.Text(old[0]["access_mode"])
			}
			for _, source := range old {
				if platform.Text(source["model"]) != in.Model || platform.Text(source["access_mode"]) != in.AccessMode {
					return nil, bad("型号或访问模式不同的 GPU 分配需要分别重新绑定。")
				}
			}
		}
		if in.Count <= 0 || in.Count > 64 {
			return nil, bad("count 必须在 1–64 范围内。")
		}
		if in.MaxHourlyPT == "" {
			in.MaxHourlyPT = platform.Text(r["max_gpu_hourly_pt"])
		}
		if in.AccessMode == "" {
			in.AccessMode = "shared"
		}
		target, e = reserveGPUs(ctx, a, db, r, in, old)
		if e != nil {
			return nil, e
		}
		if e = billing.AdmitInstance(ctx, db, r, "gpu_attach"); e != nil {
			return nil, e
		}
		if e = billing.InstanceUsage(ctx, db, r, "gpu_reserved", target); e != nil {
			return nil, e
		}
	}
	if len(ids) > 0 {
		var reason any
		if in.Reason != "" {
			reason = in.Reason
		}
		if e = db.Table("gpu_allocations").Where("id IN ?", ids).Updates(platform.Row{"status": "releasing", "release_requested_at": a.Time(), "release_reason": reason, "revision": gorm.Expr("revision+1")}).Error; e != nil {
			return nil, e
		}
	}
	if e = db.Table("instances").Where("id=?", r["id"]).UpdateColumn("revision", gorm.Expr("revision+1")).Error; e != nil {
		return nil, e
	}
	params := platform.Row{"action": action, "allocation_ids": ids, "target_ids": target, "allow_live": in.AllowLive}
	if in.Reason != "" {
		params["reason"] = in.Reason
	}
	return platform.Enqueue(db, "instance.gpu", platform.Uint(r["user_id"]), platform.Uint(r["node_id"]), platform.Uint(r["id"]), true, params)
}
func reserveGPUs(ctx context.Context, a *platform.App, db *gorm.DB, r platform.Row, in GPURequest, old []platform.Row) ([]uint64, error) {
	max, e := decimal.NewFromString(in.MaxHourlyPT)
	if e != nil || max.IsNegative() || max.Exponent() < -6 {
		return nil, bad("max_hourly_pt 必须为非负十进制数，最多六位小数。")
	}
	if in.AccessMode != "shared" && in.AccessMode != "exclusive" {
		return nil, bad("access_mode 必须为 shared 或 exclusive。")
	}
	if len(in.FixedUUIDs) > 0 && len(in.FixedUUIDs) != in.Count {
		return nil, bad("fixed_uuids 数量必须与申请的 GPU 数量一致。")
	}
	policy, e := a.InstancePolicy(ctx, r, "gpu-scheduling")
	if e != nil {
		return nil, e
	}
	maxAge := time.Duration(platform.Uint(policy["metrics_max_age_seconds"])) * time.Second
	if maxAge <= 0 {
		return nil, platform.E(503, "policy_not_configured", "GPU 观测有效期未配置，请管理员发布有效的调度策略。")
	}
	smWeight, memWeight := numeric(policy["sm_weight"]), numeric(policy["memory_weight"])
	if smWeight < 0 || memWeight < 0 || smWeight+memWeight < 0.999999 || smWeight+memWeight > 1.000001 {
		return nil, platform.E(503, "policy_not_configured", "GPU 调度权重无效，请管理员检查调度策略。")
	}
	price, e := a.InstancePolicy(ctx, r, "gpu-pricing")
	if e != nil {
		return nil, e
	}
	rate, e := boundGPUPrice(in, price, max)
	if e != nil {
		return nil, e
	}
	_, priceSource, e := platform.PolicySource(db, "gpu-pricing", platform.Text(object(r["policy_versions"])["gpu-pricing"]), false)
	if e != nil {
		return nil, e
	}
	n, e := platform.Get(db, "nodes", platform.Uint(r["node_id"]), true)
	if e != nil {
		return nil, e
	}
	if platform.Text(n["administrative_state"]) != "active" || platform.Text(n["health"]) != "online" || a.Time().Sub(platform.Stamp(n["resources_sampled_at"])) > maxAge {
		return nil, conflict("gpu_capacity_unavailable", "节点 GPU 库存不可用或观测已过期，请管理员检查节点状态与资源采集。")
	}
	var limits platform.Row
	if e = locked(db).Table("user_limits").Where("user_id=?", r["user_id"]).Take(&limits).Error; e != nil {
		return nil, e
	}
	var used int64
	if e = db.Table("gpu_allocations").Where("user_id=? AND active_slot=1", r["user_id"]).Count(&used).Error; e != nil {
		return nil, e
	}
	if uint64(in.Count) > platform.Uint(limits["max_gpu_allocations"]) || uint64(used) > platform.Uint(limits["max_gpu_allocations"])-uint64(in.Count) {
		return nil, admissionCapacityError("quota_exceeded", "user_quota", "gpus", "GPU 分配数量", "count", uint64(used), uint64(in.Count), platform.Uint(limits["max_gpu_allocations"]), "请释放不再需要的 GPU 分配，或联系管理员提高 GPU 配额；重新绑定期间源分配仍计入占用。")
	}
	inventory := map[string]map[string]any{}
	for _, v := range rows(object(n["resources"])["gpus"]) {
		g := object(v)
		inventory[platform.Text(g["uuid"])] = g
	}
	var all []platform.Row
	if e = locked(db).Table("node_gpus g").Select("g.*").Where("g.node_id=?", r["node_id"]).Order("g.id").Find(&all).Error; e != nil {
		return nil, e
	}
	excluded := map[uint64]bool{}
	var own []platform.Row
	if e = db.Table("gpu_allocations").Select("gpu_id").Where("instance_id=? AND active_slot=1", r["id"]).Find(&own).Error; e != nil {
		return nil, e
	}
	for _, v := range own {
		excluded[platform.Uint(v["gpu_id"])] = true
	}
	fixed := map[string]bool{}
	for _, uuid := range in.FixedUUIDs {
		if fixed[uuid] {
			return nil, bad("fixed_uuids 中存在重复的 GPU UUID，请移除重复项。")
		}
		fixed[uuid] = true
	}
	type candidate struct {
		row   platform.Row
		score float64
		used  int64
	}
	candidates := []candidate{}
	for _, g := range all {
		uuid := platform.Text(g["uuid"])
		if len(fixed) > 0 && !fixed[uuid] {
			continue
		}
		if g["memory_mib"] == nil || excluded[platform.Uint(g["id"])] || platform.Text(g["status"]) != "available" || platform.Text(g["model"]) != platform.Text(price["gpu_model"]) || platform.Uint(g["memory_mib"]) < mustUint(in.MinMemoryMiB) || a.Time().Sub(platform.Stamp(g["last_seen_at"])) > maxAge {
			continue
		}
		var count int64
		var exclusive int64
		if e = db.Table("gpu_allocations").Where("gpu_id=? AND active_slot=1", g["id"]).Count(&count).Error; e != nil {
			return nil, e
		}
		if e = db.Table("gpu_allocations").Where("gpu_id=? AND active_slot=1 AND access_mode='exclusive'", g["id"]).Count(&exclusive).Error; e != nil {
			return nil, e
		}
		if exclusive > 0 || in.AccessMode == "exclusive" && count > 0 || uint64(count) >= platform.Uint(g["max_allocations"]) {
			continue
		}
		live := inventory[uuid]
		score := 0.0
		if len(fixed) == 0 {
			if live == nil || live["utilization_percent"] == nil || live["memory_utilization_percent"] == nil || platform.Text(live["error"]) != "" {
				continue
			}
			sm, mem := numeric(live["utilization_percent"]), numeric(live["memory_utilization_percent"])
			if sm < 0 || sm > 100 || mem < 0 || mem > 100 {
				continue
			}
			score = smWeight*sm + memWeight*mem
		}
		candidates = append(candidates, candidate{g, score, count})
	}
	if in.Reason == "automatic_gpu_rebalance" {
		if len(old) == 0 {
			return nil, bad("自动重新绑定需要有效的源 GPU 分配。")
		}
		sourceScore := 101.0
		sourceRate := max
		for _, source := range old {
			live := inventory[platform.Text(source["uuid"])]
			if live == nil || live["utilization_percent"] == nil || live["memory_utilization_percent"] == nil {
				return nil, conflict("observation_incomplete", "源 GPU 负载未知，请等待最新的完整观测。")
			}
			score := smWeight*numeric(live["utilization_percent"]) + memWeight*numeric(live["memory_utilization_percent"])
			if score < sourceScore {
				sourceScore = score
			}
			rate, err := decimal.NewFromString(platform.Text(source["gpu_hourly_pt"]))
			if err != nil {
				return nil, err
			}
			if rate.LessThan(sourceRate) {
				sourceRate = rate
			}
		}
		filtered := candidates[:0]
		for _, c := range candidates {
			if c.score > numeric(policy["target_low_load_pct"]) || sourceScore-c.score < numeric(policy["min_improvement_pct"]) || !platform.Bool(policy["allow_price_increase"]) && rate.GreaterThan(sourceRate) {
				continue
			}
			filtered = append(filtered, c)
		}
		candidates = filtered
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score < candidates[j].score
		}
		if candidates[i].used != candidates[j].used {
			return candidates[i].used < candidates[j].used
		}
		return platform.Text(candidates[i].row["uuid"]) < platform.Text(candidates[j].row["uuid"])
	})
	if len(candidates) < in.Count {
		return nil, platform.EDetails(409, "gpu_capacity_unavailable", "符合型号、显存、访问模式和最新观测要求的 GPU 不足：本次申请 "+strconv.Itoa(in.Count)+"，当前可选 "+strconv.Itoa(len(candidates))+"。", platform.Row{"scope": "gpu", "resource": "gpus", "requested": strconv.Itoa(in.Count), "available": strconv.Itoa(len(candidates)), "unit": "count", "action": "请减少 GPU 数量，或联系管理员检查 GPU 库存、分配上限和观测状态。"})
	}
	ids := make([]uint64, 0, in.Count)
	for _, c := range candidates[:in.Count] {
		allocation := platform.Row{"instance_id": r["id"], "user_id": r["user_id"], "node_id": r["node_id"], "gpu_id": c.row["id"], "gpu_price_version_id": priceSource, "gpu_hourly_pt": rate.StringFixed(6), "status": "reserved", "access_mode": in.AccessMode, "reserved_at": a.Time(), "configured": false, "observed": false, "release_verified": false, "revision": 1}
		if e = db.Table("gpu_allocations").Create(&allocation).Error; e != nil {
			return nil, e
		}
		id, e := platform.InsertedID(allocation)
		if e != nil {
			return nil, e
		}
		ids = append(ids, id)
	}
	return ids, nil
}
func boundGPUPrice(in GPURequest, price platform.Row, max decimal.Decimal) (decimal.Decimal, error) {
	model, access := platform.Text(price["gpu_model"]), platform.Text(price["access_mode"])
	if model == "" || access != "shared" && access != "exclusive" {
		return decimal.Zero, platform.E(500, "invalid_stored_policy", "实例绑定的 GPU 定价型号或访问模式无效。")
	}
	if in.Model != "" && in.Model != model || in.AccessMode != access {
		return decimal.Zero, bad("GPU 定价的型号或访问模式与申请不匹配，请检查实例绑定的 GPU 定价策略。")
	}
	rate, e := policyMoney(platform.Text(price["gpu_hourly_pt"]))
	if e != nil {
		return decimal.Zero, platform.E(500, "invalid_stored_policy", "实例绑定的 GPU 价格无效。")
	}
	if rate.GreaterThan(max) {
		return decimal.Zero, platform.EDetails(409, "gpu_price_exceeds_limit", "实例绑定的 GPU 单价超过本次价格上限，请提高价格上限。", platform.Row{"scope": "gpu", "resource": "gpu_hourly_pt", "gpu_hourly_pt": rate.StringFixed(6), "max_hourly_pt": max.StringFixed(6), "reason": "gpu_price_exceeds_limit"})
	}
	return rate, nil
}
func numeric(v any) float64 { f, _ := strconv.ParseFloat(platform.Text(v), 64); return f }
func storedIdle(ctx context.Context, a *platform.App, db *gorm.DB, r platform.Row, allocations []platform.Row) error {
	var sample platform.Row
	if e := db.Table("instance_metric_samples").Where("instance_id=?", r["id"]).Order("sampled_at DESC,id DESC").Take(&sample).Error; e != nil {
		return conflict("observation_incomplete", "缺少完整的 GPU 占用证据，请等待完整采集后重试。")
	}
	policy, e := a.InstancePolicy(ctx, r, "gpu-scheduling")
	if e != nil {
		return e
	}
	if a.Time().Sub(platform.Stamp(sample["sampled_at"])) > time.Duration(platform.Uint(policy["metrics_max_age_seconds"]))*time.Second || !platform.Bool(sample["collection_complete"]) || !platform.Bool(sample["attribution_complete"]) || platform.Text(sample["instance_uuid"]) != platform.Text(r["incus_uuid"]) {
		return conflict("observation_incomplete", "GPU 占用证据不完整或已过期，请等待最新完整采集。")
	}
	for _, allocation := range allocations {
		var g platform.Row
		if e = db.Table("instance_gpu_metric_samples").Where("sample_id=? AND gpu_uuid=?", sample["id"], allocation["uuid"]).Take(&g).Error; e != nil {
			return conflict("observation_incomplete", "缺少此 GPU 的观测记录，请等待资源采集。")
		}
		if !platform.Bool(g["attribution_complete"]) || platform.Text(g["reference_scan_quality"]) != "complete" || g["process_count"] == nil || g["reference_count"] == nil || g["memory_used_bytes"] == nil {
			return conflict("observation_incomplete", "GPU 设备引用尚未完整确认，不能安全释放，请等待完整引用扫描。")
		}
		if platform.Uint(g["process_count"]) > 0 || platform.Uint(g["reference_count"]) > 0 || platform.Uint(g["memory_used_bytes"]) > 0 || platform.Text(g["gpu_state"]) != "idle_candidate" {
			return conflict("gpu_in_use", "GPU 仍有进程、设备引用或显存占用，请结束使用并等待确认空闲后重试。")
		}
	}
	return nil
}
func liveGPU(ctx context.Context, a *platform.App, r platform.Row) (map[string]map[string]any, error) {
	frame, e := a.Agent(ctx, platform.Uint(r["node_id"]), "GET", "/agent/v1/instances/metrics?project="+url.QueryEscape(platform.Text(r["project_name"])), nil)
	if e != nil {
		return nil, e
	}
	if !platform.Bool(frame["collection_complete"]) {
		return nil, conflict("observation_incomplete", "实时 GPU 采集不完整，请等待节点恢复完整采集。")
	}
	stamp := platform.Stamp(frame["collected_at"])
	policy, e := a.InstancePolicy(ctx, r, "gpu-scheduling")
	if e != nil {
		return nil, e
	}
	if stamp.IsZero() || a.Time().Sub(stamp) > time.Duration(platform.Uint(policy["metrics_max_age_seconds"]))*time.Second || stamp.After(a.Time().Add(time.Second)) {
		return nil, conflict("observation_incomplete", "实时 GPU 样本已过期，请等待最新采集。")
	}
	for _, item := range rows(frame["instances"]) {
		instance := object(item)
		if platform.Text(instance["uuid"]) != platform.Text(r["incus_uuid"]) || platform.Text(instance["name"]) != platform.Text(r["incus_name"]) {
			continue
		}
		q := object(instance["quality"])
		if !platform.Bool(q["attribution_complete"]) {
			return nil, conflict("observation_incomplete", "实时 GPU 归属信息不完整，暂不能安全调整分配。")
		}
		out := map[string]map[string]any{"_frame": {"collected_at": stamp, "state": instance["state"], "started_at": instance["started_at"]}}
		for _, v := range rows(instance["gpus"]) {
			g := object(v)
			out[platform.Text(g["uuid"])] = g
		}
		return out, nil
	}
	return nil, conflict("observation_incomplete", "实时采集中未找到此实例，暂不能安全调整 GPU。")
}
func safeLiveCard(g map[string]any, removed bool) error {
	if g == nil {
		return conflict("observation_incomplete", "实时采集中未找到此 GPU，暂不能安全调整分配。")
	}
	q := object(g["quality"])
	if !platform.Bool(q["attribution_complete"]) || g["process_count"] == nil || g["referencing_process_count"] == nil || g["used_memory_bytes"] == nil {
		return conflict("observation_incomplete", "GPU 引用扫描不完整，请等待完整扫描。")
	}
	if platform.Uint(g["process_count"]) > 0 || platform.Uint(g["referencing_process_count"]) > 0 || platform.Uint(g["used_memory_bytes"]) > 0 {
		return conflict("gpu_in_use", "GPU 仍有进程、设备引用或显存占用，请结束使用并等待确认空闲后重试。")
	}
	if removed && (platform.Bool(g["configured"]) || platform.Bool(g["observed"])) {
		return conflict("observation_incomplete", "源 GPU 仍被配置或观测为已连接，不能确认释放。")
	}
	if !removed && platform.Text(g["state"]) != "idle_candidate" {
		return conflict("observation_incomplete", "尚未确认 GPU 空闲，请停止使用并等待最新观测。")
	}
	return nil
}
func runGPU(ctx context.Context, a *platform.App, job platform.Job) error {
	var p struct {
		Action        string   `json:"action"`
		AllocationIDs []uint64 `json:"allocation_ids"`
		TargetIDs     []uint64 `json:"target_ids"`
		AllowLive     bool     `json:"allow_live"`
	}
	if e := json.Unmarshal(job.Parameters, &p); e != nil {
		return e
	}
	r, e := platform.Get(a.DB, "instances", job.InstanceID, false)
	if e != nil {
		return platform.Unknown(e)
	}
	var allocations []platform.Row
	if e = a.DB.Table("gpu_allocations ga").Select("ga.*,g.uuid").Joins("JOIN node_gpus g ON g.id=ga.gpu_id").Where("ga.instance_id=? AND (ga.active_slot=1 OR ga.id IN ?)", job.InstanceID, p.AllocationIDs).Order("ga.id").Find(&allocations).Error; e != nil {
		return platform.Unknown(e)
	}
	old := map[uint64]bool{}
	for _, id := range p.AllocationIDs {
		old[id] = true
	}
	targets := map[uint64]bool{}
	for _, id := range p.TargetIDs {
		targets[id] = true
	}
	uuids := []string{}
	removed := []platform.Row{}
	for _, g := range allocations {
		id := platform.Uint(g["id"])
		if old[id] {
			if platform.Text(g["status"]) == "released" && !platform.Bool(g["release_verified"]) {
				return platform.Unknown(conflict("observation_incomplete", "源 GPU 的释放尚未验证，请等待独立观测确认。"))
			}
			removed = append(removed, g)
			continue
		}
		if targets[id] {
			delete(targets, id)
		}
		uuids = append(uuids, platform.Text(g["uuid"]))
	}
	if len(removed) != len(old) || len(targets) > 0 {
		return platform.Unknown(conflict("observation_incomplete", "GPU 分配归属已变更，请刷新分配状态并等待对账。"))
	}
	state, e := a.Agent(ctx, job.NodeID, "GET", "/1.0/instances/"+url.PathEscape(platform.Text(r["incus_name"]))+"/state?project="+url.QueryEscape(platform.Text(r["project_name"])), nil)
	if e != nil {
		return platform.Unknown(e)
	}
	st := strings.ToLower(platform.Text(object(state["metadata"])["status"]))
	if st != "stopped" && st != "running" {
		return platform.Unknown(conflict("invalid_state", "实例尚未稳定处于运行或停止状态，不能安全变更 GPU。"))
	}
	if e = configureGPU(ctx, a, job, r, uuids, removed, p.AllowLive, st); e != nil {
		return platform.Unknown(e)
	}
	if len(p.TargetIDs) > 0 {
		if e = a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
			if e := lockWorkerAccount(db, a, job, platform.Uint(r["user_id"])); e != nil {
				return e
			}
			var observed any
			if st == "stopped" {
				observed = false
			}
			if e := db.Table("gpu_allocations").Where("id IN ? AND instance_id=?", p.TargetIDs, job.InstanceID).Updates(platform.Row{"status": "active", "configured": true, "observed": observed, "attached_at": gorm.Expr("COALESCE(attached_at,?)", a.Time()), "last_verified_at": a.Time(), "revision": gorm.Expr("revision+1")}).Error; e != nil {
				return e
			}
			return billing.InstanceUsage(ctx, db, r, "gpu_attached", p.TargetIDs)
		}); e != nil {
			return platform.Unknown(e)
		}
	}
	if len(removed) > 0 && st == "running" {
		after := a.Time()
		policy, e := a.InstancePolicy(ctx, r, "gpu-scheduling")
		if e != nil {
			return platform.Unknown(e)
		}
		deadline := after.Add(time.Duration(2*platform.Uint(policy["poll_interval_seconds"])+30) * time.Second)
		for {
			live, e := liveGPU(ctx, a, r)
			if e != nil {
				return platform.Unknown(e)
			}
			if platform.Stamp(live["_frame"]["collected_at"]).After(after) {
				for _, g := range removed {
					card := live[platform.Text(g["uuid"])]
					if card == nil {
						continue
					}
					if e = safeLiveCard(card, true); e != nil {
						return platform.Unknown(e)
					}
				}
				break
			}
			if !a.Time().Before(deadline) {
				return platform.Unknown(conflict("observation_incomplete", "正在等待变更后的独立 GPU 释放样本，暂不能确认资源已释放。"))
			}
			select {
			case <-ctx.Done():
				return platform.Unknown(ctx.Err())
			case <-time.After(time.Second):
			}
		}
	}
	e = a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if e := lockWorkerAccount(db, a, job, platform.Uint(r["user_id"])); e != nil {
			return e
		}
		if len(p.AllocationIDs) > 0 {
			if e = db.Table("gpu_allocations").Where("id IN ? AND instance_id=?", p.AllocationIDs, job.InstanceID).Updates(platform.Row{"status": "released", "configured": false, "observed": false, "release_verified": true, "released_at": gorm.Expr("COALESCE(released_at,?)", a.Time()), "last_verified_at": a.Time(), "revision": gorm.Expr("revision+1")}).Error; e != nil {
				return e
			}
			if e = billing.InstanceUsage(ctx, db, r, "gpu_released", p.AllocationIDs); e != nil {
				return e
			}
		}
		if p.Action == "rebind" {
			if e = db.Table("instances").Where("id=?", job.InstanceID).Update("last_rebound_at", a.Time()).Error; e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		return platform.Unknown(e)
	}
	return a.TaskResult(ctx, job, platform.Row{"configuration_verified": true, "release_verified": len(p.AllocationIDs) > 0})
}
func configureGPU(ctx context.Context, a *platform.App, job platform.Job, r platform.Row, want []string, removed []platform.Row, allowLive bool, state string) error {
	path := "/1.0/instances/" + url.PathEscape(platform.Text(r["incus_name"])) + "?project=" + url.QueryEscape(platform.Text(r["project_name"]))
	observe := func() (bool, error) {
		response, e := a.Agent(ctx, job.NodeID, "GET", path, nil)
		if e != nil {
			return false, e
		}
		native := object(response["metadata"])
		if platform.Text(object(native["config"])["volatile.uuid"]) != platform.Text(r["incus_uuid"]) {
			return false, conflict("instance_identity_changed", "原生实例 UUID 已变化，为避免操作错误实例已停止变更，请联系管理员核对身份。")
		}
		inventory, e := a.Agent(ctx, job.NodeID, "GET", "/agent/v1/resources", nil)
		if e != nil {
			return false, e
		}
		actual, e := configuredGPUUUIDs(native, inventory)
		if e != nil {
			return false, e
		}
		if len(actual) != len(want) {
			return false, nil
		}
		set := map[string]bool{}
		for _, uuid := range actual {
			set[uuid] = true
		}
		for _, uuid := range want {
			if !set[uuid] {
				return false, nil
			}
		}
		return true, nil
	}
	matches, e := observe()
	if e != nil {
		return platform.Unknown(e)
	}
	if matches {
		return nil
	}
	if state == "running" && !allowLive {
		return conflict("unsupported_configuration", "请停止容器，或明确设置 allow_live 允许在线调整。")
	}
	prior, e := workerStep(ctx, a, job, "gpu.configure")
	if e != nil {
		return e
	}
	if prior != nil {
		operation := platform.Text(prior["operation_id"])
		if operation != "" {
			if !strings.HasPrefix(operation, "/1.0/operations/") {
				operation = "/1.0/operations/" + url.PathEscape(operation)
			}
			if _, e = a.WaitOperation(ctx, job.NodeID, operation); e != nil {
				return platform.Unknown(e)
			}
			matches, e = observe()
			if e != nil {
				return platform.Unknown(e)
			}
			if matches {
				return nil
			}
		}
		return platform.Unknown(conflict("observation_incomplete", "上一次 GPU 变更结果尚未确认，需先完成对账；系统不会盲目重放。"))
	}
	if state == "running" {
		live, e := liveGPU(ctx, a, r)
		if e != nil {
			return e
		}
		for _, g := range removed {
			if e = safeLiveCard(live[platform.Text(g["uuid"])], false); e != nil {
				return e
			}
		}
	}
	if e = a.Step(ctx, job, "gpu.configure", "", "running"); e != nil {
		return e
	}
	result, e := a.Agent(ctx, job.NodeID, "PUT", "/agent/v1/instances/"+url.PathEscape(platform.Text(r["incus_name"]))+"/gpu?project="+url.QueryEscape(platform.Text(r["project_name"])), platform.Row{"gpu_uuids": want, "allow_live": allowLive})
	if e != nil {
		return platform.Unknown(e)
	}
	operation := platform.Text(result["operation_id"])
	if e = a.Step(ctx, job, "gpu.configure", operation, "waiting"); e != nil {
		return platform.Unknown(e)
	}
	if !platform.Bool(result["operation_completed"]) || platform.Text(result["configuration_state"]) != "observed" {
		return platform.Unknown(conflict("observation_incomplete", "GPU 操作后的配置状态仍不确定，请等待对账确认。"))
	}
	matches, e = observe()
	if e != nil {
		return platform.Unknown(e)
	}
	if !matches {
		return platform.Unknown(conflict("observation_incomplete", "观测到的 GPU 配置与请求 UUID 不一致，请等待对账或联系管理员。"))
	}
	return a.Step(ctx, job, "gpu.configure", operation, "succeeded")
}
func configuredGPUUUIDs(native, inventory platform.Row) ([]string, error) {
	devices, ok := native["expanded_devices"].(map[string]any)
	if !ok {
		return nil, conflict("observation_incomplete", "无法读取原生实例的展开设备配置，请检查节点观测状态。")
	}
	byPCI := map[string]string{}
	byUUID := map[string]bool{}
	for _, v := range rows(inventory["gpus"]) {
		g := object(v)
		pci := strings.ToLower(platform.Text(g["pci_address"]))
		uuid := platform.Text(g["uuid"])
		if len(pci) == 7 {
			pci = "0000:" + pci
		}
		if pci == "" || uuid == "" {
			continue
		}
		if byPCI[pci] != "" || byUUID[uuid] {
			return nil, conflict("observation_incomplete", "GPU 库存标识存在歧义，请管理员核对 UUID 与 PCI 标识。")
		}
		byPCI[pci] = uuid
		byUUID[uuid] = true
	}
	out := []string{}
	seen := map[string]bool{}
	for _, v := range devices {
		device, ok := v.(map[string]any)
		if !ok || platform.Text(device["type"]) == "" {
			return nil, conflict("observation_incomplete", "原生设备类型信息缺失，无法安全识别 GPU。")
		}
		if platform.Text(device["type"]) != "gpu" {
			continue
		}
		if kind := platform.Text(device["gputype"]); kind != "" && kind != "physical" {
			return nil, conflict("unsupported_configuration", "仅支持通过 UUID 标识的物理 GPU。")
		}
		pci := strings.ToLower(platform.Text(device["pci"]))
		if len(pci) == 7 {
			pci = "0000:" + pci
		}
		uuid := byPCI[pci]
		if uuid == "" || seen[uuid] {
			return nil, conflict("observation_incomplete", "无法唯一确定 GPU UUID，请管理员检查设备与库存标识。")
		}
		seen[uuid] = true
		out = append(out, uuid)
	}
	return out, nil
}
