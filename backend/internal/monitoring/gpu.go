package monitoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"101cloud/backend/internal/instances"
	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type gpuAutomaticPolicy struct {
	Idle struct {
		Enabled bool   `json:"enabled"`
		Timeout uint64 `json:"timeout_seconds"`
		Action  string `json:"action"`
	} `json:"idle"`
	Rebalance struct {
		Enabled bool   `json:"enabled"`
		Live    bool   `json:"allow_live"`
		Max     string `json:"max_hourly_pt"`
	} `json:"rebalance"`
}

type gpuPollKey struct {
	Node   uint64
	Policy string
}

var gpuPollState = struct {
	sync.Mutex
	next map[gpuPollKey]time.Time
}{next: map[gpuPollKey]time.Time{}}

// The detach mirror deliberately excludes stop_instance. Read that saved action
// explicitly so an idle-stop-only policy is still evaluated without enabling detach.
const gpuAutomaticCandidateFilter = "node_id=? AND deleted_at IS NULL AND creation_failed_at IS NULL AND observed_state='running' AND (auto_release_gpu=1 OR auto_rebind_gpu=1 OR (JSON_EXTRACT(gpu_policy,'$.idle.enabled')=CAST('true' AS JSON) AND JSON_UNQUOTE(JSON_EXTRACT(gpu_policy,'$.idle.action'))='stop_instance'))"

var gpuHighWindowConflict = clause.OnConflict{DoUpdates: clause.AssignmentColumns([]string{"high_since", "last_sampled_at", "updated_at"})}
var gpuIdleWindowConflict = clause.OnConflict{DoUpdates: clause.AssignmentColumns([]string{"agent_boot_id", "runtime_started_at", "last_sequence", "last_sampled_at", "idle_since", "updated_at"})}

