package node

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"101cloud/hostagent/internal/monitor"
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
		frames[i] = metricsFrame{Snapshot: monitor.Snapshot{CollectedAt: now, Instances: []monitor.InstanceMetrics{{Project: "default", Name: "first", UUID: "instance-a"}, {Project: "tenant", Name: "second", UUID: "instance-b"}}}, AgentBootID: metricsTestBoot, SchemaVersion: 1, SampleSequence: uint64(i + 5), WindowStart: now.Add(-time.Second), WindowEnd: now, WindowSeconds: 1, CollectionComplete: true, CollectionErrors: []string{}}
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
	status, payload = call("/agent/v1/instances/metrics/samples?project=*&agent_boot_id=" + metricsTestBoot + "&after_sequence=4&limit=2")
	var samples []metricsFrame
	_ = json.Unmarshal(payload["samples"], &samples)
	if status != 200 || len(samples) != 2 || samples[0].SampleSequence != 5 || samples[1].SampleSequence != 6 || string(payload["next_sequence"]) != "6" || string(payload["gap"]) != "false" {
		t.Fatalf("cursor lost or repeated samples: %d %s", status, payload)
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
