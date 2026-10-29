package node

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"101cloud/hostagent/internal/monitor"
)

const metricsHistoryCapacity = 120

type metricsFrame struct {
	monitor.Snapshot
	SchemaVersion             int       `json:"schema_version"`
	AgentBootID               string    `json:"agent_boot_id"`
	SampleSequence            uint64    `json:"sample_sequence"`
	WindowStart               time.Time `json:"window_start_at"`
	WindowEnd                 time.Time `json:"window_end_at"`
	WindowSeconds             float64   `json:"window_seconds"`
	CollectionComplete        bool      `json:"collection_complete"`
	CollectionErrors          []string  `json:"collection_errors"`
	CollectionDurationSeconds float64   `json:"collection_duration_seconds"`
}

type metricsSampler struct {
	mu                  sync.RWMutex
	bootID              string
	interval, timeLimit time.Duration
	history             []metricsFrame
	next                int
	count               int
	sequence            uint64
	engine              *monitor.Engine
	input               func(context.Context) metricsInput
}

// StartMetrics creates one bounded sampling loop. Snapshots are immutable and
// retained in memory only; neither private process data nor metrics touch disk.
func (s *Service) StartMetrics(ctx context.Context, interval, timeLimit time.Duration) error {
	if interval < time.Second || interval > 60*time.Second || timeLimit < time.Second || timeLimit > 30*time.Second {
		return fmt.Errorf("invalid metrics sampling durations")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return fmt.Errorf("metrics identity unavailable")
	}
	id[6] = (id[6] & 15) | 64
	id[8] = (id[8] & 63) | 128
	m := &metricsSampler{bootID: fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), interval: interval, timeLimit: timeLimit, history: make([]metricsFrame, metricsHistoryCapacity), engine: monitor.NewEngine(monitor.Options{}), input: s.collectMetricsInput}
	s.metricsMu.Lock()
	if s.metrics != nil {
		s.metricsMu.Unlock()
		m.engine.Close()
		return fmt.Errorf("metrics sampler already started")
	}
	s.metrics = m
	s.metricsMu.Unlock()
	go m.run(ctx)
	return nil
}

