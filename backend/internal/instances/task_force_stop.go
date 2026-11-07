package instances

import (
	"sort"
	"strconv"
	"strings"

	"101cloud/backend/internal/catalog"
	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func taskTerminal(r platform.Row) bool {
	switch platform.Text(r["status"]) {
	case "succeeded", "failed", "cancelled":
		return true
	}
	return false
}

// Ownership is not initiation: enforcement and administrator-created work can
// belong to a member without granting that member authority to stop it.
func taskInitiatedBy(db *gorm.DB, r platform.Row, actor uint64) (bool, error) {
	p := object(r["parameters_redacted"])
	if strings.HasPrefix(platform.Text(p["reason"]), "credit-enforcement:") || platform.Text(r["task_type"]) == "credit_enforcement_reconcile" {
		return false, nil
	}
	if initiated := platform.Uint(p["initiated_by_user_id"]); initiated != 0 {
		return initiated == actor, nil
	}
	// Pre-marker tasks remain controllable when the original committed HTTP
	// response and its mutation audit unambiguously establish the actor.
	var count int64
	err := db.Table("backend_requests b").Joins("JOIN audit_logs l ON JSON_UNQUOTE(JSON_EXTRACT(l.result_redacted,'$.request_id'))=JSON_UNQUOTE(JSON_EXTRACT(b.response,'$.request_id'))").Where("l.actor_user_id=? AND l.event_type='api.mutation' AND b.status_code BETWEEN 200 AND 299 AND JSON_UNQUOTE(JSON_EXTRACT(b.response,'$.data.task_id'))=?", actor, strconv.FormatUint(platform.Uint(r["id"]), 10)).Count(&count).Error
	return count > 0, err
}

func taskRequestDTO(c *gin.Context, db *gorm.DB, r platform.Row) (platform.Row, error) {
	out, err := taskDTO(db, r)
	if err != nil {
		return nil, err
	}
	allowed := c.GetBool("admin")
	if !allowed {
		allowed, err = taskInitiatedBy(db, r, platform.Current(c).ID)
		if err != nil {
			return nil, err
		}
	}
	out["can_force_stop"] = allowed && !taskTerminal(r)
	if !allowed {
		out["force_stop_blocked_reason"] = "task_stop_not_allowed"
	}
	return out, nil
}

// Reconciliation wrappers are not independent cancellation sources. Revoking
// either endpoint of their graph revokes every still-active source and wrapper.
func forceStopComponent(db *gorm.DB, selected platform.Row) ([]uint64, error) {
	var tasks []platform.Row
	if err := db.Table("tasks").Select("id,task_type,parameters_redacted").Where("user_id <=> ? AND node_id <=> ? AND instance_id <=> ?", selected["user_id"], selected["node_id"], selected["instance_id"]).Order("id").Find(&tasks).Error; err != nil {
		return nil, err
	}
	identities := make(map[uint64]bool, len(tasks))
	for _, task := range tasks {
		identities[platform.Uint(task["id"])] = true
	}
	included := map[uint64]bool{platform.Uint(selected["id"]): true}
	for {
		changed := false
		for _, task := range tasks {
			if platform.Text(task["task_type"]) != "instance.reconcile" {
				continue
			}
			id, source := platform.Uint(task["id"]), platform.Uint(object(task["parameters_redacted"])["source_task_id"])
			if source != 0 && (included[id] || included[source]) {
				if !identities[source] {
					return nil, conflict("invalid_recovery_source", "Recovery source does not share this task identity")
				}
				if !included[id] {
					included[id], changed = true, true
				}
				if !included[source] {
					included[source], changed = true, true
				}
			}
		}
		if !changed {
			break
		}
	}
	ids := make([]uint64, 0, len(included))
	for id := range included {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func forceStopTask(c *gin.Context, a *platform.App) (any, error) {
	id, err := platform.ID(c, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if err = platform.Decode(c, &in); err != nil {
		return nil, err
	}
	if !in.Confirm {
		return nil, bad("confirm must be true")
	}
	db := platform.DB(c, a)
	// Authorize and check the caller's version before acquiring the graph locks.
	// Execution cancellation is strictly postcommit, including audit failures.
	selected, err := platform.Get(db, "tasks", id, false)
	if err != nil {
		return nil, err
	}
	if !c.GetBool("admin") && platform.Uint(selected["user_id"]) != platform.Current(c).ID {
		return nil, platform.E(404, "not_found", "resource not found")
	}
	if err = platform.Revision(c, selected); err != nil {
		return nil, err
	}
	if !c.GetBool("admin") {
		allowed, e := taskInitiatedBy(db, selected, platform.Current(c).ID)
		if e != nil {
			return nil, e
		}
		if !allowed {
			return nil, platform.E(403, "task_stop_not_allowed", "仅能停止本人主动发起的任务；系统安全或欠费执行任务需要管理员处理。")
		}
	}
	if taskTerminal(selected) {
		platform.SetETag(c, selected)
		return taskRequestDTO(c, db, selected)
	}
	ids, err := forceStopComponent(db, selected)
	if err != nil {
		return nil, err
	}
	if selected["instance_id"] != nil {
		if err = lockOwnerAccount(db, platform.Uint(selected["user_id"])); err != nil {
			return nil, err
		}
	}
	var tasks []platform.Row
	if err = locked(db).Table("tasks").Where("id IN ?", ids).Order("id").Find(&tasks).Error; err != nil {
		return nil, err
	}
	for _, task := range tasks {
		if platform.Uint(task["id"]) == id {
			if err = platform.Revision(c, task); err != nil {
				return nil, err
			}
			if taskTerminal(task) {
				platform.SetETag(c, task)
				return taskRequestDTO(c, db, task)
			}
		}
	}
	// A wrapper may have committed just before the source locks were acquired.
	// Current locking reads close that insertion race; future reconciliation
	// must first acquire an already-held source row and sees it cancelled.
	currentIDs, e := forceStopComponent(locked(db), selected)
	if e != nil {
		return nil, e
	}
	if len(currentIDs) != len(ids) {
		ids = currentIDs
		if err = locked(db).Table("tasks").Where("id IN ?", ids).Order("id").Find(&tasks).Error; err != nil {
			return nil, err
		}
	}
	if instanceID := platform.Uint(selected["instance_id"]); instanceID != 0 {
		if _, err = platform.Get(db, "instances", instanceID, true); err != nil {
			return nil, err
		}
	}
	now := a.Time()
	mutationComponent := false
	for _, task := range tasks {
		kind := platform.Text(task["task_type"])
		if platform.Bool(task["is_instance_mutation"]) || (strings.HasPrefix(kind, "instance.") && kind != "instance.reconcile") {
			mutationComponent = true
		}
	}
	// Revoke every execution before inspecting the dispatch journal. The task
	// row locks serialize this with native submission; no later worker can add
	// dispatch evidence or mutate allocations under the revoked lease.
	for _, task := range tasks {
		if taskTerminal(task) {
			continue
		}
		if err = db.Table("tasks").Where("id=?", task["id"]).Updates(platform.Row{"status": "cancelled", "finished_at": now, "lease_owner": nil, "lease_until": nil, "lease_generation": gorm.Expr("lease_generation+1")}).Error; err != nil {
			return nil, err
		}
	}
	gpuRoot, gpuUnsent, err := undispatchedGPUComponent(db, tasks)
	if err != nil {
		return nil, err
	}
	if gpuUnsent {
		if err = rollbackUndispatchedGPU(c.Request.Context(), a, db, gpuRoot, now); err != nil {
			return nil, err
		}
		if err = db.Table("tasks").Where("id IN ? AND status='cancelled' AND error_code='task_force_stopped'", ids).Updates(platform.Row{"is_instance_mutation": false, "result_redacted": gorm.Expr("JSON_SET(COALESCE(result_redacted,JSON_OBJECT()),'$.force_stop.external_state','not_dispatched','$.force_stop.requires_reconciliation',false,'$.force_stop.instance_mutation_barrier',false,'$.force_stop.reconciliation_completed',true)"), "revision": gorm.Expr("revision+1")}).Error; err != nil {
			return nil, err
		}
	}
	var domainCallbacks []func()
	for _, task := range tasks {
		if taskTerminal(task) {
			continue
		}
		// The graph cannot cross identities. Reject malformed references rather
		// than allowing an administrator wrapper to confer member privileges.
		if platform.Uint(task["user_id"]) != platform.Uint(selected["user_id"]) || platform.Uint(task["node_id"]) != platform.Uint(selected["node_id"]) || platform.Uint(task["instance_id"]) != platform.Uint(selected["instance_id"]) {
			return nil, conflict("invalid_recovery_source", "Recovery task identity changed")
		}
		var steps []platform.Row
		if err = db.Table("task_steps").Where("task_id=?", task["id"]).Find(&steps).Error; err != nil {
			return nil, err
		}
		dispatched := platform.Text(task["status"]) == "unknown" || (platform.Text(task["status"]) == "running" && len(steps) == 0)
		for _, step := range steps {
			journal := object(step["result_redacted"])
			if platform.Text(step["operation_id"]) != "" || platform.Bool(journal["native_dispatch_started"]) {
				dispatched = true
			}
			if !platform.Bool(journal["native_dispatch_fenced"]) && platform.Text(step["status"]) != "succeeded" {
				dispatched = true
			}
		}
		external := "not_dispatched"
		if dispatched {
			external = "requires_reconciliation"
		}
		requires := dispatched || platform.Bool(task["is_instance_mutation"])
		switch platform.Text(task["task_type"]) {
		case "instance.reconcile", "image.prepare", "node.probe", "node.drain", "network.validate":
			requires = true
		}
		if gpuUnsent {
			external, requires = "not_dispatched", false
		}
		result := object(task["result_redacted"])
		result["force_stop"] = platform.Row{"orchestration_stopped": true, "external_state": external, "requires_reconciliation": requires, "resources_retained": true, "instance_mutation_barrier": requires && mutationComponent && platform.Uint(task["instance_id"]) != 0}
		object(result["force_stop"])["previous_status"] = task["status"]
		changes := platform.Row{"status": "cancelled", "error_code": "task_force_stopped", "finished_at": now, "revision": gorm.Expr("revision+1"), "result_redacted": jsonText(result)}
		if gpuUnsent {
			changes["is_instance_mutation"] = false
		}
		if err = db.Table("tasks").Where("id=?", task["id"]).Updates(changes).Error; err != nil {
			return nil, err
		}
		// Open steps remain explicit unknown evidence, not fictional cancelled
		// native operations. Unsent intent is marked cancelled only when fenced.
		for _, step := range steps {
			switch platform.Text(step["status"]) {
			case "succeeded", "failed", "cancelled":
				continue
			}
			status := "unknown"
			if platform.Text(step["operation_id"]) == "" && !platform.Bool(object(step["result_redacted"])["native_dispatch_started"]) && platform.Bool(object(step["result_redacted"])["native_dispatch_fenced"]) {
				status = "cancelled"
			}
			changes := platform.Row{"status": status, "error_code": "task_force_stopped"}
			if status == "cancelled" {
				changes["finished_at"] = now
			}
			if err = db.Table("task_steps").Where("id=?", step["id"]).Updates(changes).Error; err != nil {
				return nil, err
			}
		}
		if err = catalog.ForceStopTask(db, task, now); err != nil {
			return nil, err
		}
		callback, stopErr := a.StopTaskResources(db, task, now)
		if stopErr != nil {
			return nil, stopErr
		}
		if callback != nil {
			domainCallbacks = append(domainCallbacks, callback)
		}
		if err = platform.Audit(c.Request.Context(), db, platform.Current(c).ID, platform.Uint(task["id"]), platform.Uint(task["instance_id"]), "task.force_stopped", platform.Row{"selected_task_id": strconv.FormatUint(id, 10), "previous_status": task["status"], "external_state": external, "resources_retained": true}); err != nil {
			return nil, err
		}
	}
	hooks, _ := c.Get("after_commit")
	callbacks, _ := hooks.([]func())
	callbacks = append(callbacks, func() {
		a.CancelTaskExecutions(ids...)
		for _, callback := range domainCallbacks {
			callback()
		}
	})
	c.Set("after_commit", callbacks)
	current, err := platform.Get(db, "tasks", id, false)
	if err != nil {
		return nil, err
	}
	platform.SetETag(c, current)
	return taskRequestDTO(c, db, current)
}
