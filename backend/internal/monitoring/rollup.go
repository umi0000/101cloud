package monitoring

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type rollupInterval struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

func rollupUnion(in []rollupInterval) []rollupInterval {
	sort.Slice(in, func(i, j int) bool { return in[i].From.Before(in[j].From) })
	out := make([]rollupInterval, 0, len(in))
	for _, v := range in {
		if !v.To.After(v.From) {
			continue
		}
		n := len(out)
		if n > 0 && !v.From.After(out[n-1].To) {
			if v.To.After(out[n-1].To) {
				out[n-1].To = v.To
			}
		} else {
			out = append(out, v)
		}
	}
	return out
}
func rollupClip(v rollupInterval, from, to time.Time) rollupInterval {
	if v.From.Before(from) {
		v.From = from
	}
	if v.To.After(to) {
		v.To = to
	}
	return v
}
func rollupUncovered(v rollupInterval, used []rollupInterval) []rollupInterval {
	if !v.To.After(v.From) {
		return nil
	}
	out := []rollupInterval{}
	p := v.From
	for _, u := range used {
		if !u.To.After(p) {
			continue
		}
		if !u.From.Before(v.To) {
			break
		}
		if u.From.After(p) {
			out = append(out, rollupInterval{p, u.From})
		}
		if u.To.After(p) {
			p = u.To
		}
		if !p.Before(v.To) {
			return out
		}
	}
	if p.Before(v.To) {
		out = append(out, rollupInterval{p, v.To})
	}
	return out
}
func rollupSeconds(v rollupInterval) decimal.Decimal {
	if !v.To.After(v.From) {
		return decimal.Zero
	}
	return decimal.NewFromInt(v.To.Sub(v.From).Microseconds()).Shift(-6)
}

// Callers pass either an interval union or the disjoint result of rollupUncovered.
func rollupDuration(v []rollupInterval) decimal.Decimal {
	n := decimal.Zero
	for _, x := range v {
		n = n.Add(rollupSeconds(x))
	}
	return n
}
func rollupDecimal(v any) (decimal.Decimal, bool) {
	if v == nil {
		return decimal.Zero, false
	}
	d, e := decimal.NewFromString(platform.Text(v))
	return d, e == nil
}
func rollupFloor(t time.Time, seconds int64) time.Time {
	wall := t.Unix() + 8*3600
	q := wall / seconds
	if wall%seconds < 0 {
		q--
	}
	return time.Unix(q*seconds-8*3600, 0).In(t.Location())
}
func rollupCPUWindow(r platform.Row) rollupInterval {
	end := platform.Stamp(r["cpu_sampled_at"])
	d, ok := rollupDecimal(r["cpu_window_seconds"])
	if !ok || !d.IsPositive() || end.IsZero() {
		return rollupInterval{}
	}
	return rollupInterval{end.Add(-time.Duration(d.Mul(decimal.NewFromInt(1000000)).IntPart()) * time.Microsecond), end}
}

// Differences of rounded prefixes conserve integer counter deltas across buckets.
func rollupCounterPart(total decimal.Decimal, window rollupInterval, parts []rollupInterval, scale int32) decimal.Decimal {
	duration := rollupSeconds(window)
	sum := decimal.Zero
	for _, part := range parts {
		left := rollupSeconds(rollupInterval{window.From, part.From})
		right := rollupSeconds(rollupInterval{window.From, part.To})
		sum = sum.Add(total.Mul(right).DivRound(duration, scale).Sub(total.Mul(left).DivRound(duration, scale)))
	}
	return sum
}

type rollupAccumulator struct {
	row      platform.Row
	used     map[string][]rollupInterval
	coverage []rollupInterval
}

