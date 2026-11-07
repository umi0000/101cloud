package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Load observations are deliberately separate from hardware inventory. Use the
// shortest published scheduling interval so even an empty node is ready for its
// first placement, and retain support for instances bound to retired policies.
func pollNodeGPULoad(ctx context.Context, a *platform.App) error {
	var configs []string
	if err := a.DB.WithContext(ctx).Table("policy_versions").Where("kind='gpu-scheduling' AND status IN ('active','retired') AND effective_at<=?", a.Time()).Pluck("config", &configs).Error; err != nil {
		return err
	}
	var interval time.Duration
	for _, config := range configs {
		var policy platform.Row
		if err := json.Unmarshal([]byte(config), &policy); err != nil {
			return err
		}
		seconds := platform.Uint(policy["poll_interval_seconds"])
		if seconds == 0 || seconds > 86400 {
			continue
		}
		value := time.Duration(seconds) * time.Second
		if interval == 0 || value < interval {
			interval = value
		}
	}
	if interval == 0 {
		return nil
	}
	var ids []uint64
	if err := a.DB.WithContext(ctx).Table("nodes n").Select("n.id").Joins("LEFT JOIN node_gpu_load_snapshots s ON s.node_id=n.id").Where("n.administrative_state='active' AND EXISTS (SELECT 1 FROM node_gpus g WHERE g.node_id=n.id) AND (s.node_id IS NULL OR s.sampled_at<=? OR s.sampled_at>?)", a.Time().Add(-interval), a.Time()).Order("n.id").Pluck("n.id", &ids).Error; err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if err := refreshNodeGPULoad(ctx, a, id); err != nil {
			failures = append(failures, fmt.Errorf("node %d GPU load: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

func refreshNodeGPULoad(ctx context.Context, a *platform.App, id uint64) error {
	initial, err := platform.Get(a.DB.WithContext(ctx), "nodes", id, false)
	if err != nil {
		return err
	}
	// Timestamp the start, not the end: slow networking cannot renew old load.
	sampled := a.Time()
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := a.Agent(call, id, "GET", "/agent/v1/resources", nil)
	if err != nil {
		return err
	}
	cards, ok := raw["gpus"].([]any)
	if !ok {
		return platform.E(503, "observation_incomplete", "Agent GPU load observation is incomplete")
	}
	loads := make([]platform.Row, 0, len(cards))
	seen := make(map[string]bool, len(cards))
	for _, value := range cards {
		card, ok := value.(map[string]any)
		uuid := platform.Text(card["uuid"])
		if !ok || uuid == "" || seen[uuid] {
			return platform.E(503, "observation_incomplete", "Agent GPU load identities are incomplete or ambiguous")
		}
		seen[uuid] = true
		load := platform.Row{"uuid": uuid, "utilization_percent": card["utilization_percent"], "memory_utilization_percent": card["memory_utilization_percent"]}
		if platform.Text(card["error"]) != "" {
			load["error"] = "unavailable"
		}
		loads = append(loads, load)
	}
	data, err := json.Marshal(platform.Row{"gpus": loads})
	if err != nil {
		return err
	}
	// All network I/O precedes these locks. The connection and monotonic sample
	// fences prevent a late response from replacing newer or newly trusted data.
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, err := platform.Get(tx, "nodes", id, true)
		if err != nil {
			return err
		}
		if !nodeSameConnection(current, initial) {
			return platform.E(409, "configuration_conflict", "node connection changed during GPU load collection")
		}
		var previous platform.Row
		err = tx.Table("node_gpu_load_snapshots").Clauses(clause.Locking{Strength: "UPDATE"}).Where("node_id=?", id).Take(&previous).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if platform.Stamp(previous["sampled_at"]).After(sampled) {
			return platform.E(409, "observation_superseded", "a newer GPU load observation has already been recorded")
		}
		return tx.Table("node_gpu_load_snapshots").Clauses(clause.OnConflict{DoUpdates: clause.AssignmentColumns([]string{"sampled_at", "resources"})}).Create(platform.Row{"node_id": id, "sampled_at": sampled, "resources": string(data)}).Error
	})
}
