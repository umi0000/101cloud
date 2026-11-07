package billing

import (
	"context"
	"encoding/json"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

type enforcementJobKey struct{}

func enforcementManualReconcile(ctx context.Context) bool {
	job, ok := ctx.Value(enforcementJobKey{}).(platform.Job)
	if !ok {
		return false
	}
	var input struct {
		Manual bool `json:"manual_reconcile"`
	}
	return json.Unmarshal(job.Parameters, &input) == nil && input.Manual
}

// Pollers have no task lease. A registered reconciliation job carries its lease
// through every observation and continuation transaction, including advanceEvent.
func enforcementWriteLease(ctx context.Context, a *platform.App, db *gorm.DB) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	j, ok := ctx.Value(enforcementJobKey{}).(platform.Job)
	if !ok {
		return nil
	}
	task, err := platform.Get(db, "tasks", j.ID, true)
	if err != nil {
		return err
	}
	if platform.Text(task["status"]) != "running" || platform.Uint(task["lease_generation"]) != j.LeaseGeneration || !platform.Stamp(task["lease_until"]).After(a.Time()) || platform.Uint(task["user_id"]) != j.UserID || platform.Text(task["task_type"]) != "credit_enforcement_reconcile" {
		return platform.E(409, "task_lease_lost", "Credit reconciliation no longer owns its task lease; no financial or enforcement changes were applied.")
	}
	return nil
}