func rollupNew(instance, node uint64, start time.Time, size int64, gpu platform.Row) *rollupAccumulator {
	r := platform.Row{"instance_id": instance, "node_id": node, "bucket_start_at": start, "bucket_seconds": size, "sample_count": uint64(0), "unknown_count": uint64(0), "coverage_seconds": "0"}
	if gpu == nil {
		for _, k := range []string{"cpu_valid_seconds", "cpu_allocation_valid_seconds", "memory_used_valid_seconds", "memory_limit_valid_seconds", "memory_utilization_valid_seconds"} {
			r[k] = "0"
		}
		for _, k := range []string{"cpu_usage_delta_ns", "allocated_cpu_seconds", "cpu_normalized_usage_delta_ns", "cpu_utilization_pct_seconds", "cpu_used_cores_peak", "cpu_utilization_peak_pct", "memory_used_byte_seconds", "memory_limit_byte_seconds", "memory_utilization_pct_seconds", "memory_utilization_peak_pct", "memory_peak_bytes"} {
			r[k] = nil
		}
	} else {
		for _, k := range []string{"gpu_uuid", "gpu_id", "gpu_model", "gpu_memory_total_bytes"} {
			r[k] = gpu[k]
		}
		for _, k := range []string{"utilization_valid_seconds", "memory_valid_seconds"} {
			r[k] = "0"
		}
		r["process_valid_sample_count"] = uint64(0)
		for _, k := range []string{"utilization_pct_seconds", "utilization_peak_pct", "memory_used_byte_seconds", "memory_peak_bytes", "max_processes"} {
			r[k] = nil
		}
	}
	return &rollupAccumulator{r, map[string][]rollupInterval{}, nil}
}
func (a *rollupAccumulator) add(key string, value decimal.Decimal) {
	old, _ := rollupDecimal(a.row[key])
	a.row[key] = old.Add(value).String()
}
func (a *rollupAccumulator) peak(key string, value decimal.Decimal) {
	old, ok := rollupDecimal(a.row[key])
	if !ok || value.GreaterThan(old) {
		a.row[key] = value.String()
	}
}
func (a *rollupAccumulator) weight(metric, valid, sum string, value any, v rollupInterval) decimal.Decimal {
	d, ok := rollupDecimal(value)
	if !ok {
		return decimal.Zero
	}
	parts := rollupUncovered(v, a.used[metric])
	secs := rollupDuration(parts)
	if secs.IsPositive() {
		a.used[metric] = rollupUnion(append(a.used[metric], parts...))
		a.add(valid, secs)
		a.add(sum, d.Mul(secs))
	}
	return secs
}
func (a *rollupAccumulator) observe(r platform.Row, from, to time.Time, gpu bool) {
	a.row["sample_count"] = platform.Uint(a.row["sample_count"]) + 1
	unknown := !platform.Bool(r["attribution_complete"])
	if !gpu {
		unknown = unknown || !platform.Bool(r["collection_complete"]) || !platform.Bool(r["metrics_complete"])
	} else {
		unknown = unknown || !platform.Bool(r["metrics_complete"]) || platform.Text(r["gpu_state"]) == "unknown"
	}
	if unknown {
		a.row["unknown_count"] = platform.Uint(a.row["unknown_count"]) + 1
	}
	frame := rollupClip(rollupInterval{platform.Stamp(r["window_start_at"]), platform.Stamp(r["window_end_at"])}, from, to)
	a.coverage = append(a.coverage, frame)
	if gpu {
		a.weight("util", "utilization_valid_seconds", "utilization_pct_seconds", r["utilization_pct"], frame)
		if v, ok := rollupDecimal(r["utilization_pct"]); ok && rollupSeconds(frame).IsPositive() {
			a.peak("utilization_peak_pct", v)
		}
		a.weight("mem", "memory_valid_seconds", "memory_used_byte_seconds", r["memory_used_bytes"], frame)
		if v, ok := rollupDecimal(r["memory_used_bytes"]); ok && rollupSeconds(frame).IsPositive() {
			a.peak("memory_peak_bytes", v)
		}
		if v, ok := rollupDecimal(r["process_count"]); ok {
			a.row["process_valid_sample_count"] = platform.Uint(a.row["process_valid_sample_count"]) + 1
			a.peak("max_processes", v)
		}
		return
	}
	cpu := rollupCPUWindow(r)
	runtime := platform.Stamp(r["runtime_started_at"])
	// A counter interval straddling a runtime generation is never a valid delta.
	validCPU := !cpu.From.IsZero() && (runtime.IsZero() || !cpu.From.Before(runtime))
	clipped := rollupClip(cpu, from, to)
	if validCPU {
		a.coverage = append(a.coverage, clipped)
	}
	if delta, ok := rollupDecimal(r["cpu_usage_delta_ns"]); ok && validCPU && rollupSeconds(cpu).IsPositive() {
		rate := delta.DivRound(rollupSeconds(cpu), 24)
		cpuParts := rollupUncovered(clipped, a.used["cpu"])
		cpuSeconds := rollupDuration(cpuParts)
		if cpuSeconds.IsPositive() {
			a.used["cpu"] = rollupUnion(append(a.used["cpu"], cpuParts...))
			a.add("cpu_valid_seconds", cpuSeconds)
			a.add("cpu_usage_delta_ns", rollupCounterPart(delta, cpu, cpuParts, 0))
		}
		if rollupSeconds(clipped).IsPositive() {
			a.peak("cpu_used_cores_peak", rate.Shift(-9))
		}
		allocation, known := rollupDecimal(r["allocated_cpu_seconds"])
		pct, pctKnown := rollupDecimal(r["cpu_utilization_pct"])
		if known && allocation.IsPositive() && pctKnown {
			parts := rollupUncovered(clipped, a.used["allocation"])
			secs := rollupDuration(parts)
			if secs.IsPositive() {
				a.used["allocation"] = rollupUnion(append(a.used["allocation"], parts...))
				a.add("cpu_allocation_valid_seconds", secs)
				a.add("allocated_cpu_seconds", rollupCounterPart(allocation, cpu, parts, 9))
				a.add("cpu_normalized_usage_delta_ns", rollupCounterPart(delta, cpu, parts, 0))
				a.add("cpu_utilization_pct_seconds", pct.Mul(secs))
			}
			if rollupSeconds(clipped).IsPositive() {
				a.peak("cpu_utilization_peak_pct", pct)
			}
		}
	}
	a.weight("memory", "memory_used_valid_seconds", "memory_used_byte_seconds", r["memory_used_bytes"], frame)
	if v, ok := rollupDecimal(r["memory_used_bytes"]); ok && rollupSeconds(frame).IsPositive() {
		a.peak("memory_peak_bytes", v)
	}
	a.weight("limit", "memory_limit_valid_seconds", "memory_limit_byte_seconds", r["memory_limit_bytes"], frame)
	used, uok := rollupDecimal(r["memory_used_bytes"])
	limit, lok := rollupDecimal(r["memory_limit_bytes"])
	if uok && lok && limit.IsPositive() {
		pct := used.Mul(decimal.NewFromInt(100)).DivRound(limit, 24)
		a.weight("memory_pct", "memory_utilization_valid_seconds", "memory_utilization_pct_seconds", pct.String(), frame)
		if rollupSeconds(frame).IsPositive() {
			a.peak("memory_utilization_peak_pct", pct)
		}
	}
}
func (a *rollupAccumulator) finish() platform.Row {
	intervals := rollupUnion(a.coverage)
	a.row["coverage_seconds"] = rollupDuration(intervals).String()
	b, _ := json.Marshal(intervals)
	a.row["coverage_intervals"] = string(b)
	return a.row
}