func (m *metricsSampler) run(ctx context.Context) {
	defer m.engine.Close()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	var previous time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		sampleCtx, cancel := context.WithTimeout(ctx, m.timeLimit)
		input := m.input(sampleCtx)
		m.engine.SetInventory(input.Inventory)
		m.engine.SetCollectionComplete(input.Complete)
		snapshot := m.engine.Sample(sampleCtx, input.Instances)
		cancel()
		if ctx.Err() != nil {
			return
		}
		end := snapshot.CollectedAt
		if end.IsZero() {
			end = time.Now()
		}
		end = end.UTC().Truncate(time.Microsecond)
		snapshot.CollectedAt = end
		begin := previous
		complete := input.Complete
		if begin.IsZero() {
			begin = end
		}
		if end.Before(begin) {
			begin = end
			complete = false
			input.Errors = append(input.Errors, "clock_discontinuity")
		}
		seconds := float64(end.Sub(begin).Microseconds()) / 1e6
		m.mu.Lock()
		m.sequence++
		frame := metricsFrame{Snapshot: snapshot, SchemaVersion: 1, AgentBootID: m.bootID, SampleSequence: m.sequence, WindowStart: begin, WindowEnd: end, WindowSeconds: seconds, CollectionComplete: complete, CollectionErrors: input.Errors, CollectionDurationSeconds: time.Since(started).Seconds()}
		m.history[m.next] = frame
		m.next = (m.next + 1) % len(m.history)
		if m.count < len(m.history) {
			m.count++
		}
		m.mu.Unlock()
		previous = end
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func filterMetricsFrame(frame metricsFrame, project string) metricsFrame {
	if project == "*" {
		return frame
	}
	instances := frame.Instances
	frame.Instances = make([]monitor.InstanceMetrics, 0)
	for _, instance := range instances {
		if instance.Project == project {
			frame.Instances = append(frame.Instances, instance)
		}
	}
	return frame
}

func (s *Service) metricsAPI(w http.ResponseWriter, r *http.Request, history bool) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	query, err := parseMetricsQuery(r, history)
	if err != nil {
		writeError(w, 400, "invalid_metrics_query", err.Error())
		return
	}
	s.metricsMu.RLock()
	m := s.metrics
	s.metricsMu.RUnlock()
	if m == nil {
		writeError(w, 503, "metrics_warming_up", "metrics sampler has not started")
		return
	}
	m.mu.RLock()
	if m.count == 0 {
		m.mu.RUnlock()
		writeError(w, 503, "metrics_warming_up", "first metrics sample is not ready")
		return
	}
	newest := m.history[(m.next-1+len(m.history))%len(m.history)]
	oldestIndex := (m.next - m.count + len(m.history)) % len(m.history)
	oldest := m.history[oldestIndex].SampleSequence
	sequence := m.sequence
	frames := make([]metricsFrame, 0)
	if history {
		frames = make([]metricsFrame, m.count)
		for i := range frames {
			frames[i] = m.history[(oldestIndex+i)%len(m.history)]
		}
	}
	m.mu.RUnlock()
	stale := time.Since(newest.CollectedAt) > 2*m.interval+m.timeLimit
	if !history {
		frame := filterMetricsFrame(newest, query.project)
		status := http.StatusOK
		if stale || !frame.CollectionComplete {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, struct {
			metricsFrame
			Stale bool `json:"stale"`
		}{frame, stale})
		return
	}
	if query.bootID != "" && query.bootID != m.bootID {
		writeJSON(w, 409, map[string]any{"error": "metrics_agent_restarted", "agent_boot_id": m.bootID, "oldest_available_sequence": oldest, "newest_sequence": sequence})
		return
	}
	if query.after > sequence {
		writeError(w, 400, "invalid_metrics_cursor", "cursor is ahead of current sampler sequence")
		return
	}
	gap := query.after < oldest-1
	samples := make([]metricsFrame, 0, query.limit)
	next := query.after
	for offset := 0; offset < len(frames) && len(samples) < query.limit; offset++ {
		frame := frames[offset]
		if frame.SampleSequence <= query.after {
			continue
		}
		samples = append(samples, filterMetricsFrame(frame, query.project))
		next = frame.SampleSequence
	}
	status := http.StatusOK
	if stale {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, struct {
		SchemaVersion   int            `json:"schema_version"`
		AgentBootID     string         `json:"agent_boot_id"`
		OldestAvailable uint64         `json:"oldest_available_sequence"`
		Newest          uint64         `json:"newest_sequence"`
		Next            uint64         `json:"next_sequence"`
		Gap             bool           `json:"gap"`
		Stale           bool           `json:"stale"`
		Samples         []metricsFrame `json:"samples"`
	}{1, m.bootID, oldest, sequence, next, gap, stale, samples})
}

type metricsQuery struct {
	project, bootID string
	after           uint64
	limit           int
}

func parseMetricsQuery(r *http.Request, history bool) (metricsQuery, error) {
	q := metricsQuery{project: "default", limit: 32}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return q, fmt.Errorf("invalid query encoding")
	}
	for name, list := range values {
		if len(list) != 1 || list[0] == "" {
			return q, fmt.Errorf("metrics query fields must be single non-empty values")
		}
		value := list[0]
		switch name {
		case "project":
			if strings.ContainsAny(value, "/\\\x00") || strings.TrimSpace(value) != value || len(value) > 63 {
				return q, fmt.Errorf("invalid project")
			}
			q.project = value
		case "agent_boot_id":
			if !history || !validMetricsBootID(value) {
				return q, fmt.Errorf("invalid agent_boot_id")
			}
			q.bootID = value
		case "after_sequence":
			if !history {
				return q, fmt.Errorf("cursor only accepted by samples endpoint")
			}
			v, e := strconv.ParseUint(value, 10, 64)
			if e != nil {
				return q, fmt.Errorf("invalid after_sequence")
			}
			q.after = v
		case "limit":
			if !history {
				return q, fmt.Errorf("limit only accepted by samples endpoint")
			}
			v, e := strconv.Atoi(value)
			if e != nil || v < 1 || v > metricsHistoryCapacity {
				return q, fmt.Errorf("limit must be 1..120")
			}
			q.limit = v
		default:
			return q, fmt.Errorf("unknown metrics query parameter")
		}
	}
	if history && q.after > 0 && q.bootID == "" {
		return q, fmt.Errorf("agent_boot_id is required with a nonzero cursor")
	}
	return q, nil
}
func validMetricsBootID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
