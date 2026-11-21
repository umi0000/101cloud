package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/lxc/incus/v6/shared/api"
)

func TestIncusObservationLimitSharedAcrossInputs(t *testing.T) {
	entered := make(chan struct{}, maxConcurrentIncusObservations)
	release := make(chan struct{})
	var calls atomic.Int32
	service := observationTestService(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/1.0" {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		serveObservationFixture(w, r)
	})

	healthCtx, cancelHealth := context.WithCancel(t.Context())
	defer cancelHealth()
	health := make(chan *httptest.ResponseRecorder, 1)
	go func() { health <- callObservation(healthCtx, service, "health") }()
	waitForObservation(t, entered)
	metrics := make(chan metricsInput, 1)
	go func() { metrics <- service.collectMetricsInput(t.Context()) }()
	waitForObservation(t, entered)

	assertObservationError(t, callObservation(t.Context(), service, "resources"), http.StatusServiceUnavailable, "incus_resources_failed", "admission", "busy")
	if calls.Load() != maxConcurrentIncusObservations {
		t.Fatalf("saturated admission contacted Incus: %d calls", calls.Load())
	}
	cancelHealth()
	assertObservationError(t, waitForObservation(t, health), http.StatusServiceUnavailable, "incus_unavailable", "request", "canceled")
	if response := callObservation(t.Context(), service, "resources"); response.Code != http.StatusOK {
		t.Fatalf("released slots did not allow a live resource read: %d %s", response.Code, response.Body)
	}
	close(release)
	if result := waitForObservation(t, metrics); !result.Complete {
		t.Fatalf("admitted metrics failed: %+v", result)
	}
}

func TestIncusObservationCooldownPreservesPartialMetricsAndRequiresLiveRecovery(t *testing.T) {
	const secret = "Bearer private-token; /1.0/resources?secret=private-query"
	var failResources, blockHealth atomic.Bool
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	service := observationTestService(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch {
		case r.URL.Path == "/1.0/resources" && failResources.Load():
			fixtureError(w, http.StatusForbidden, secret)
			return
		case r.URL.Path == "/1.0" && blockHealth.Load():
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		serveObservationFixture(w, r)
	})
	var clock atomic.Int64
	service.observations.now = func() time.Time { return time.Unix(100, clock.Load()) }
	if response := callObservation(t.Context(), service, "health"); response.Code != http.StatusOK {
		t.Fatalf("initial live health failed: %d %s", response.Code, response.Body)
	}

	failResources.Store(true)
	partial := service.collectMetricsInput(t.Context())
	if partial.Complete || len(partial.Instances) != 1 || partial.Instances[0].Name != "worker" || len(partial.Inventory) != 0 {
		t.Fatalf("GPU inventory failure lost instance evidence or marked the input complete: %+v", partial)
	}
	if !slices.Equal(partial.Errors, []string{"gpu_device_inventory_unavailable", "incus_request_permission_denied"}) {
		t.Fatalf("partial metrics lack safe diagnostics: %v", partial.Errors)
	}
	afterFailure := calls.Load()
	assertObservationError(t, callObservation(t.Context(), service, "health"), http.StatusServiceUnavailable, "incus_unavailable", "admission", "cooldown")
	skipped := service.collectMetricsInput(t.Context())
	if skipped.Complete || len(skipped.Instances) != 0 || !slices.Equal(skipped.Errors, []string{"incus_admission_cooldown"}) {
		t.Fatalf("cooldown returned fresh or incomplete diagnostics: %+v", skipped)
	}
	if calls.Load() != afterFailure {
		t.Fatal("cooldown made an upstream request")
	}

	clock.Add(int64(incusObservationCooldown))
	response := callObservation(t.Context(), service, "resources?secret=private-query")
	assertObservationError(t, response, http.StatusBadGateway, "incus_resources_failed", "request", "permission_denied")
	if strings.Contains(response.Body.String(), "private-") {
		t.Fatalf("the response exposed a token or query: %s", response.Body)
	}
	assertObservationError(t, callObservation(t.Context(), service, "health"), http.StatusServiceUnavailable, "incus_unavailable", "admission", "cooldown")

	clock.Add(int64(incusObservationCooldown))
	failResources.Store(false)
	blockHealth.Store(true)
	probe := make(chan *httptest.ResponseRecorder, 1)
	go func() { probe <- callObservation(t.Context(), service, "health") }()
	waitForObservation(t, entered)
	duringProbe := calls.Load()
	assertObservationError(t, callObservation(t.Context(), service, "resources"), http.StatusServiceUnavailable, "incus_resources_failed", "admission", "cooldown")
	if calls.Load() != duringProbe {
		t.Fatal("a second half-open observation reached Incus")
	}
	close(release)
	if response := waitForObservation(t, probe); response.Code != http.StatusOK {
		t.Fatalf("live recovery probe failed: %d %s", response.Code, response.Body)
	}
	blockHealth.Store(false)
	if input := service.collectMetricsInput(t.Context()); !input.Complete || len(input.Instances) != 1 {
		t.Fatalf("the successful probe did not reopen the gate: %+v", input)
	}
}