type rollupPolicy struct{ RawDays, RollupDays, Seconds, Interval int64 }

func rollupReadPolicy(ctx context.Context, a *platform.App) (rollupPolicy, error) {
	r, e := a.Policy(ctx, "metrics")
	if e != nil {
		return rollupPolicy{}, e
	}
	p := rollupPolicy{}
	for k, dst := range map[string]*int64{"raw_retention_days": &p.RawDays, "rollup_retention_days": &p.RollupDays, "bucket_seconds": &p.Seconds, "collection_interval_seconds": &p.Interval} {
		n, e := strconv.ParseInt(platform.Text(r[k]), 10, 64)
		if e != nil || n <= 0 || n > 31536000 {
			return p, fmt.Errorf("invalid metrics policy %s", k)
		}
		*dst = n
	}
	return p, nil
}
func RegisterRollups(a *platform.App) {
	a.AddPoller("metrics-rollups", 10*time.Second, rollupWorker)
	a.AddPoller("metrics-retention", time.Minute, rollupRetention)
}
func rollupWorker(ctx context.Context, a *platform.App) error {
	p, e := rollupReadPolicy(ctx, a)
	if e != nil {
		return e
	}
	var cursors []platform.Row
	if e = a.DB.WithContext(ctx).Table("node_metric_cursors").Where("project_scope = ?", "*").Order("node_id").Find(&cursors).Error; e != nil {
		return e
	}
	for _, cursor := range cursors {
		node := platform.Uint(cursor["node_id"])
		var sizes []int64
		if e = a.DB.WithContext(ctx).Table("metric_rollup_progress").Where("node_id = ?", node).Pluck("bucket_seconds", &sizes).Error; e != nil {
			return e
		}
		found := false
		for _, s := range sizes {
			if s == p.Seconds {
				found = true
			}
		}
		if !found {
			sizes = append(sizes, p.Seconds)
		}
		for _, size := range sizes {
			for range 32 {
				advanced := false
				e = a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					var current platform.Row
					if err := tx.Table("node_metric_cursors").Clauses(clause.Locking{Strength: "UPDATE"}).Where("node_id = ? AND project_scope = '*'", node).Take(&current).Error; err != nil {
						return err
					}
					var progress []platform.Row
					if err := tx.Table("metric_rollup_progress").Where("node_id = ? AND bucket_seconds = ?", node, size).Find(&progress).Error; err != nil {
						return err
					}
					var start time.Time
					if len(progress) > 0 {
						start = rollupFloor(platform.Stamp(progress[0]["through_at"]), size)
					} else {
						var first platform.Row
						if err := tx.Table("instance_metric_samples").Select("MIN(LEAST(window_start_at,COALESCE(TIMESTAMPADD(MICROSECOND,-CAST(cpu_window_seconds * 1000000 AS SIGNED),cpu_sampled_at),window_start_at))) AS first_at").Where("node_id = ?", node).Take(&first).Error; err != nil {
							return err
						}
						start = rollupFloor(platform.Stamp(first["first_at"]), size)
						if platform.Stamp(first["first_at"]).IsZero() {
							return nil
						}
					}
					end := start.Add(time.Duration(size) * time.Second)
					safe := platform.Stamp(current["last_sampled_at"]).Add(-time.Duration(p.Interval) * time.Second)
					if end.After(safe) {
						return nil
					}
					var samples []platform.Row
					if err := tx.Table("instance_metric_samples").Where("node_id = ? AND ((window_start_at < ? AND window_end_at > ?) OR (cpu_sampled_at > ? AND TIMESTAMPADD(MICROSECOND,-CAST(cpu_window_seconds * 1000000 AS SIGNED),cpu_sampled_at) < ?) OR (sampled_at >= ? AND sampled_at < ?))", node, end, start, start, end, start, end).Order("sampled_at,id").Find(&samples).Error; err != nil {
						return err
					}
					groups := map[uint64]*rollupAccumulator{}
					gpuGroups := map[string]*rollupAccumulator{}
					ids := make([]uint64, 0, len(samples))
					byID := map[uint64]platform.Row{}
					for _, r := range samples {
						id := platform.Uint(r["instance_id"])
						if groups[id] == nil {
							groups[id] = rollupNew(id, node, start, size, nil)
						}
						groups[id].observe(r, start, end, false)
						sid := platform.Uint(r["id"])
						ids = append(ids, sid)
						byID[sid] = r
					}
					if len(ids) > 0 {
						var gpuRows []platform.Row
						if err := tx.Table("instance_gpu_metric_samples").Where("sample_id IN ?", ids).Order("sampled_at,id").Find(&gpuRows).Error; err != nil {
							return err
						}
						for _, r := range gpuRows {
							parent := byID[platform.Uint(r["sample_id"])]
							r["window_start_at"] = parent["window_start_at"]
							r["window_end_at"] = parent["window_end_at"]
							frame := rollupClip(rollupInterval{platform.Stamp(r["window_start_at"]), platform.Stamp(r["window_end_at"])}, start, end)
							sampled := platform.Stamp(r["sampled_at"])
							if !frame.To.After(frame.From) && (sampled.Before(start) || !sampled.Before(end)) {
								continue
							}
							key := fmt.Sprintf("%d/%s", platform.Uint(r["instance_id"]), platform.Text(r["gpu_uuid"]))
							if gpuGroups[key] == nil {
								gpuGroups[key] = rollupNew(platform.Uint(r["instance_id"]), node, start, size, r)
							}
							gpuGroups[key].observe(r, start, end, true)
						}
					}
					// Rebuilds and checkpoint commit together; child tables have no rollup-parent FK.
					for _, table := range []string{"instance_gpu_metric_rollups", "instance_metric_rollups"} {
						if err := tx.Table(table).Where("node_id = ? AND bucket_start_at = ? AND bucket_seconds = ?", node, start, size).Delete(&platform.Row{}).Error; err != nil {
							return err
						}
					}
					for _, g := range groups {
						if err := tx.Table("instance_metric_rollups").Create(g.finish()).Error; err != nil {
							return err
						}
					}
					for _, g := range gpuGroups {
						if err := tx.Table("instance_gpu_metric_rollups").Create(g.finish()).Error; err != nil {
							return err
						}
					}
					err := tx.Exec("INSERT INTO metric_rollup_progress(node_id,bucket_seconds,through_at,updated_at) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE through_at=VALUES(through_at),updated_at=VALUES(updated_at)", node, size, end, a.Time()).Error
					advanced = err == nil
					return err
				})
				if e != nil {
					return e
				}
				if !advanced {
					break
				}
			}
		}
	}
	return nil
}
func rollupRetention(ctx context.Context, a *platform.App) error {
	p, e := rollupReadPolicy(ctx, a)
	if e != nil {
		return e
	}
	now := a.Time()
	rawBefore := now.AddDate(0, 0, -int(p.RawDays))
	rollBefore := now.AddDate(0, 0, -int(p.RollupDays))
	var cursors []platform.Row
	if e = a.DB.WithContext(ctx).Table("node_metric_cursors").Where("project_scope = '*' ").Find(&cursors).Error; e != nil {
		return e
	}
	for _, r := range cursors {
		node := platform.Uint(r["node_id"])
		if e = a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var locked platform.Row
			if err := tx.Table("node_metric_cursors").Clauses(clause.Locking{Strength: "UPDATE"}).Where("node_id = ? AND project_scope = '*'", node).Take(&locked).Error; err != nil {
				return err
			}
			var progress []platform.Row
			if err := tx.Table("metric_rollup_progress").Where("node_id = ?", node).Find(&progress).Error; err != nil {
				return err
			}
			if len(progress) == 0 {
				return nil
			}
			through := rawBefore
			current := false
			for _, v := range progress {
				size := int64(platform.Uint(v["bucket_seconds"]))
				if size == p.Seconds {
					current = true
				}
				cutoff := rollupFloor(rawBefore, size)
				if cutoff.Before(through) {
					through = cutoff
				}
				t := rollupFloor(platform.Stamp(v["through_at"]), size)
				if t.Before(through) {
					through = t
				}
			}
			if !current {
				return nil
			}
			var ids []uint64
			if err := tx.Table("instance_metric_samples").Where("node_id = ? AND sampled_at < ? AND window_end_at <= ? AND (cpu_sampled_at IS NULL OR cpu_sampled_at <= ?)", node, through, through, through).Order("sampled_at,id").Limit(1000).Pluck("id", &ids).Error; err != nil {
				return err
			}
			if len(ids) > 0 {
				if err := tx.Table("instance_gpu_metric_samples").Where("sample_id IN ?", ids).Delete(&platform.Row{}).Error; err != nil {
					return err
				}
				if err := tx.Table("instance_metric_samples").Where("id IN ?", ids).Delete(&platform.Row{}).Error; err != nil {
					return err
				}
			}
			return nil
		}); e != nil {
			return e
		}
	}
	for _, table := range []string{"instance_gpu_metric_rollups", "instance_metric_rollups"} {
		if e = a.DB.WithContext(ctx).Exec("DELETE FROM "+table+" WHERE TIMESTAMPADD(SECOND,bucket_seconds,bucket_start_at) < ? ORDER BY bucket_start_at,id LIMIT 1000", rollBefore).Error; e != nil {
			return e
		}
	}
	return nil
}
