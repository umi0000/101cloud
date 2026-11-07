package instances

import (
	"context"
	"time"

	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

// Called only while every component task row is locked and its execution lease
// has been revoked. The GPU implementation has one native mutation entry point,
// gpu.configure, and commits its dispatch intent before sending any bytes.
func undispatchedGPUComponent(db *gorm.DB, tasks []platform.Row) (platform.Row, bool, error) {
	var root platform.Row
	for _, task := range tasks {
		switch platform.Text(task["task_type"]) {
		case "instance.gpu":
			if root != nil {
				return nil, false, nil
			}
			root = task
		case "instance.reconcile":
		default:
			return nil, false, nil
		}
	}
	if root == nil || platform.Text(root["status"]) == "succeeded" {
		return nil, false, nil
	}
	for _, task := range tasks {
		var steps []platform.Row
		if err := db.Table("task_steps").Where("task_id=?", task["id"]).Find(&steps).Error; err != nil {
			return nil, false, err
		}
		preflight := false
		for _, step := range steps {
			journal := object(step["result_redacted"])
			if platform.Text(step["operation_id"]) != "" || platform.Bool(journal["native_dispatch_started"]) || !platform.Bool(journal["native_dispatch_fenced"]) {
				return nil, false, nil
			}
			switch platform.Text(step["step_key"]) {
			case "gpu.preflight":
				preflight = true
			case "gpu.configure": // Fenced intent that never crossed dispatch.
				preflight = true
			default:
				return nil, false, nil
			}
		}
		if preflight {
			continue
		}
		if len(steps) == 0 && platform.Text(task["task_type"]) == "instance.reconcile" && platform.Bool(object(task["parameters_redacted"])["read_only_force_stop_reconcile"]) {
			continue // This implementation never dispatches a native mutation.
		}
		status := platform.Text(task["status"])
		if len(steps) == 0 && platform.Uint(task["attempt_count"]) == 0 && (status == "queued" || (status == "cancelled" && platform.Text(object(object(task["result_redacted"])["force_stop"])["previous_status"]) == "queued")) {
			continue
		}
		// A missing journal alone is not evidence. These exact stored business
		// failures identify read-only GPU ownership preflight in the historical
		// implementation; any prior native mutation would have gpu.configure.
		err := object(object(task["result_redacted"])["error"])
		if len(steps) == 0 && platform.Text(err["code"]) == "observation_incomplete" {
			switch platform.Text(err["message"]) {
			case "实时 GPU 采集不完整，请等待节点恢复完整采集。", "实时 GPU 样本已过期，请等待最新采集。", "实时 GPU 归属信息不完整，暂不能安全调整分配。", "实时采集中未找到此实例，暂不能安全调整 GPU。":
				continue
			}
		}
		return nil, false, nil
	}
	return root, true, nil
}

// This is logical intent rollback, not physical GPU release. In particular a
// target already active/configured/observed can never be revoked by this path.
func rollbackUndispatchedGPU(ctx context.Context, a *platform.App, db *gorm.DB, root platform.Row, now time.Time) error {
	p := object(root["parameters_redacted"])
	ids := func(key string) []uint64 {
		var result []uint64
		for _, value := range rows(p[key]) {
			if id := platform.Uint(value); id != 0 {
				result = append(result, id)
			}
		}
		return result
	}
	if source := ids("allocation_ids"); len(source) > 0 {
		// Keep prior release metadata as history; do not erase another intent's
		// timestamps/reason or claim that the original physical card was freed.
		if err := db.Table("gpu_allocations").Where("id IN ? AND instance_id=? AND user_id=? AND node_id=? AND status='releasing'", source, root["instance_id"], root["user_id"], root["node_id"]).Updates(platform.Row{"status": "active", "revision": gorm.Expr("revision+1")}).Error; err != nil {
			return err
		}
	}
	var targets []platform.Row
	if target := ids("target_ids"); len(target) > 0 {
		if err := locked(db).Table("gpu_allocations").Where("id IN ? AND instance_id=? AND user_id=? AND node_id=? AND status='reserved' AND configured=FALSE AND observed=FALSE", target, root["instance_id"], root["user_id"], root["node_id"]).Find(&targets).Error; err != nil {
			return err
		}
	}
	if len(targets) == 0 {
		return nil
	}
	targetIDs := make([]uint64, 0, len(targets))
	for _, target := range targets {
		targetIDs = append(targetIDs, platform.Uint(target["id"]))
	}
	if err := db.Table("gpu_allocations").Where("id IN ?", targetIDs).Updates(platform.Row{"status": "cancelled", "released_at": now, "release_reason": "force stopped before GPU dispatch", "revision": gorm.Expr("revision+1")}).Error; err != nil {
		return err
	}
	instance, err := platform.Get(db, "instances", platform.Uint(root["instance_id"]), true)
	if err != nil {
		return err
	}
	return billing.InstanceUsage(ctx, db, instance, "gpu_reservation_cancelled", targetIDs)
}
