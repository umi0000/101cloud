package monitoring

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Register installs read APIs and durable workers. Collection never runs in a request.
func Register(a *platform.App, user, admin *gin.RouterGroup) {
	RegisterHistory(a, user, admin)
	RegisterConsole(a, user, admin)
	RegisterRollups(a)
	a.AddPoller("metrics-collection", time.Second, collectMetrics)
	a.AddPoller("gpu-idle-and-rebalance", time.Second, evaluateGPU)
}

type metricQuality struct {
	Complete            bool     `json:"complete"`
	AttributionComplete bool     `json:"attribution_complete"`
	Reasons             []string `json:"reasons"`
}
type metricCPU struct {
	CollectedAt time.Time        `json:"collected_at"`
	Usage       *uint64          `json:"usage_ns"`
	Allocated   *uint64          `json:"allocated_ns_per_second"`
	Delta       *uint64          `json:"delta_ns"`
	Window      *decimal.Decimal `json:"window_seconds"`
	Percent     *decimal.Decimal `json:"allocation_percent"`
}
type metricMemory struct {
	Used  *uint64 `json:"usage_bytes"`
	Limit *uint64 `json:"limit_bytes"`
	Peak  *uint64 `json:"peak_bytes"`
}
type metricGPU struct {
	UUID       string           `json:"uuid"`
	Configured bool             `json:"configured"`
	Observed   bool             `json:"observed"`
	Processes  *uint32          `json:"process_count"`
	References *uint32          `json:"referencing_process_count"`
	Memory     *uint64          `json:"used_memory_bytes"`
	SM         *decimal.Decimal `json:"sm_utilization_percent"`
	State      string           `json:"state"`
	Quality    metricQuality    `json:"quality"`
}
type metricInstance struct {
	Project  string        `json:"project"`
	Name     string        `json:"name"`
	UUID     string        `json:"uuid"`
	State    string        `json:"state"`
	Started  time.Time     `json:"started_at"`
	CPU      metricCPU     `json:"cpu"`
	Memory   metricMemory  `json:"memory"`
	GPUs     []metricGPU   `json:"gpus"`
	GPUState string        `json:"gpu_state"`
	Quality  metricQuality `json:"quality"`
}
type metricFrame struct {
	Schema    int              `json:"schema_version"`
	Boot      string           `json:"agent_boot_id"`
	Sequence  uint64           `json:"sample_sequence"`
	At        time.Time        `json:"collected_at"`
	Start     time.Time        `json:"window_start_at"`
	End       time.Time        `json:"window_end_at"`
	Complete  bool             `json:"collection_complete"`
	Instances []metricInstance `json:"instances"`
}
type metricBatch struct {
	Schema  int           `json:"schema_version"`
	Boot    string        `json:"agent_boot_id"`
	Oldest  uint64        `json:"oldest_available_sequence"`
	Newest  uint64        `json:"newest_sequence"`
	Next    uint64        `json:"next_sequence"`
	Gap     bool          `json:"gap"`
	Stale   bool          `json:"stale"`
	Samples []metricFrame `json:"samples"`
}
type metricCheckpoint struct {
	NodeID         uint64
	ProjectScope   string
	AgentBootID    []byte
	LastSequence   uint64
	LastSampledAt  *time.Time
	LastIngestedAt time.Time
}

var collectionState = struct {
	sync.Mutex
	next map[gpuPollKey]time.Time
}{next: map[gpuPollKey]time.Time{}}

