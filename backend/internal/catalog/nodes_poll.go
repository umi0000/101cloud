package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

// Inventory is a node property, not a side effect of running instances or GPU
// automation. Also observe maintenance/disabled nodes without enabling them.
func pollNodeInventory(ctx context.Context, a *platform.App) error {
	var ids []uint64
	if err := a.DB.WithContext(ctx).Table("nodes").Order("id").Pluck("id", &ids).Error; err != nil {
		return err
	}
	jobs := make(chan uint64)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	for range min(8, len(ids)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if err := refreshNodeInventory(ctx, a, id); err != nil {
					mu.Lock()
					failures = append(failures, fmt.Errorf("node %d inventory: %w", id, err))
					mu.Unlock()
				}
			}
		}()
	}
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	return errors.Join(failures...)
}

func refreshNodeInventory(ctx context.Context, a *platform.App, id uint64) error {
	initial, err := platform.Get(a.DB.WithContext(ctx), "nodes", id, false)
	if err != nil {
		return err
	}
	observation, observationErr := a.ObserveNodeInventory(ctx, initial)
	if errors.Is(observationErr, platform.ErrNodeObservationDeferred) {
		return nil
	}
	healthAt, resourcesAt := observation.HealthSampledAt, observation.Resource.SampledAt
	if healthAt.IsZero() {
		return observationErr
	}
	var snapshot platform.Row
	var data []byte
	if observationErr == nil {
		snapshot, observationErr = nodeResourceSnapshot(observation.Resource.Data)
		if observationErr == nil {
			data, observationErr = json.Marshal(snapshot)
		}
	}
	// Use the parent context so a timed-out Agent request can record failure;
	// shutdown cancellation still prevents writes. Never refresh inventory on failure.
	err = a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, err := platform.Get(tx, "nodes", id, true)
		if err != nil {
			return err
		}
		if err = nodeObservationFence(current, initial, healthAt, resourcesAt); err != nil {
			return err
		}
		state := "offline"
		updates := platform.Row{}
		if observation.Online {
			state = "online"
			updates["last_heartbeat_at"] = healthAt
			updates["connection_verified_at"] = healthAt
		}
		if observationErr == nil && resourcesAt.After(platform.Stamp(current["resources_sampled_at"])) {
			if _, err = nodeSaveGPUs(tx, a, id, snapshot, resourcesAt); err != nil {
				return err
			}
			updates["resources"] = string(data)
			updates["resources_sampled_at"] = resourcesAt
		}
		updates["health"] = state
		// Pure observations must not invalidate an administrator's edit ETag.
		if platform.Text(current["health"]) != state {
			updates["revision"] = gorm.Expr("revision + 1")
			updates["updated_at"] = a.Time()
		}
		return tx.Table("nodes").Where("id = ?", id).Updates(updates).Error
	})
	return errors.Join(observationErr, err)
}