func TestIncusObservationIgnoresOlderSuccessAndCanceledProbe(t *testing.T) {
	now := time.Unix(100, 0)
	gate := incusObservationGate{now: func() time.Time { return now }}
	begin := func(probe bool, reason string) {
		t.Helper()
		gotProbe, failure := gate.begin(t.Context())
		if gotProbe != probe || failure.reason != reason {
			t.Fatalf("admission got probe=%v failure=%+v; want probe=%v reason=%q", gotProbe, failure, probe, reason)
		}
	}
	begin(false, "")
	begin(false, "")
	gate.finish(false, incusObservationFailure{phase: "request", reason: "timeout"})
	now = now.Add(incusObservationCooldown)
	begin(false, "cooldown") // Older live read still holds a slot.
	gate.finish(false, incusObservationFailure{})
	begin(true, "") // Its success could not close the gate.
	gate.finish(true, incusObservationFailure{phase: "request", reason: "canceled"})
	begin(true, "") // Caller cancellation is not an upstream failure.
	gate.finish(true, incusObservationFailure{})
	begin(false, "")
	gate.finish(false, incusObservationFailure{})
}

func TestIncusObservationClassifiesTypedFailures(t *testing.T) {
	dial := func(errno syscall.Errno) error {
		return &url.Error{Op: "Get", URL: "http://unix.socket/1.0?token=private-query", Err: &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", errno)}}
	}
	for _, tc := range []struct {
		name string
		err  error
		want incusObservationFailure
	}{
		{"accept backlog", dial(syscall.EAGAIN), incusObservationFailure{"connect", "socket_backlog"}},
		{"dial timeout", dial(syscall.ETIMEDOUT), incusObservationFailure{"connect", "timeout"}},
		{"socket permissions", dial(syscall.EACCES), incusObservationFailure{"connect", "permission_denied"}},
		{"missing socket", dial(syscall.ENOENT), incusObservationFailure{"connect", "socket_unavailable"}},
		{"request timeout", context.DeadlineExceeded, incusObservationFailure{"request", "timeout"}},
		{"HTTP permissions", api.StatusErrorf(http.StatusForbidden, "private token"), incusObservationFailure{"request", "permission_denied"}},
		{"request canceled", context.Canceled, incusObservationFailure{"request", "canceled"}},
		{"other request failure", errors.New("private token"), incusObservationFailure{"request", "request_failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyIncusObservationFailure("request", tc.err); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func observationTestService(t *testing.T, handler http.HandlerFunc) *Service {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ha-observe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "incus.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	service, err := New(socket)
	if err != nil {
		t.Fatal(err)
	}
	service.gpuCollector = func(context.Context, *api.Resources) ([]GPUInfo, *float64) { return []GPUInfo{}, nil }
	service.networkCollector = func(context.Context) NetworkInfo { return NetworkInfo{} }
	return service
}

func serveObservationFixture(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/1.0":
		fixtureSync(w, api.Server{ServerUntrusted: api.ServerUntrusted{APIExtensions: []string{"container_full", "instance_all_projects", "resources"}}, Environment: api.ServerEnvironment{ServerName: "node"}})
	case "/1.0/instances":
		fixtureSync(w, []api.InstanceFull{{Instance: api.Instance{Name: "worker", InstancePut: api.InstancePut{Config: map[string]string{"volatile.uuid": "worker-id"}}}, State: &api.InstanceState{Status: "Stopped", StatusCode: api.Stopped}}})
	case "/1.0/resources":
		fixtureSync(w, &api.Resources{})
	default:
		http.NotFound(w, r)
	}
}

func callObservation(ctx context.Context, service *Service, endpoint string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	service.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodGet, "/agent/v1/"+endpoint, nil))
	return response
}

func assertObservationError(t *testing.T, response *httptest.ResponseRecorder, status int, code, phase, reason string) {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != status || body["error"] != code || body["phase"] != phase || body["reason"] != reason {
		t.Fatalf("want HTTP %d %s (%s/%s), got %d %s", status, code, phase, reason, response.Code, response.Body)
	}
}

func waitForObservation[T any](t *testing.T, done <-chan T) T {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for observation")
		var zero T
		return zero
	}
}
