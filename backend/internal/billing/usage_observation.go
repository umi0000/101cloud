package billing

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

// Refresh stale metering evidence with read-only native requests, outside every
// financial transaction. Failed/absent observations never imply a stopped guest.
func refreshUsageObservation(ctx context.Context, a *platform.App, instanceID uint64) error {
	db := a.DB.WithContext(ctx)
	before, err := platform.Get(db, "instances", instanceID, false)
	if err != nil {
		return err
	}
	if before["deleted_at"] != nil || before["creation_failed_at"] != nil || freshUsageObservation(before, now()) || platform.Text(before["incus_uuid"]) == "" {
		return nil
	}
	var online int64
	if err = db.Table("nodes").Where("id=? AND health='online'", before["node_id"]).Count(&online).Error; err != nil {
		return err
	}
	if online == 0 {
		return nil
	}
	started := now()
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	path := "/1.0/instances/" + url.PathEscape(platform.Text(before["incus_name"])) + "?project=" + url.QueryEscape(platform.Text(before["project_name"]))
	response, err := a.Agent(probe, platform.Uint(before["node_id"]), http.MethodGet, path, nil)
	if err != nil {
		return nil
	}
	native, ok := response["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	config, ok := native["expanded_config"].(map[string]any)
	if !ok {
		config, _ = native["config"].(map[string]any)
	}
	if platform.Text(native["name"]) != platform.Text(before["incus_name"]) || platform.Text(config["volatile.uuid"]) != platform.Text(before["incus_uuid"]) {
		return nil
	}
	state := ""
	switch platform.Text(native["status"]) {
	case "Stopped":
		state = "stopped"
	case "Running":
		state = "running"
	default:
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if _, err := account(tx, platform.Uint(before["user_id"])); err != nil {
			return err
		}
		current, err := platform.Get(tx, "instances", instanceID, true)
		if err != nil {
			return err
		}
		if current["deleted_at"] != nil || current["creation_failed_at"] != nil || platform.Text(current["revision"]) != platform.Text(before["revision"]) || stamp(current["observed_at"]).After(started) {
			return nil
		}
		for _, field := range []string{"user_id", "node_id", "project_name", "incus_name", "incus_uuid"} {
			if platform.Text(current[field]) != platform.Text(before[field]) {
				return nil
			}
		}
		var active int64
		if err = tx.Table("tasks").Where("instance_id=? AND is_instance_mutation=1 AND status IN ('queued','running','waiting')", instanceID).Count(&active).Error; err != nil {
			return err
		}
		if active != 0 {
			return nil
		}
		var stillOnline int64
		if err = tx.Table("nodes").Where("id=? AND health='online'", current["node_id"]).Count(&stillOnline).Error; err != nil {
			return err
		}
		if stillOnline == 0 {
			return nil
		}
		return tx.Table("instances").Where("id=?", instanceID).Updates(row{"observed_state": state, "observed_at": started, "revision": gorm.Expr("revision+1")}).Error
	})
}