func evaluateGPU(ctx context.Context, a *platform.App) error {
	var nodes []struct{ ID uint64 }
	if err := a.DB.WithContext(ctx).Table("nodes").Select("id").Where("administrative_state='active' AND EXISTS (SELECT 1 FROM node_gpus ng WHERE ng.node_id=nodes.id)").Find(&nodes).Error; err != nil {
		return err
	}
	var failures []error
	for _, n := range nodes {
		var candidates []platform.Row
		if err := a.DB.WithContext(ctx).Table("instances").Select("*,JSON_UNQUOTE(JSON_EXTRACT(policy_versions,'$.\"gpu-scheduling\"')) AS scheduling_policy_id").Where(gpuAutomaticCandidateFilter, n.ID).Find(&candidates).Error; err != nil {
			failures = append(failures, err)
			continue
		}
		groups := map[string][]platform.Row{}
		for _, instance := range candidates {
			id := platform.Text(instance["scheduling_policy_id"])
			groups[id] = append(groups[id], instance)
		}
		var initial platform.Row
		var observation platform.NodeResourceObservation
		var observationErr error
		var observed bool
		for policyID, instances := range groups {
			scheduling, err := a.Policy(ctx, "gpu-scheduling", policyID)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			interval := platform.Uint(scheduling["poll_interval_seconds"])
			if interval == 0 || platform.Uint(scheduling["metrics_max_age_seconds"]) == 0 {
				failures = append(failures, platform.E(422, "invalid_policy", "GPU sampling intervals are required"))
				continue
			}
			key := gpuPollKey{Node: n.ID, Policy: policyID}
			gpuPollState.Lock()
			due := !a.Time().Before(gpuPollState.next[key])
			if due {
				gpuPollState.next[key] = a.Time().Add(time.Duration(interval) * time.Second)
			}
			gpuPollState.Unlock()
			if !due {
				continue
			}
			if !observed {
				observed = true
				initial, observationErr = platform.Get(a.DB.WithContext(ctx), "nodes", n.ID, false)
				if observationErr == nil {
					observation, observationErr = a.ObserveNodeResources(ctx, initial)
				}
				if observationErr != nil && !errors.Is(observationErr, platform.ErrNodeObservationDeferred) {
					failures = append(failures, observationErr)
				}
			}
			if observationErr != nil {
				continue
			}
			err = sampleGPUResources(ctx, a, initial, observation, policyID, scheduling)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			for _, instance := range instances {
				if err = evaluateGPUInstance(ctx, a, platform.Uint(instance["id"])); err != nil {
					failures = append(failures, err)
				}
			}
		}
	}
	return errors.Join(failures...)
}
func metricDecimal(v any) (decimal.Decimal, bool) {
	if v == nil {
		return decimal.Zero, false
	}
	d, e := decimal.NewFromString(platform.Text(v))
	return d, e == nil
}
func sampleGPUResources(ctx context.Context, a *platform.App, initial platform.Row, observation platform.NodeResourceObservation, policyID string, policy platform.Row) error {
	sw, ok := metricDecimal(policy["sm_weight"])
	if !ok {
		return fmt.Errorf("invalid scheduling weight")
	}
	mw, ok := metricDecimal(policy["memory_weight"])
	if !ok {
		return fmt.Errorf("invalid scheduling weight")
	}
	high, ok := metricDecimal(policy["source_high_load_pct"])
	if !ok {
		return fmt.Errorf("invalid scheduling threshold")
	}
	gpus := observation.Data["gpus"].([]any)
	highByUUID := make(map[string]bool, len(gpus))
	seen := make([]string, 0, len(gpus))
	maxPercent := decimal.NewFromInt(100)
	for _, value := range gpus {
		card := value.(map[string]any)
		uuid, _ := card["uuid"].(string)
		if uuid == "" {
			continue
		}
		if _, duplicate := highByUUID[uuid]; duplicate {
			return platform.E(503, "observation_incomplete", "Agent GPU load identities are ambiguous")
		}
		seen = append(seen, uuid)
		sm, smOK := metricDecimal(card["utilization_percent"])
		memory, memoryOK := metricDecimal(card["memory_utilization_percent"])
		highByUUID[uuid] = platform.Text(card["error"]) == "" && smOK && memoryOK &&
			sm.Sign() >= 0 && sm.LessThanOrEqual(maxPercent) && memory.Sign() >= 0 && memory.LessThanOrEqual(maxPercent) &&
			sm.Mul(sw).Add(memory.Mul(mw)).GreaterThanOrEqual(high)
	}
	node, observedAt := platform.Uint(initial["id"]), observation.SampledAt
	interval := time.Duration(platform.Uint(policy["poll_interval_seconds"])) * time.Second
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, e := platform.Get(tx, "nodes", node, true)
		if e != nil {
			return e
		}
		if !platform.SameNodeConnection(current, initial) {
			return platform.E(409, "configuration_conflict", "node connection changed during GPU evaluation")
		}
		q := tx.Table("metric_gpu_high_windows").Where("node_id=? AND policy_version_id=?", node, policyID)
		var windows []platform.Row
		if e := q.Session(&gorm.Session{}).Clauses(clause.Locking{Strength: "UPDATE"}).Find(&windows).Error; e != nil {
			return e
		}
		previous := make(map[string]platform.Row, len(windows))
		for _, window := range windows {
			if !observedAt.After(platform.Stamp(window["last_sampled_at"])) {
				return nil // A reused or superseded observation cannot change continuity.
			}
			previous[platform.Text(window["gpu_uuid"])] = window
		}
		updatedAt := a.Time()
		for _, uuid := range seen {
			var since any
			if highByUUID[uuid] {
				since = observedAt
				prior := previous[uuid]
				if prior["high_since"] != nil && observedAt.Sub(platform.Stamp(prior["last_sampled_at"])) <= 2*interval {
					since = platform.Stamp(prior["high_since"])
				}
			}
			row := map[string]any{"node_id": node, "gpu_uuid": uuid, "policy_version_id": policyID, "high_since": since, "last_sampled_at": observedAt, "updated_at": updatedAt}
			if e := tx.Table("metric_gpu_high_windows").Clauses(gpuHighWindowConflict).Create(row).Error; e != nil {
				return e
			}
		}
		if len(seen) > 0 {
			q = q.Where("gpu_uuid NOT IN ?", seen)
		}
		return q.Updates(map[string]any{"high_since": nil, "last_sampled_at": observedAt, "updated_at": updatedAt}).Error
	})
}

