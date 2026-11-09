package billing

import (
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Observation may win the race with the worker's final confirmation. Reuse the
// original operation key so it captures the first metering cutover, not a later
// 0->0 retry. Pure repeated RUNNING observations do not create START events.
func prepareObservedInstanceEvent(db *gorm.DB, instance row, state string, observed, at time.Time) (func() error, error) {
	ss, err := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id"), "usage_sessions", "instance_id=? AND status='active'", instance["id"])
	if err != nil {
		return nil, err
	}
	changed, last := false, uint64(0)
	for _, s := range ss {
		if platform.Uint(s["id"]) > last {
			last = platform.Uint(s["id"])
		}
		if state == "running" && platform.Text(s["charge_kind"]) == "base_compute" && platform.Text(s["metering_state"]) == "running" {
			return nil, nil
		}
		if state == "stopped" && platform.Text(s["metering_state"]) == "running" && !observed.Before(stamp(s["started_at"])) {
			changed = true
		}
	}
	if state == "stopped" && !changed {
		return nil, nil
	}
	// Lock only the account side of this current read, never a task (the worker
	// obtains its task lease before its instance lock).
	var task row
	kinds := []string{"instance.start", "instance.create"}
	if state == "stopped" {
		kinds = []string{"instance.stop", "instance.resize", "instance.delete", "instance.start", "instance.create"}
	}
	err = db.Table("credit_accounts a").Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "a"}}).Select("t.id,t.task_type").Joins("JOIN tasks t ON t.user_id=a.user_id").Where("a.user_id=? AND t.instance_id=? AND t.task_type IN ? AND t.status IN ('queued','running','waiting','unknown')", instance["user_id"], instance["id"], kinds).Order("t.id DESC").Take(&task).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	if err == gorm.ErrRecordNotFound {
		task = nil
	}
	if state == "running" && task == nil {
		// Rebuilding a missing session does not establish a new user START.
		return nil, nil
	}
	eventType, published := "instance.stop", true
	key := "observed.stop:" + sid(instance["id"]) + ":" + sid(last)
	if task != nil {
		if state == "running" {
			eventType = "instance.start"
			if platform.Text(task["task_type"]) == "instance.create" {
				eventType, published = "instance.create", false
			}
		}
		key = "task:" + sid(task["id"]) + ":" + eventType
	}
	return PrepareInstanceEvent(db, instance, eventType, key, at, published)
}