func collectMetrics(ctx context.Context, a *platform.App) error {
	var nodes []struct {
		ID              uint64
		MetricsPolicyID string
	}
	if err := a.DB.WithContext(ctx).Table("nodes").Select("id,metrics_policy_id").Where("EXISTS (SELECT 1 FROM instances i WHERE i.node_id=nodes.id AND i.deleted_at IS NULL)").Find(&nodes).Error; err != nil {
		return err
	}
	groups := map[string][]uint64{}
	for _, node := range nodes {
		groups[node.MetricsPolicyID] = append(groups[node.MetricsPolicyID], node.ID)
	}
	var failures []error
	for id, group := range groups {
		p, err := a.Policy(ctx, "metrics", id)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		interval := platform.Uint(p["collection_interval_seconds"])
		timeout := platform.Uint(p["collection_timeout_seconds"])
		concurrency := platform.Uint(p["max_concurrent_nodes"])
		if interval == 0 || timeout == 0 || timeout > 120 || concurrency == 0 || concurrency > 32 {
			failures = append(failures, platform.E(422, "invalid_policy", "Explicit metrics worker bounds are required"))
			continue
		}
		jobs := make(chan uint64)
		var wg sync.WaitGroup
		var mu sync.Mutex
		for range int(concurrency) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for node := range jobs {
					call, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
					err := collectNode(call, a, node)
					cancel()
					if err != nil {
						mu.Lock()
						failures = append(failures, err)
						mu.Unlock()
					}
				}
			}()
		}
		for _, node := range group {
			key := gpuPollKey{Node: node, Policy: id}
			collectionState.Lock()
			due := !a.Time().Before(collectionState.next[key])
			if due {
				collectionState.next[key] = a.Time().Add(time.Duration(interval) * time.Second)
			}
			collectionState.Unlock()
			if !due {
				continue
			}
			select {
			case jobs <- node:
			case <-ctx.Done():
				close(jobs)
				wg.Wait()
				return ctx.Err()
			}
		}
		close(jobs)
		wg.Wait()
	}
	return errors.Join(failures...)
}