func gpuHighWindowReady(now time.Time, window platform.Row, maxAge, duration time.Duration) bool {
	last := platform.Stamp(window["last_sampled_at"])
	since := platform.Stamp(window["high_since"])
	return !since.IsZero() && !since.After(last) && platform.ObservationFresh(now, last, maxAge) && last.Sub(since) >= duration
}

func evaluateGPUInstance(ctx context.Context, a *platform.App, id uint64) error {
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var placement struct{ UserID uint64 }
		if e := tx.Table("instances").Select("user_id").Where("id=?", id).Take(&placement).Error; e != nil {
			return e
		}
		var accounts []platform.Row
		if e := tx.Table("credit_accounts").Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id=?", placement.UserID).Find(&accounts).Error; e != nil {
			return e
		}
		instance, e := platform.Get(tx, "instances", id, true)
		if e != nil {
			return e
		}
		if instance["creation_failed_at"] != nil || platform.Text(instance["observed_state"]) != "running" || instance["deleted_at"] != nil {
			return nil
		}
		idle, e := a.InstancePolicy(ctx, instance, "gpu-idle")
		if e != nil {
			return e
		}
		scheduling, e := a.InstancePolicy(ctx, instance, "gpu-scheduling")
		if e != nil {
			return e
		}
		var bindings map[string]string
		if e = json.Unmarshal([]byte(platform.Text(instance["policy_versions"])), &bindings); e != nil {
			return e
		}
		maxAge := platform.Uint(scheduling["metrics_max_age_seconds"])
		if maxAge == 0 {
			return platform.E(422, "invalid_policy", "GPU metric maximum age is required")
		}
		var policy gpuAutomaticPolicy
		if e = json.Unmarshal([]byte(platform.Text(instance["gpu_policy"])), &policy); e != nil {
			return e
		}
		var active int64
		if e = tx.Table("tasks").Where("instance_id=? AND is_instance_mutation=1 AND status IN ('queued','running','waiting','unknown')", id).Count(&active).Error; e != nil {
			return e
		}
		if active > 0 {
			return nil
		}
		if _, e := platform.Get(tx, "nodes", platform.Uint(instance["node_id"]), true); e != nil {
			return e
		}
		var sample platform.Row
		e = tx.Table("instance_metric_samples").Clauses(clause.Locking{Strength: "SHARE"}).Where("instance_id=?", id).Order("sampled_at DESC,id DESC").Take(&sample).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return nil
		}
		if e != nil {
			return e
		}
		at := platform.Stamp(sample["sampled_at"])
		fresh := !at.IsZero() && !at.After(a.Time()) && a.Time().Sub(at) <= time.Duration(maxAge)*time.Second
		var gpus []platform.Row
		if e = tx.Table("instance_gpu_metric_samples").Clauses(clause.Locking{Strength: "SHARE"}).Where("sample_id=?", platform.Uint(sample["id"])).Find(&gpus).Error; e != nil {
			return e
		}
		var allocations []platform.Row
		if e = tx.Table("gpu_allocations ga").Select("ga.id,ng.uuid").Joins("JOIN node_gpus ng ON ng.id=ga.gpu_id").Where("ga.instance_id=? AND ga.status='active'", id).Find(&allocations).Error; e != nil {
			return e
		}
		byUUID := map[string]uint64{}
		for _, r := range allocations {
			byUUID[platform.Text(r["uuid"])] = platform.Uint(r["id"])
		}
		var detach, rebind []uint64
		allIdle := len(gpus) > 0
		seen := []string{}
		sequence := platform.Uint(sample["sample_sequence"])
		runtime := platform.Stamp(sample["runtime_started_at"])
		boot := sample["agent_boot_id"]
		node := platform.Uint(instance["node_id"])
		for _, g := range gpus {
			uuid := platform.Text(g["gpu_uuid"])
			seen = append(seen, uuid)
			safe := fresh && platform.Bool(sample["collection_complete"]) && platform.Bool(sample["attribution_complete"]) && platform.Text(sample["instance_state"]) == "running" && !runtime.IsZero() && platform.Bool(g["attribution_complete"]) && platform.Text(g["reference_scan_quality"]) == "complete" && platform.Text(g["gpu_state"]) == "idle_candidate" && g["process_count"] != nil && platform.Uint(g["process_count"]) == 0 && g["reference_count"] != nil && platform.Uint(g["reference_count"]) == 0 && g["memory_used_bytes"] != nil && platform.Uint(g["memory_used_bytes"]) == 0
			var prior platform.Row
			e := tx.Table("metric_gpu_idle_windows").Clauses(clause.Locking{Strength: "UPDATE"}).Where("instance_id=? AND gpu_uuid=?", id, uuid).Take(&prior).Error
			if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
				return e
			}
			// Only ingestion advances continuity. Policy polling must not rewind a
			// newer window after a repeatable-read snapshot or a concurrent batch.
			var since any
			if safe && prior["idle_since"] != nil && platform.Text(prior["agent_boot_id"]) == platform.Text(boot) && platform.Stamp(prior["runtime_started_at"]).Equal(runtime) && platform.Uint(prior["last_sequence"]) == sequence {
				since = platform.Stamp(prior["idle_since"])
			}
			duration := time.Duration(0)
			if since != nil {
				t := since.(time.Time)
				duration = at.Sub(t)
			}
			eligible := safe && duration >= time.Duration(policy.Idle.Timeout)*time.Second
			if !eligible {
				allIdle = false
			}
			allocation := byUUID[uuid]
			if eligible && allocation > 0 {
				detach = append(detach, allocation)
			}
			if safe && allocation > 0 && duration >= time.Duration(platform.Uint(scheduling["evaluation_window_seconds"]))*time.Second {
				var high platform.Row
				e = tx.Table("metric_gpu_high_windows").Where("node_id=? AND gpu_uuid=? AND policy_version_id=?", node, uuid, bindings["gpu-scheduling"]).Take(&high).Error
				if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
					return e
				}
				if gpuHighWindowReady(a.Time(), high, time.Duration(maxAge)*time.Second, time.Duration(platform.Uint(scheduling["evaluation_window_seconds"]))*time.Second) {
					rebind = append(rebind, allocation)
				}
			}
		}
		for uuid := range byUUID {
			found := false
			for _, v := range seen {
				if uuid == v {
					found = true
					break
				}
			}
			if !found {
				allIdle = false
			}
		}
		user := platform.Uint(instance["user_id"])
		idleEnabled := platform.Bool(idle["enabled"]) && policy.Idle.Enabled && policy.Idle.Timeout >= platform.Uint(idle["min_timeout_seconds"]) && policy.Idle.Timeout <= platform.Uint(idle["max_timeout_seconds"])
		if idleEnabled && gpuActionAllowed(idle, policy.Idle.Action) {
			if policy.Idle.Action == "stop_instance" && allIdle {
				_, e = instances.QueueStop(ctx, a, tx, id, user, false, "automatic_gpu_idle")
				return e
			}
			if policy.Idle.Action == "detach_gpu" && len(detach) > 0 {
				_, e = instances.QueueGPUDetach(ctx, a, tx, id, user, detach, true, "automatic_gpu_idle")
				return e
			}
		}
		if !policy.Rebalance.Enabled || !policy.Rebalance.Live || len(rebind) == 0 {
			return nil
		}
		// Exhaustion takes precedence over automatic replacement, including grace periods.
		var enforcement int64
		if e = tx.Table("credit_enforcements").Where("user_id=? AND status IN ('grace','stopping','unknown')", user).Count(&enforcement).Error; e != nil {
			return e
		}
		if enforcement > 0 {
			return nil
		}
		cooldown := time.Duration(platform.Uint(scheduling["cooldown_seconds"])) * time.Second
		if last := platform.Stamp(instance["last_rebound_at"]); !last.IsZero() && a.Time().Sub(last) < cooldown {
			return nil
		}
		var recent int64
		if e = tx.Table("metric_rebalance_events").Where("instance_id=? AND created_at>=?", id, a.Time().Add(-time.Duration(platform.Uint(scheduling["rebindings_window_seconds"]))*time.Second)).Count(&recent).Error; e != nil {
			return e
		}
		if uint64(recent) >= platform.Uint(scheduling["max_rebindings_per_window"]) {
			return nil
		}
		var concurrent int64
		if e = tx.Table("metric_rebalance_events e").Joins("JOIN tasks t ON t.id=e.task_id").Where("e.node_id=? AND t.status IN ('queued','running','waiting','unknown')", node).Count(&concurrent).Error; e != nil {
			return e
		}
		if uint64(concurrent) >= platform.Uint(scheduling["max_concurrent_per_node"]) {
			return nil
		}
		rebind = rebind[:1] // Rebind one card per serialized task, preserving heterogeneous peers.
		task, e := instances.QueueGPURebind(ctx, a, tx, id, user, rebind, policy.Rebalance.Max, true, "automatic_gpu_rebalance")
		if e != nil {
			return e
		}
		return tx.Table("metric_rebalance_events").Create(map[string]any{"instance_id": id, "node_id": node, "task_id": platform.Uint(task["task_id"]), "created_at": a.Time(), "updated_at": a.Time()}).Error
	})
}
func gpuActionAllowed(policy platform.Row, action string) bool {
	raw, e := json.Marshal(policy["allowed_actions"])
	if e != nil {
		return false
	}
	var actions []string
	if json.Unmarshal(raw, &actions) != nil {
		return false
	}
	for _, v := range actions {
		if v == action || v == "release_gpu" && action == "detach_gpu" {
			return true
		}
	}
	return false
}

