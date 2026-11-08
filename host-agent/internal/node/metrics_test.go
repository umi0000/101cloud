package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"101cloud/hostagent/internal/monitor"
	"101cloud/hostagent/internal/monitor/gpu"
)

const metricsTestBoot = "11111111-1111-4111-8111-111111111111"

func metricsTestService(t *testing.T) *Service {
	t.Helper()
	s, err := New("/nonexistent/incus.sock")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	frames := make([]metricsFrame, 3)
	for i := range frames {
		frames[i] = metricsFrame{Snapshot: monitor.Snapshot{CollectedAt: now, Instances: []monitor.InstanceMetrics{{Project: "default", Name: "first", UUID: "instance-a"}, {Project: "tenant", Name: "second", UUID: "instance-b"}}, Quality: monitor.Quality{Reasons: []string{"instance_attribution_unknown"}}, HostQuality: monitor.Quality{Complete: true, AttributionComplete: true, ContextAttributionComplete: true, Reasons: []string{}}}, AgentBootID: metricsTestBoot, SchemaVersion: 1, SampleSequence: uint64(i + 5), WindowStart: now.Add(-time.Second), WindowEnd: now, WindowSeconds: 1, CollectionComplete: true, CollectionErrors: []string{}, CollectionStartedAt: now.Add(-100 * time.Millisecond).Truncate(time.Microsecond)}
	}
	s.metrics = &metricsSampler{bootID: metricsTestBoot, interval: time.Second, timeLimit: time.Second, history: frames, count: 3, next: 0, sequence: 7}
	return s
}

func TestMetricsProjectAndCursorBoundaries(t *testing.T) {
	s := metricsTestService(t)
	call := func(path string) (int, map[string]json.RawMessage) {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		var result map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return w.Code, result
	}
	status, payload := call("/agent/v1/instances/metrics?project=tenant")
	var instances []monitor.InstanceMetrics
	if err := json.Unmarshal(payload["instances"], &instances); err != nil {
		t.Fatal(err)
	}
	if status != 200 || len(instances) != 1 || instances[0].Project != "tenant" {
		t.Fatalf("project filter leaked instances: %d %+v", status, instances)
	}
	var hostQuality monitor.Quality
	if err := json.Unmarshal(payload["host_quality"], &hostQuality); err != nil {
		t.Fatal(err)
	}
	if !hostQuality.AttributionComplete || !hostQuality.ContextAttributionComplete || !hostQuality.Complete {
		t.Fatalf("project filtering replaced independent host quality: %s", payload["host_quality"])
	}
	status, payload = call("/agent/v1/instances/metrics/samples?project=*&agent_boot_id=" + metricsTestBoot + "&after_sequence=4&limit=2")
	var samples []metricsFrame
	_ = json.Unmarshal(payload["samples"], &samples)
	if status != 200 || len(samples) != 2 || samples[0].SampleSequence != 5 || samples[1].SampleSequence != 6 || string(payload["next_sequence"]) != "6" || string(payload["gap"]) != "false" {
		t.Fatalf("cursor lost or repeated samples: %d %s", status, payload)
	}
	for _, sample := range samples {
		if !sample.HostQuality.Complete || !sample.HostQuality.AttributionComplete || !sample.HostQuality.ContextAttributionComplete || sample.Quality.AttributionComplete || sample.Quality.ContextAttributionComplete || sample.CollectionStartedAt.IsZero() {
			t.Fatalf("history lost separate host evidence or collection start: %+v", sample)
		}
	}
	status, payload = call("/agent/v1/instances/metrics/samples?after_sequence=0")
	if status != 200 || string(payload["gap"]) != "true" {
		t.Fatal("a truncated initial history must report a gap")
	}
	for _, path := range []string{
		"/agent/v1/instances/metrics?limit=2",
		"/agent/v1/instances/metrics?project=a&project=b",
		"/agent/v1/instances/metrics/samples?after_sequence=4",
		"/agent/v1/instances/metrics/samples?agent_boot_id=" + metricsTestBoot + "&after_sequence=8",
		"/agent/v1/instances/metrics/samples?limit=121",
	} {
		status, _ := call(path)
		if status != 400 {
			t.Errorf("accepted invalid cursor/query %s: %d", path, status)
		}
	}
	status, payload = call("/agent/v1/instances/metrics/samples?agent_boot_id=22222222-2222-4222-8222-222222222222&after_sequence=4")
	if status != 409 || string(payload["error"]) != `"metrics_agent_restarted"` {
		t.Fatal("agent restart must not silently join different counter histories")
	}
}