func bootBytes(s string) ([]byte, error) {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return nil, fmt.Errorf("invalid agent boot identity")
	}
	v, e := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if e != nil || len(v) != 16 {
		return nil, fmt.Errorf("invalid agent boot identity")
	}
	return v, nil
}
func bootString(b []byte) string {
	if len(b) != 16 {
		return ""
	}
	s := hex.EncodeToString(b)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func fetchMetricBatch(ctx context.Context, a *platform.App, node uint64, cp metricCheckpoint) (metricBatch, int, error) {
	var out metricBatch
	client, origin, e := a.AgentClient(ctx, node)
	if e != nil {
		return out, 0, e
	}
	q := url.Values{"project": {"*"}, "limit": {"120"}, "after_sequence": {fmt.Sprint(cp.LastSequence)}}
	if len(cp.AgentBootID) == 16 {
		q.Set("agent_boot_id", bootString(cp.AgentBootID))
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/agent/v1/instances/metrics/samples?"+q.Encode(), nil)
	if e != nil {
		return out, 0, e
	}
	res, e := client.Do(req)
	if e != nil {
		return out, 0, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 && res.StatusCode != 503 {
		return out, res.StatusCode, nil
	}
	d := json.NewDecoder(io.LimitReader(res.Body, 16<<20))
	if e = d.Decode(&out); e != nil {
		return out, res.StatusCode, e
	}
	var extra any
	if e = d.Decode(&extra); e != io.EOF {
		return out, res.StatusCode, fmt.Errorf("invalid metrics response framing")
	}
	return out, res.StatusCode, nil
}
func collectNode(ctx context.Context, a *platform.App, node uint64) error {
	var cp metricCheckpoint
	err := a.DB.WithContext(ctx).Table("node_metric_cursors").Where("node_id=? AND project_scope='*'", node).Take(&cp).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	restarted := false
	b, status, err := fetchMetricBatch(ctx, a, node, cp)
	if err != nil {
		return err
	}
	if status == 409 {
		restarted = true
		b, status, err = fetchMetricBatch(ctx, a, node, metricCheckpoint{})
		if err != nil {
			return err
		}
	}
	if status != 200 && status != 503 {
		return fmt.Errorf("metrics node %d upstream status %d", node, status)
	}
	// A warming-up 503 has no schema; a stale envelope still contains immutable history.
	if b.Schema != 1 {
		return platform.E(503, "metrics_unavailable", "Agent metric schema unavailable")
	}
	boot, err := bootBytes(b.Boot)
	if err != nil {
		return err
	}
	if b.Next > b.Newest || len(b.Samples) > 120 {
		return fmt.Errorf("invalid metric batch cursor")
	}
	policies, err := nodeMetricPolicies(ctx, a, node)
	if err != nil {
		return err
	}
	days := policies.Node.RawDays
	for _, policy := range policies.Instances {
		days = max(days, policy.RawDays)
	}
	cutoff := a.Time().AddDate(0, 0, -int(days))
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		seed := map[string]any{"node_id": node, "project_scope": "*", "agent_boot_id": boot, "last_sequence": 0, "last_ingested_at": a.Time()}
		if e := tx.Table("node_metric_cursors").Clauses(clause.OnConflict{DoUpdates: clause.AssignmentColumns([]string{"node_id"})}).Create(seed).Error; e != nil {
			return e
		}
		var locked metricCheckpoint
		if e := tx.Table("node_metric_cursors").Clauses(clause.Locking{Strength: "UPDATE"}).Where("node_id=? AND project_scope='*'", node).Take(&locked).Error; e != nil {
			return e
		}
		var sizes []int64
		if e := tx.Table("metric_rollup_progress").Where("node_id=?", node).Pluck("bucket_seconds", &sizes).Error; e != nil {
			return e
		}
		sizes = append(sizes, policies.Node.Seconds)
		for _, policy := range policies.Instances {
			sizes = append(sizes, policy.Seconds)
		}
		for _, size := range sizes {
			if size <= 0 {
				return fmt.Errorf("invalid metrics bucket size")
			}
			floor := rollupFloor(cutoff, size)
			if floor.Before(cutoff) {
				cutoff = floor
			}
		}
		// A concurrent collector already advanced this stream: do not overwrite its checkpoint.
		if bootString(locked.AgentBootID) != bootString(cp.AgentBootID) && locked.LastSequence > 0 && bootString(locked.AgentBootID) != b.Boot {
			return nil
		}
		changed := bootString(locked.AgentBootID) != b.Boot
		if !changed && b.Next < locked.LastSequence {
			return nil
		}
		var earliest, last time.Time
		lastSeq := uint64(0)
		for _, f := range b.Samples {
			if f.Schema != 1 || f.Boot != b.Boot || f.Sequence > b.Next || f.Sequence <= lastSeq || f.At.IsZero() || f.Start.After(f.End) || f.End.After(f.At) {
				return fmt.Errorf("invalid metrics frame")
			}
			lastSeq = f.Sequence
			if !changed && f.Sequence <= locked.LastSequence {
				continue
			}
			at := f.At.In(a.Location).Truncate(time.Microsecond)
			start := f.Start.In(a.Location).Truncate(time.Microsecond)
			end := f.End.In(a.Location).Truncate(time.Microsecond)
			if at.After(last) {
				last = at
			}
			frameEarliest := start
			for _, i := range f.Instances {
				cpuStart, _, _, e := metricCPUInterval(i.CPU, a.Location)
				if e != nil {
					return e
				}
				if !cpuStart.IsZero() {
					if cpuStart.Before(frameEarliest) {
						frameEarliest = cpuStart
					}
				}
			}
			if frameEarliest.Before(cutoff) {
				if e := tx.Table("metric_gaps").Create(map[string]any{"node_id": node, "project_scope": "*", "from_at": frameEarliest.In(a.Location), "to_at": at, "reason": "late_sample", "created_at": a.Time(), "updated_at": a.Time()}).Error; e != nil {
					return e
				}
				continue
			}
			if earliest.IsZero() || frameEarliest.Before(earliest) {
				earliest = frameEarliest
			}
			if e := ingestFrame(tx, a, node, boot, f, at, start, end, &earliest); e != nil {
				return e
			}
		}
		if len(b.Samples) > 0 && lastSeq != b.Next {
			return fmt.Errorf("metric batch checkpoint does not match last sample")
		}
		if (changed || restarted || b.Gap) && !earliest.IsZero() {
			from := earliest
			if locked.LastSampledAt != nil && locked.LastSampledAt.Before(earliest) {
				from = *locked.LastSampledAt
			}
			reason := "history_evicted"
			if changed || restarted {
				reason = "agent_restarted"
			}
			if e := tx.Table("metric_gaps").Create(map[string]any{"node_id": node, "project_scope": "*", "from_at": from, "to_at": earliest, "reason": reason, "created_at": a.Time(), "updated_at": a.Time()}).Error; e != nil {
				return e
			}
		}
		if !earliest.IsZero() {
			if e := tx.Exec("UPDATE metric_rollup_progress SET through_at=LEAST(through_at,?),updated_at=? WHERE node_id=?", earliest, a.Time(), node).Error; e != nil {
				return e
			}
		}
		updates := map[string]any{"agent_boot_id": boot, "last_sequence": b.Next, "last_ingested_at": a.Time()}
		if !last.IsZero() {
			updates["last_sampled_at"] = last
		}
		return tx.Table("node_metric_cursors").Where("node_id=? AND project_scope='*'", node).Updates(updates).Error
	})
}

func ingestFrame(tx *gorm.DB, a *platform.App, node uint64, boot []byte, f metricFrame, at, start, end time.Time, earliest *time.Time) error {
	var inventory []platform.Row
	if e := tx.Table("node_gpus").Select("id,uuid,model,memory_mib").Where("node_id=?", node).Find(&inventory).Error; e != nil {
		return e
	}
	cards := map[string]platform.Row{}
	for _, r := range inventory {
		cards[platform.Text(r["uuid"])] = r
	}
	for _, i := range f.Instances {
		if _, e := bootBytes(i.UUID); e != nil {
			continue
		}
		var owner platform.Row
		e := tx.Table("instances").Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("node_id=? AND incus_uuid=? AND project_name=? AND deleted_at IS NULL", node, i.UUID, i.Project).Take(&owner).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			continue
		}
		if e != nil {
			return e
		}
		id := platform.Uint(owner["id"])
		state := metricEnum(i.State, "unknown", "pending", "running", "stopped", "frozen", "starting", "stopping", "restarting", "error", "unknown")
		row := map[string]any{"instance_id": id, "node_id": node, "agent_boot_id": boot, "sample_sequence": f.Sequence, "instance_uuid": i.UUID, "instance_state": state, "sampled_at": at, "window_start_at": start, "window_end_at": end, "window_seconds": decimal.NewFromInt(end.Sub(start).Microseconds()).Div(decimal.NewFromInt(1000000)).StringFixed(6), "gpu_state": metricEnum(i.GPUState, "unknown", "in_use", "idle_candidate", "unknown", "not_attached"), "collection_complete": f.Complete, "metrics_complete": i.Quality.Complete, "attribution_complete": i.Quality.AttributionComplete, "reference_scan_quality": metricReference(i.Quality), "quality_reasons": metricReasons(i.Quality), "memory_used_bytes": i.Memory.Used, "memory_limit_bytes": i.Memory.Limit, "memory_peak_bytes": i.Memory.Peak, "cpu_usage_total_ns": i.CPU.Usage}
		if !i.Started.IsZero() && !i.Started.After(at) {
			row["runtime_started_at"] = i.Started.In(a.Location).Truncate(time.Microsecond)
		}
		if i.CPU.Allocated != nil && *i.CPU.Allocated > 0 {
			row["allocated_vcpus"] = decimal.NewFromBigInt(newBigUint(*i.CPU.Allocated), -9).StringFixed(6)
		}
		cpuStart, cpuAt, cpuWindow, e := metricCPUInterval(i.CPU, a.Location)
		if e != nil {
			return e
		}
		if !cpuAt.IsZero() && !cpuAt.After(at) {
			row["cpu_sampled_at"] = cpuAt
			row["cpu_window_seconds"] = cpuWindow.StringFixed(6)
			if cpuStart.Before(*earliest) {
				*earliest = cpuStart
			}
			if i.CPU.Delta != nil && i.CPU.Usage != nil && *i.CPU.Delta <= *i.CPU.Usage && !i.Started.IsZero() && !cpuStart.Before(i.Started) {
				row["cpu_usage_delta_ns"] = *i.CPU.Delta
				if i.CPU.Percent != nil && i.CPU.Percent.Sign() >= 0 && i.CPU.Allocated != nil && *i.CPU.Allocated > 0 {
					row["cpu_utilization_pct"] = i.CPU.Percent.StringFixed(6)
					row["allocated_cpu_seconds"] = decimal.NewFromBigInt(newBigUint(*i.CPU.Allocated), -9).Mul(cpuWindow).StringFixed(9)
				}
			}
		}
		result := tx.Table("instance_metric_samples").Clauses(clause.OnConflict{DoUpdates: clause.AssignmentColumns([]string{"instance_id"})}).Create(row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		var sample struct{ ID uint64 }
		if e := tx.Table("instance_metric_samples").Select("id").Where("node_id=? AND agent_boot_id=? AND sample_sequence=? AND instance_id=?", node, boot, f.Sequence, id).Take(&sample).Error; e != nil {
			return e
		}
		if f.Complete && !at.After(a.Time()) && (state == "running" || state == "stopped" || state == "frozen" || state == "error" || state == "pending") {
			if e := tx.Table("instances").Where("id=? AND (observed_at IS NULL OR observed_at<?)", id, at).Updates(map[string]any{"observed_state": state, "observed_at": at}).Error; e != nil {
				return e
			}
		}
		seen := map[string]bool{}
		for _, g := range i.GPUs {
			if len(g.UUID) == 0 || len(g.UUID) > 128 || seen[g.UUID] {
				return fmt.Errorf("invalid GPU sample identity")
			}
			seen[g.UUID] = true
			if g.SM != nil && (g.SM.Sign() < 0 || g.SM.GreaterThan(decimal.NewFromInt(100))) {
				return fmt.Errorf("invalid GPU utilization")
			}
			gr := map[string]any{"sample_id": sample.ID, "instance_id": id, "node_id": node, "sampled_at": at, "gpu_uuid": g.UUID, "is_configured": g.Configured, "is_observed": g.Observed, "gpu_state": metricEnum(g.State, "unknown", "in_use", "idle_candidate", "unknown", "not_attached"), "memory_used_bytes": g.Memory, "process_count": g.Processes, "reference_count": g.References, "attribution_complete": g.Quality.AttributionComplete, "metrics_complete": g.Quality.Complete, "reference_scan_quality": metricReference(g.Quality), "quality_reasons": metricReasons(g.Quality), "compute_process_support": "unknown", "graphics_process_support": "unknown", "utilization_support": "unknown", "memory_support": "unknown"}
			if g.SM != nil {
				gr["utilization_pct"] = g.SM.StringFixed(6)
			}
			if card, ok := cards[g.UUID]; ok {
				gr["gpu_id"] = platform.Uint(card["id"])
				gr["gpu_model"] = platform.Text(card["model"])
				gr["gpu_memory_total_bytes"] = decimal.NewFromBigInt(newBigUint(platform.Uint(card["memory_mib"])), 0).Mul(decimal.NewFromInt(1048576)).StringFixed(0)
			}
			if e := tx.Table("instance_gpu_metric_samples").Create(gr).Error; e != nil {
				return e
			}
		}
		if e := metricObserveGPUIdle(tx, a, id, boot, f, i, at, start); e != nil {
			return e
		}
		if e := metricObserveAllocations(tx, a, id, node, f, i, at); e != nil {
			return e
		}
	}
	return nil
}

// metricCPUInterval preserves the CPU counter's independent observation window.
// Agent windows have nanosecond precision; quantize both endpoints, not the
// duration alone, so retention, rollups and stored DATETIME(6) values agree.
func metricCPUInterval(cpu metricCPU, location *time.Location) (start, end time.Time, window decimal.Decimal, err error) {
	if cpu.CollectedAt.IsZero() || cpu.Window == nil || !cpu.Window.IsPositive() {
		return
	}
	ns := cpu.Window.Mul(decimal.NewFromInt(int64(time.Second)))
	if ns.GreaterThan(decimal.NewFromInt(int64(24 * time.Hour))) {
		err = fmt.Errorf("CPU metric window exceeds collection bound")
		return
	}
	duration := time.Duration(ns.Round(0).IntPart())
	start = cpu.CollectedAt.Add(-duration).In(location).Truncate(time.Microsecond)
	end = cpu.CollectedAt.In(location).Truncate(time.Microsecond)
	if !start.Before(end) {
		// A sub-microsecond interval has no representable duration; leave it NULL.
		return time.Time{}, time.Time{}, decimal.Zero, nil
	}
	window = decimal.NewFromInt(end.Sub(start).Microseconds()).Div(decimal.NewFromInt(1000000))
	return
}

func metricEnum(s, fallback string, allowed ...string) string {
	for _, v := range allowed {
		if strings.EqualFold(s, v) {
			return v
		}
	}
	return fallback
}
func metricReference(q metricQuality) string {
	for _, r := range q.Reasons {
		if r == "gpu_reference_scan_incomplete" || r == "gpu_references_unresolved" {
			return "partial"
		}
	}
	if q.AttributionComplete {
		return "complete"
	}
	return "unknown"
}
func metricReasons(q metricQuality) string {
	out := []string{}
	seen := map[string]bool{}
	for _, r := range q.Reasons {
		switch r {
		case "process_scan_incomplete", "instance_inventory_incomplete", "device_inventory_incomplete", "gpu_collection_failed", "gpu_sample_stale", "collection_cancelled", "instance_identity_changed", "gpu_utilization_stale", "gpu_identity_unknown", "gpu_identity_ambiguous", "gpu_attribution_incomplete", "gpu_metric_unavailable", "gpu_utilization_unknown", "gpu_inventory_unavailable", "unattributed_gpu_contexts", "unattributed_gpu_activity", "gpu_reference_scan_incomplete", "gpu_references_unresolved", "instance_attribution_unknown", "gpu_configuration_unresolved", "configured_gpu_unresolved", "gpu_utilization_identity_unconfirmed":
			if !seen[r] {
				out = append(out, r)
				seen[r] = true
			}
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}
func newBigUint(v uint64) *big.Int { return new(big.Int).SetUint64(v) }

// Synchronization records observations only. Releasing ownership and charging
// intervals remains the serialized allocation worker's responsibility.
func metricObserveAllocations(tx *gorm.DB, a *platform.App, instanceID, nodeID uint64, f metricFrame, i metricInstance, at time.Time) error {
	if !f.Complete || !i.Quality.AttributionComplete || at.After(a.Time()) {
		return nil
	}
	var held []struct {
		ID   uint64
		UUID string
	}
	if e := tx.Table("gpu_allocations ga").Select("ga.id,ng.uuid").Joins("JOIN node_gpus ng ON ng.id=ga.gpu_id AND ng.node_id=ga.node_id").Where("ga.instance_id=? AND ga.node_id=? AND ga.active_slot=1 AND ga.reserved_at<=? AND (ga.last_verified_at IS NULL OR ga.last_verified_at<?)", instanceID, nodeID, at, at).Find(&held).Error; e != nil {
		return e
	}
	observed := make(map[string]metricGPU, len(i.GPUs))
	for _, g := range i.GPUs {
		observed[g.UUID] = g
	}
	for _, allocation := range held {
		configured, used := false, false
		if g, present := observed[allocation.UUID]; present {
			if !g.Quality.AttributionComplete {
				continue
			}
			configured, used = g.Configured, g.Observed
		}
		if e := tx.Table("gpu_allocations").Where("id=? AND instance_id=? AND node_id=? AND active_slot=1 AND reserved_at<=? AND (last_verified_at IS NULL OR last_verified_at<?)", allocation.ID, instanceID, nodeID, at, at).Updates(map[string]any{"configured": configured, "observed": used, "last_verified_at": at, "revision": gorm.Expr("revision+1")}).Error; e != nil {
			return e
		}
	}
	return nil
}
