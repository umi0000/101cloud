package catalog

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

type catalogSourceTaskKey struct{}
type forceStoppedCatalogSourceKey struct{}

// catalogReadOnlyJob retains the wrapper lease and separately fences its unknown
// source. Stopping either task must invalidate late native observations.
func catalogReadOnlyJob(ctx context.Context, a *platform.App, job platform.Job) (context.Context, platform.Job, error) {
	var input map[string]json.RawMessage
	if err := json.Unmarshal(job.Parameters, &input); err != nil {
		return ctx, job, err
	}
	encoded, wrapped := input["source_task_id"]
	if !wrapped {
		return ctx, job, nil
	}
	var source json.Number
	if err := json.Unmarshal(encoded, &source); err != nil {
		return ctx, job, err
	}
	id := platform.Uint(source)
	root, err := platform.Get(a.DB.WithContext(ctx), "tasks", id, false)
	if err != nil {
		return ctx, job, err
	}
	if platform.Text(root["task_type"]) != job.Type || platform.Uint(root["node_id"]) != job.NodeID || platform.Uint(root["user_id"]) != job.UserID || platform.Text(root["status"]) != "unknown" || platform.Text(root["lease_owner"]) != "reconcile:"+strconv.FormatUint(job.ID, 10) {
		return ctx, job, platform.E(409, "task_lease_lost", "reconciliation source ownership changed")
	}
	job.Parameters = json.RawMessage(platform.Text(root["parameters_redacted"]))
	return context.WithValue(ctx, catalogSourceTaskKey{}, id), job, nil
}

func catalogTaskFence(ctx context.Context, tx *gorm.DB, job platform.Job, now time.Time) error {
	var task platform.Row
	if err := tx.Raw("SELECT id,node_id,user_id FROM tasks WHERE id=? AND lease_generation=? AND status='running' AND lease_until>? FOR UPDATE", job.ID, job.LeaseGeneration, now).Scan(&task).Error; err != nil {
		return err
	}
	if platform.Uint(task["id"]) != job.ID || platform.Uint(task["node_id"]) != job.NodeID || platform.Uint(task["user_id"]) != job.UserID {
		return platform.E(409, "task_lease_lost", "task no longer owns its live lease")
	}
	if rootID, wrapped := ctx.Value(catalogSourceTaskKey{}).(uint64); wrapped {
		var root platform.Row
		if err := tx.Raw("SELECT id,status,lease_owner FROM tasks WHERE id=? FOR UPDATE", rootID).Scan(&root).Error; err != nil {
			return err
		}
		if platform.Text(root["status"]) != "unknown" || platform.Text(root["lease_owner"]) != "reconcile:"+strconv.FormatUint(job.ID, 10) {
			return platform.E(409, "task_lease_lost", "reconciliation source ownership changed")
		}
	}
	if source, recovering := ctx.Value(forceStoppedCatalogSourceKey{}).(platform.Row); recovering {
		var root platform.Row
		if err := tx.Raw("SELECT * FROM tasks WHERE id=? FOR UPDATE", source["id"]).Scan(&root).Error; err != nil {
			return err
		}
		if platform.Text(root["status"]) != "cancelled" || platform.Text(root["error_code"]) != "task_force_stopped" || platform.Uint(root["lease_generation"]) != platform.Uint(source["lease_generation"]) || platform.Uint(root["revision"]) != platform.Uint(source["revision"]) {
			return platform.E(409, "task_lease_lost", "force-stopped reconciliation source changed")
		}
	}
	return nil
}

// ForceStopTask records uncertainty without deleting artifacts, releasing any
// reservations, or claiming that a partially observed external action completed.
// The caller holds the task lock and commits this with the cancelled task state.
func ForceStopTask(tx *gorm.DB, task platform.Row, now time.Time) error {
	switch platform.Text(task["task_type"]) {
	case "image.prepare":
		return tx.Table("image_nodes").Where("task_id=?", task["id"]).Updates(platform.Row{"status": "unknown", "error": `{"code":"task_force_stopped","message":"Orchestration stopped; external image state requires reconciliation"}`, "revision": gorm.Expr("revision+1")}).Error
	case "network.validate":
		// Malformed historical parameters cannot identify a projection safely,
		// but must not prevent revoking the task itself. Task metadata retains
		// the reconciliation requirement in that case.
		var args struct {
			BindingID json.RawMessage `json:"binding_id"`
			Revision  json.RawMessage `json:"revision"`
		}
		if err := json.Unmarshal([]byte(platform.Text(task["parameters_redacted"])), &args); err != nil {
			return nil
		}
		var bindingID, revision json.Number
		if err := json.Unmarshal(args.BindingID, &bindingID); err != nil {
			return nil
		}
		if err := json.Unmarshal(args.Revision, &revision); err != nil {
			return nil
		}
		return tx.Table("node_network_bindings").Where("id=? AND revision=?", platform.Uint(bindingID), platform.Uint(revision)).Updates(platform.Row{"status": "disabled", "validation_status": "failed", "validated_at": now, "validation_details": `{"error":"task_force_stopped","requires_reconciliation":true}`, "revision": gorm.Expr("revision+1")}).Error
	}
	return nil
}