func TestMetricsStaleAndIncompleteAreNotSuccessfulFreshData(t *testing.T) {
	for _, kind := range []string{"stale", "incomplete"} {
		t.Run(kind, func(t *testing.T) {
			s := metricsTestService(t)
			if kind == "stale" {
				s.metrics.history[2].CollectedAt = time.Now().Add(-10 * time.Second)
			} else {
				s.metrics.history[2].CollectionComplete = false
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest("GET", "/agent/v1/instances/metrics", nil))
			if w.Code != 503 {
				t.Fatalf("%s data advertised as fresh success: %d %s", kind, w.Code, w.Body)
			}
		})
	}
	s, _ := New("/nonexistent/incus.sock")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/agent/v1/instances/metrics", nil))
	if w.Code != 503 {
		t.Fatal("unstarted sampler must not return zero-filled metrics")
	}
}

type stalledMetricsWriter struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
}

func (w *stalledMetricsWriter) Header() http.Header { return w.header }
func (w *stalledMetricsWriter) WriteHeader(int)     {}
func (w *stalledMetricsWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(p), nil
}
func TestSlowMetricsClientCannotBlockSamplerLock(t *testing.T) {
	s := metricsTestService(t)
	w := &stalledMetricsWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeHTTP(w, httptest.NewRequest("GET", "/agent/v1/instances/metrics/samples", nil))
	}()
	<-w.entered
	unlocked := make(chan struct{})
	go func() { s.metrics.mu.Lock(); s.metrics.mu.Unlock(); close(unlocked) }()
	select {
	case <-unlocked:
	case <-time.After(2 * time.Second):
		close(w.release)
		<-done
		t.Fatal("HTTP encoding retained the sampler's lock")
	}
	close(w.release)
	<-done
}

func TestMetricsConfiguredGPUSelection(t *testing.T) {
	resources := testResources()
	devices := map[string]map[string]string{"gpu": {"type": "gpu", "pci": "41:00.0"}}
	ids, problem := metricsConfiguredGPUs(devices, resources)
	if problem != "" || len(ids) != 1 || ids[0] != secondUUID {
		t.Fatalf("PCI selector did not resolve current UUID: %v %s", ids, problem)
	}
	devices["gpu"]["gputype"] = "mig"
	ids, problem = metricsConfiguredGPUs(devices, resources)
	if len(ids) != 0 || problem != "gpu_configuration_unresolved" {
		t.Fatal("unsupported GPU type must not look unattached")
	}
}

type metricsFixtureReader func(context.Context, uint64) (gpu.Sample, error)

func (r metricsFixtureReader) Query(ctx context.Context, cursor uint64) (gpu.Sample, error) {
	return r(ctx, cursor)
}

