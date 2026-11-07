package instances

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"101cloud/backend/internal/catalog"
	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

// This mode shares the cleanup journal/inventory checks, but never cancels or
// waits on a native operation and never dispatches a replacement mutation.
type forceStopObservationKey struct{}

func forceStoppedSource(source platform.Row) bool {
	return platform.Text(source["status"]) == "cancelled" && platform.Text(source["error_code"]) == "task_force_stopped" && platform.Bool(object(object(source["result_redacted"])["force_stop"])["requires_reconciliation"])
}

func forceStopPending(reason string) error {
	return platform.Unknown(platform.EDetails(409, "force_stop_reconciliation_pending", "Force-stopped orchestration remains stopped. Native work is unresolved; resources and the instance mutation barrier are retained.", platform.Row{
		"reason": reason, "requires_reconciliation": true,
		"action": "Inspect the original native operation and restore its journal operation ID if missing, then reconcile again. Do not replay the stopped task.",
	}))
}

func reconcileForceStopped(ctx context.Context, a *platform.App, j platform.Job, source platform.Row) error {
	if platform.Uint(source["instance_id"]) != j.InstanceID || platform.Uint(source["node_id"]) != j.NodeID || platform.Uint(source["user_id"]) != j.UserID {
		return forceStopPending("source_identity_changed")
	}
	root := source
	seen := map[uint64]bool{}
	for platform.Text(root["task_type"]) == "instance.reconcile" {
		id := platform.Uint(object(root["parameters_redacted"])["source_task_id"])
		if id == 0 || seen[id] || len(seen) >= 32 {
			return forceStopPending("invalid_source_chain")
		}
		seen[id] = true
		var err error
		root, err = platform.Get(a.DB.WithContext(ctx), "tasks", id, false)
		if err != nil {
			return err
		}
		if (!forceStoppedSource(root) && !taskTerminal(root)) || platform.Uint(root["instance_id"]) != j.InstanceID || platform.Uint(root["node_id"]) != j.NodeID || platform.Uint(root["user_id"]) != j.UserID {
			return forceStopPending("source_chain_not_stopped")
		}
	}
	if !strings.HasPrefix(platform.Text(root["task_type"]), "instance.") {
		var err error
		if platform.Text(root["task_type"]) == "monitoring.console.prepare" {
			original := j
			original.Type = "monitoring.console.prepare"
			original.Parameters = json.RawMessage(jsonText(platform.Row{"source_task_id": platform.Text(root["id"])}))
			err = a.ReconcileJob(ctx, original)
		} else {
			err = catalog.ReconcileForceStoppedTask(ctx, a, j, root)
		}
		if err != nil {
			return err
		}
		// Domain verification also resolves cancelled recovery wrappers, not
		// just their root, while retaining every original terminal result.
		return a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
			ids, err := forceStopComponent(locked(db), root)
			if err != nil {
				return err
			}
			if err = workerLease(a, db, j); err != nil {
				return err
			}
			return db.Table("tasks").Where("id IN ? AND status='cancelled' AND error_code='task_force_stopped' AND COALESCE(JSON_UNQUOTE(JSON_EXTRACT(result_redacted,'$.force_stop.reconciliation_completed')),'false')<>'true'", ids).Updates(platform.Row{"is_instance_mutation": false, "result_redacted": gorm.Expr("JSON_SET(result_redacted,'$.force_stop.reconciliation_completed',true,'$.force_stop.reconciled_at',?)", a.Time().UTC().Format(time.RFC3339Nano)), "revision": gorm.Expr("revision+1")}).Error
		})
	}
	if j.InstanceID == 0 {
		return forceStopPending("managed_instance_identity_missing")
	}
	// Include both ancestors and descendants: a cancelled recovery wrapper may
	// contain the only operation ID for work started after the original failed.
	var tasks []platform.Row
	if err := a.DB.WithContext(ctx).Table("tasks").Where("instance_id=? AND node_id=? AND user_id=?", j.InstanceID, j.NodeID, j.UserID).Order("id").Find(&tasks).Error; err != nil {
		return err
	}
	connected := map[uint64]bool{platform.Uint(source["id"]): true}
	for {
		changed := false
		for _, task := range tasks {
			if platform.Text(task["task_type"]) != "instance.reconcile" {
				continue
			}
			id, parent := platform.Uint(task["id"]), platform.Uint(object(task["parameters_redacted"])["source_task_id"])
			if parent != 0 && (connected[id] || connected[parent]) {
				if !connected[id] || !connected[parent] {
					changed = true
				}
				connected[id], connected[parent] = true, true
			}
		}
		if !changed {
			break
		}
	}
	var stopped []platform.Row
	for _, task := range tasks {
		if !connected[platform.Uint(task["id"])] || platform.Uint(task["id"]) == j.ID {
			continue
		}
		if platform.Text(task["task_type"]) == "instance.reconcile" && platform.Bool(object(task["parameters_redacted"])["read_only_force_stop_reconcile"]) && !forceStoppedSource(task) {
			continue
		}
		switch platform.Text(task["status"]) {
		case "queued", "running", "waiting", "unknown":
			return forceStopPending("connected_orchestration_not_stopped")
		}
		if forceStoppedSource(task) {
			stopped = append(stopped, task)
		}
	}
	// Historical force-stops predate the GPU preflight rollback. Re-evaluate
	// the whole stopped source chain under its revoked leases before any native
	// observation; a proven unsent intent needs no physical release evidence.
	rolledBack := false
	err := a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if err := lockWorkerAccount(db, a, j, j.UserID); err != nil {
			return err
		}
		ids, err := forceStopComponent(locked(db), source)
		if err != nil {
			return err
		}
		var component []platform.Row
		if err = locked(db).Table("tasks").Where("id IN ? AND id<>?", ids, j.ID).Order("id").Find(&component).Error; err != nil {
			return err
		}
		for _, task := range component {
			if !taskTerminal(task) && !(platform.Text(task["task_type"]) == "instance.reconcile" && platform.Bool(object(task["parameters_redacted"])["read_only_force_stop_reconcile"])) {
				return forceStopPending("connected_orchestration_not_stopped")
			}
		}
		gpu, safe, err := undispatchedGPUComponent(db, component)
		if err != nil || !safe {
			return err
		}
		if err = rollbackUndispatchedGPU(ctx, a, db, gpu, a.Time()); err != nil {
			return err
		}
		if err = db.Table("tasks").Where("id IN ? AND status='cancelled' AND error_code='task_force_stopped'", ids).Updates(platform.Row{
			"is_instance_mutation": false,
			"result_redacted":      gorm.Expr("JSON_SET(COALESCE(result_redacted,JSON_OBJECT()),'$.force_stop.external_state','not_dispatched','$.force_stop.requires_reconciliation',false,'$.force_stop.instance_mutation_barrier',false,'$.force_stop.reconciliation_completed',true,'$.force_stop.reconciled_at',?)", a.Time().UTC().Format(time.RFC3339Nano)),
			"revision":             gorm.Expr("revision+1"),
		}).Error; err != nil {
			return err
		}
		rolledBack = true
		return nil
	})
	if err != nil {
		return err
	}
	if rolledBack {
		return a.TaskResult(ctx, j, platform.Row{"source_task_id": platform.Text(source["id"]), "reconciled": true, "read_only": true, "external_state": "not_dispatched", "resources_retained": true})
	}
	r, err := workerInstance(context.WithValue(ctx, forceStopObservationKey{}, true), a, j)
	if err != nil {
		return err
	}
	if err = stuckDeleteQuiescent(context.WithValue(ctx, forceStopObservationKey{}, true), a, j, r); err != nil {
		reason := platform.Text(object(platform.PublicError(err, true)["details"])["reason"])
		if reason == "" {
			reason = "native_quiescence_unresolved"
		}
		return forceStopPending(reason)
	}
	// No IP/GPU/quota/billing release follows from stopping orchestration. Native
	// quiescence only makes a new explicit administrator/user operation safe.
	err = a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if err := lockWorkerAccount(db, a, j, j.UserID); err != nil {
			return err
		}
		for _, old := range stopped {
			current, err := platform.Get(db, "tasks", platform.Uint(old["id"]), true)
			if err != nil {
				return err
			}
			if !forceStoppedSource(current) || platform.Uint(current["revision"]) != platform.Uint(old["revision"]) || platform.Uint(current["lease_generation"]) != platform.Uint(old["lease_generation"]) {
				return forceStopPending("source_changed_during_observation")
			}
			if err = db.Table("tasks").Where("id=?", current["id"]).Updates(platform.Row{
				"is_instance_mutation": false,
				"result_redacted":      gorm.Expr("JSON_SET(result_redacted, '$.force_stop.reconciled_at', ?, '$.force_stop.reconciliation_completed', true)", a.Time().UTC().Format(time.RFC3339Nano)),
				"revision":             gorm.Expr("revision+1"),
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return a.TaskResult(ctx, j, platform.Row{"source_task_id": platform.Text(source["id"]), "reconciled": true, "read_only": true, "resources_retained": true})
}