// Each immutable ingested sample advances continuity exactly once, independently
// of the slower policy polling frequency. Re-reading the latest row cannot age it.
func metricObserveGPUIdle(tx *gorm.DB, a *platform.App, id uint64, boot []byte, f metricFrame, i metricInstance, at, start time.Time) error {
	seen := []string{}
	allContinuous := len(i.GPUs) > 0
	var latestSince time.Time
	for _, g := range i.GPUs {
		seen = append(seen, g.UUID)
		var prior platform.Row
		e := tx.Table("metric_gpu_idle_windows").Clauses(clause.Locking{Strength: "UPDATE"}).Where("instance_id=? AND gpu_uuid=?", id, g.UUID).Take(&prior).Error
		if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		safe := f.Complete && i.Quality.AttributionComplete && strings.EqualFold(i.State, "running") && !i.Started.IsZero() && g.Quality.AttributionComplete && metricReference(g.Quality) == "complete" && g.State == "idle_candidate" && g.Processes != nil && *g.Processes == 0 && g.References != nil && *g.References == 0 && g.Memory != nil && *g.Memory == 0 && (g.SM == nil || g.SM.IsZero()) && !at.After(a.Time())
		var since any
		if safe {
			since = at
			if prior["idle_since"] != nil && platform.Text(prior["agent_boot_id"]) == string(boot) && platform.Stamp(prior["runtime_started_at"]).Equal(i.Started.Truncate(time.Microsecond)) && platform.Uint(prior["last_sequence"])+1 == f.Sequence && !start.After(platform.Stamp(prior["last_sampled_at"])) {
				since = platform.Stamp(prior["idle_since"])
			}
		}
		if since == nil {
			allContinuous = false
		} else if t := since.(time.Time); latestSince.IsZero() || t.After(latestSince) {
			latestSince = t
		}
		var runtime any
		if !i.Started.IsZero() {
			runtime = i.Started.In(a.Location).Truncate(time.Microsecond)
		}
		row := map[string]any{"instance_id": id, "gpu_uuid": g.UUID, "agent_boot_id": boot, "runtime_started_at": runtime, "last_sequence": f.Sequence, "last_sampled_at": at, "idle_since": since, "updated_at": a.Time()}
		if e = tx.Table("metric_gpu_idle_windows").Clauses(gpuIdleWindowConflict).Create(row).Error; e != nil {
			return e
		}
	}
	q := tx.Table("metric_gpu_idle_windows").Where("instance_id=?", id)
	if len(seen) > 0 {
		q = q.Where("gpu_uuid NOT IN ?", seen)
	}
	if e := q.Update("idle_since", nil).Error; e != nil {
		return e
	}
	var idleSince any
	if allContinuous && !latestSince.IsZero() {
		idleSince = latestSince
	}
	return tx.Table("instances").Where("id=? AND (observed_at IS NULL OR observed_at<=?)", id, at).Update("idle_since", idleSince).Error
}