func TestMetricsCollectionStartPrecedesInputAndGPUCollection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	inputEntered := make(chan time.Time, 1)
	allowInput := make(chan struct{})
	queryEntered := make(chan time.Time, 1)
	nextInput := make(chan struct{})
	done := make(chan struct{})
	reader := metricsFixtureReader(func(context.Context, uint64) (gpu.Sample, error) {
		now := time.Now()
		queryEntered <- now
		return gpu.Sample{CollectedAt: now, Devices: []gpu.Device{{UUID: "GPU-a", ComputeSupported: true, GraphicsSupported: true, AttributionComplete: true}}}, nil
	})
	engine := monitor.NewEngine(monitor.Options{Reader: reader, ProcRoot: t.TempDir()})
	calls := 0
	m := &metricsSampler{
		bootID: metricsTestBoot, interval: time.Millisecond, timeLimit: time.Second,
		history: make([]metricsFrame, 2), engine: engine,
		input: func(sampleCtx context.Context) metricsInput {
			calls++
			if calls == 1 {
				inputEntered <- time.Now()
				select {
				case <-allowInput:
				case <-sampleCtx.Done():
				}
			} else {
				close(nextInput)
				<-sampleCtx.Done()
			}
			return metricsInput{Complete: true, Errors: []string{}, Inventory: []monitor.GPUDevice{{UUID: "GPU-a", Rdevs: []uint64{123456}}}, Instances: []monitor.Instance{{Project: "tenant", Name: "stopped", UUID: "instance-a", Type: "container", State: "Stopped"}, {Project: "other", Name: "other", UUID: "instance-b", Type: "container", State: "Stopped"}}}
		},
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("metrics sampler did not stop")
		}
	})
	beforeRun := time.Now().UTC().Truncate(time.Microsecond)
	go func() {
		defer close(done)
		m.run(ctx)
	}()
	var inputAt time.Time
	select {
	case inputAt = <-inputEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("input collection did not start")
	}
	// Hold native input collection across a real interval so a timestamp taken
	// at engine sampling or frame publication cannot pass as the collection start.
	timer := time.NewTimer(5 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	close(allowInput)
	var queryAt time.Time
	select {
	case queryAt = <-queryEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("GPU collection did not start")
	}
	select {
	case <-nextInput:
	case <-time.After(2 * time.Second):
		t.Fatal("first snapshot was not published")
	}
	s, err := New("/nonexistent/incus.sock")
	if err != nil {
		t.Fatal(err)
	}
	s.metrics = m
	var firstStart time.Time
	for _, path := range []string{
		"/agent/v1/instances/metrics?project=tenant",
		"/agent/v1/instances/metrics?project=*",
		"/agent/v1/instances/metrics/samples?project=tenant",
		"/agent/v1/instances/metrics/samples?project=*",
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("fresh snapshot unavailable at %s: %d %s", path, w.Code, w.Body)
		}
		var payload struct {
			metricsFrame
			Samples []metricsFrame `json:"samples"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		frame := payload.metricsFrame
		if payload.Samples != nil {
			if len(payload.Samples) != 1 {
				t.Fatalf("unexpected history: %s", w.Body)
			}
			frame = payload.Samples[0]
		}
		start := frame.CollectionStartedAt
		if start.IsZero() || start.Before(beforeRun) || start.After(inputAt.UTC().Truncate(time.Microsecond)) || !start.Before(queryAt.UTC().Truncate(time.Microsecond)) || start.After(frame.CollectedAt) || start.Nanosecond()%1000 != 0 || start.Location() != time.UTC {
			t.Fatalf("collection start does not bound actual input and GPU sampling: start=%s before=%s input=%s query=%s frame=%s", start, beforeRun, inputAt, queryAt, w.Body)
		}
		if firstStart.IsZero() {
			firstStart = start
		} else if !start.Equal(firstStart) {
			t.Fatalf("latest/history/project filtering changed collection start: %s", w.Body)
		}
		wantInstances := 1
		if path[len(path)-1] == '*' {
			wantInstances = 2
		}
		if len(frame.Instances) != wantInstances {
			t.Fatalf("project filtering changed: %s", w.Body)
		}
	}
}

func TestMetricsAllProjectsPreservesSampledHolderAfterTargetDeletion(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux host process attribution")
	}
	root := t.TempDir()
	for _, item := range []struct {
		pid   int
		group string
	}{{101, "/instances/target"}, {201, "/instances/holder"}} {
		base := filepath.Join(root, fmt.Sprint(item.pid))
		if err := os.MkdirAll(filepath.Join(base, "fd"), 0700); err != nil {
			t.Fatal(err)
		}
		fields := append([]string{"S"}, strings.Fields(strings.Repeat("0 ", 18))...)
		fields = append(fields, fmt.Sprint(item.pid))
		if err := os.WriteFile(filepath.Join(base, "stat"), []byte(fmt.Sprintf("%d (fixture) %s\n", item.pid, strings.Join(fields, " "))), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "cgroup"), []byte("0::"+item.group+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/dev/null", filepath.Join(base, "fd", "4")); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	memory := uint64(70)
	contexts := []gpu.Process{{PID: 101, UsedMemoryBytes: &memory}, {PID: 201, UsedMemoryBytes: &memory}}
	engine := monitor.NewEngine(monitor.Options{
		ProcRoot:  root,
		Now:       func() time.Time { return now },
		Inventory: []monitor.GPUDevice{{UUID: "GPU-a", DevicePaths: []string{"/dev/null"}}},
		Reader: metricsFixtureReader(func(context.Context, uint64) (gpu.Sample, error) {
			return gpu.Sample{CollectedAt: now, Devices: []gpu.Device{{UUID: "GPU-a", Contexts: contexts, ComputeSupported: true, GraphicsSupported: true, AttributionComplete: true, UtilizationSupported: true}}}, nil
		}),
	})
	inputs := []monitor.Instance{
		{Project: "target-project", Name: "same", UUID: "target-id", Type: "container", State: "Running", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}},
		{Project: "other-project", Name: "same", UUID: "holder-id", Type: "container", State: "Running", InitPID: 201, ConfiguredGPUs: []string{"GPU-a"}},
	}
	before := engine.Sample(context.Background(), inputs)
	if len(before.Instances) != 2 || before.Instances[0].UUID != "target-id" || before.Instances[0].GPUState != "in_use" {
		t.Fatalf("actual pre-delete sample missed target: %+v", before)
	}
	if err := os.RemoveAll(filepath.Join(root, "101")); err != nil {
		t.Fatal(err)
	}
	contexts = contexts[1:]
	confirmedAt := now
	now = now.Add(time.Microsecond)
	collectionStartedAt := now
	after := engine.Sample(context.Background(), inputs[1:])
	if len(after.Instances) != 1 || after.Anonymous != (monitor.AnonymousCounts{}) || !after.HostQuality.ContextAttributionComplete {
		t.Fatalf("actual post-delete sample lost ownership: %+v", after)
	}
	s, err := New("/nonexistent/incus.sock")
	if err != nil {
		t.Fatal(err)
	}
	frames := []metricsFrame{
		{Snapshot: before, AgentBootID: metricsTestBoot, SchemaVersion: 1, SampleSequence: 1, CollectionStartedAt: before.CollectedAt, CollectionComplete: true, CollectionErrors: []string{}},
		{Snapshot: after, AgentBootID: metricsTestBoot, SchemaVersion: 1, SampleSequence: 2, CollectionStartedAt: collectionStartedAt, CollectionComplete: true, CollectionErrors: []string{}},
	}
	s.metrics = &metricsSampler{bootID: metricsTestBoot, interval: time.Second, timeLimit: time.Second, history: frames, count: 2, sequence: 2}
	for _, path := range []string{
		"/agent/v1/instances/metrics?project=*",
		"/agent/v1/instances/metrics/samples?project=*&agent_boot_id=" + metricsTestBoot + "&after_sequence=1",
		"/agent/v1/instances/metrics?project=target-project",
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		var payload struct {
			metricsFrame
			Samples []metricsFrame `json:"samples"`
			Stale   bool           `json:"stale"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusOK || payload.Stale {
			t.Fatalf("fresh post-delete frame not served: %s: %d %s", path, w.Code, w.Body)
		}
		frame := payload.metricsFrame
		if payload.Samples != nil {
			if len(payload.Samples) != 1 {
				t.Fatalf("history cursor returned pre-delete evidence: %s", w.Body)
			}
			frame = payload.Samples[0]
		}
		if frame.SampleSequence != 2 || !frame.CollectionStartedAt.Equal(collectionStartedAt) || !frame.CollectionStartedAt.After(confirmedAt) || !frame.CollectionComplete || !frame.HostQuality.ContextAttributionComplete || frame.Anonymous != (monitor.AnonymousCounts{}) {
			t.Fatalf("projection changed collection/ownership evidence: %s", w.Body)
		}
		if strings.Contains(path, "project=target-project") {
			if len(frame.Instances) != 0 {
				t.Fatalf("target-project projection leaked another project's same-name holder: %s", w.Body)
			}
			continue
		}
		if len(frame.Instances) != 1 {
			t.Fatalf("all-project projection lost known remaining holder: %s", w.Body)
		}
		holder := frame.Instances[0]
		if holder.Project != "other-project" || holder.Name != "same" || holder.UUID != "holder-id" || holder.GPUState != "in_use" || !holder.Quality.ContextAttributionComplete || len(holder.GPUs) != 1 || holder.GPUs[0].UUID != "GPU-a" || holder.GPUs[0].ContextCount == nil || *holder.GPUs[0].ContextCount != 1 || holder.GPUs[0].UsedMemoryBytes == nil || *holder.GPUs[0].UsedMemoryBytes != memory {
			t.Fatalf("all-project projection corrupted live sampled holder: %s", w.Body)
		}
	}
}
