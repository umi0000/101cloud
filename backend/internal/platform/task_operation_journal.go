package platform

import (
	"context"
	"errors"
	"net/url"

	"gorm.io/gorm"
)

// A response may arrive after revocation. Preserve its external identity in the
// original attempt only; this grants no status or resource-write authority.
func (a *App) recordTaskOperation(ctx context.Context, dispatch taskDispatch, nodeID uint64, operation *url.URL) error {
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		task, err := Get(tx, "tasks", dispatch.job.ID, true)
		if err != nil {
			return err
		}
		var step Row
		if err = tx.Raw("SELECT * FROM task_steps WHERE task_id=? AND step_key=? AND attempt=? FOR UPDATE", dispatch.job.ID, dispatch.key, dispatch.job.LeaseGeneration).Scan(&step).Error; err != nil {
			return err
		}
		if Uint(step["id"]) == 0 {
			return errors.New("native dispatch omitted durable step intent")
		}
		if old := Text(step["operation_id"]); old != "" && old != operation.String() {
			return errors.New("native operation identity changed within one attempt")
		}
		changes := Row{"operation_id": operation.String(), "node_id": nodeID, "project_name": operation.Query().Get("project")}
		status := Text(step["status"])
		if Uint(task["lease_generation"]) == dispatch.job.LeaseGeneration && Text(task["status"]) == "running" && Stamp(task["lease_until"]).After(a.Time()) && (status == "running" || status == "waiting" || status == "unknown") {
			changes["status"] = "waiting"
		}
		return tx.Table("task_steps").Where("id=?", step["id"]).Updates(changes).Error
	})
}
