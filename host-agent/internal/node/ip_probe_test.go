package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const ipProbePath = "/agent/v1/network/ip-probe"
const validIPProbeBody = `{"interface":"ens18","address":"192.168.0.112"}`

type ipProbeHTTPResponse struct {
	IPProbeResult
	Error   string `json:"error"`
	Message string `json:"message"`
}

func newIPProbeTestService(t *testing.T, prober func(context.Context, IPProbeRequest) (IPProbeResult, error)) *Service {
	t.Helper()
	service, err := New("/nonexistent/incus-for-ip-probe-test.socket")
	if err != nil {
		t.Fatal(err)
	}
	if service.ipProber == nil || cap(service.ipProbeSlots) != maxConcurrentIPProbes {
		t.Fatal("New must configure the real prober and four probe slots")
	}
	service.ipProber = prober
	return service
}

func sendIPProbeRequest(t *testing.T, client *http.Client, method, address, body string) (*http.Response, ipProbeHTTPResponse) {
	t.Helper()
	request, err := http.NewRequest(method, address, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result ipProbeHTTPResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Header.Get("Content-Type") != "application/json" || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("missing JSON/no-store headers: %v", response.Header)
	}
	return response, result
}

func requireUnknownIPProbe(t *testing.T, result ipProbeHTTPResponse) {
	t.Helper()
	if result.Status != IPProbeStatusUnknown || result.Available != nil || result.Error == "" || result.Reason == "" {
		t.Fatalf("failure must explicitly be unknown with available=null and an error: %+v", result)
	}
	if result.Warning != IPProbeWarning || !strings.Contains(result.Warning, "reserve") || !strings.Contains(result.Warning, "DHCP") {
		t.Fatalf("missing reservation/DHCP warning: %q", result.Warning)
	}
}

func TestIPProbeHTTPResults(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     string
		available  bool
		responders []IPResponder
	}{
		{"occupied", IPProbeStatusOccupied, false, []IPResponder{{MAC: "02:00:00:00:00:15", Source: "arp_reply"}}},
		{"no_conflict", IPProbeStatusNoConflictDetected, true, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			service := newIPProbeTestService(t, func(ctx context.Context, request IPProbeRequest) (IPProbeResult, error) {
				calls.Add(1)
				if request.Interface != "ens18" || request.Address != "192.168.0.112" {
					return IPProbeResult{}, fmt.Errorf("unexpected request: %+v", request)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > ipProbeTimeout || time.Until(deadline) < 4*time.Second {
					return IPProbeResult{}, errors.New("probe context must be bounded to about five seconds")
				}
				return IPProbeResult{Status: test.status, Responders: test.responders}, nil
			})
			server := httptest.NewServer(service)
			defer server.Close()
			response, result := sendIPProbeRequest(t, server.Client(), http.MethodPost, server.URL+ipProbePath, validIPProbeBody)
			if response.StatusCode != http.StatusOK || calls.Load() != 1 || result.Status != test.status || result.Available == nil || *result.Available != test.available {
				t.Fatalf("unexpected probe response: HTTP %d, calls=%d, %+v", response.StatusCode, calls.Load(), result)
			}
			if result.Interface != "ens18" || result.Address != "192.168.0.112" || result.Warning != IPProbeWarning || result.Responders == nil {
				t.Fatalf("incomplete probe observation: %+v", result)
			}
			if len(result.Responders) != len(test.responders) || (len(test.responders) > 0 && result.Responders[0] != test.responders[0]) {
				t.Fatalf("responders changed: %+v", result.Responders)
			}
		})
	}
}

func TestIPProbeHTTPRejectsInvalidInput(t *testing.T) {
	var calls atomic.Int32
	service := newIPProbeTestService(t, func(context.Context, IPProbeRequest) (IPProbeResult, error) {
		calls.Add(1)
		return IPProbeResult{Status: IPProbeStatusNoConflictDetected}, nil
	})
	server := httptest.NewServer(service)
	defer server.Close()
	for _, test := range []struct {
		name   string
		body   string
		query  string
		status int
	}{
		{"empty", "", "", 400},
		{"null", "null", "", 400},
		{"array", "[]", "", 400},
		{"string", `"value"`, "", 400},
		{"missing_both", `{}`, "", 400},
		{"missing_interface", `{"address":"192.168.0.112"}`, "", 400},
		{"missing_address", `{"interface":"ens18"}`, "", 400},
		{"null_interface", `{"interface":null,"address":"192.168.0.112"}`, "", 400},
		{"null_address", `{"interface":"ens18","address":null}`, "", 400},
		{"numeric_address", `{"interface":"ens18","address":123}`, "", 400},
		{"object_interface", `{"interface":{},"address":"192.168.0.112"}`, "", 400},
		{"duplicate_interface", `{"interface":"ens18","interface":"ens19","address":"192.168.0.112"}`, "", 400},
		{"duplicate_address", `{"interface":"ens18","address":"192.168.0.112","address":"192.168.0.113"}`, "", 400},
		{"escaped_duplicate", `{"interface":"ens18","\u0069nterface":"ens19","address":"192.168.0.112"}`, "", 400},
		{"unknown", `{"interface":"ens18","address":"192.168.0.112","count":99}`, "", 400},
		{"case_variant", `{"Interface":"ens18","address":"192.168.0.112"}`, "", 400},
		{"multiple_objects", validIPProbeBody + `{}`, "", 400},
		{"trailing_null", validIPProbeBody + `null`, "", 400},
		{"trailing_garbage", validIPProbeBody + `garbage`, "", 400},
		{"malformed", `{"interface":"ens18","address":`, "", 400},
		{"trailing_comma", `{"interface":"ens18","address":"192.168.0.112",}`, "", 400},
		{"empty_interface", `{"interface":"","address":"192.168.0.112"}`, "", 400},
		{"interface_space", `{"interface":" ens18","address":"192.168.0.112"}`, "", 400},
		{"interface_path", `{"interface":"../ens18","address":"192.168.0.112"}`, "", 400},
		{"interface_null_byte", `{"interface":"ens\u000018","address":"192.168.0.112"}`, "", 400},
		{"interface_too_long", `{"interface":"abcdefghijklmnop","address":"192.168.0.112"}`, "", 400},
		{"empty_address", `{"interface":"ens18","address":""}`, "", 400},
		{"hostname", `{"interface":"ens18","address":"localhost"}`, "", 400},
		{"cidr", `{"interface":"ens18","address":"192.168.0.0/24"}`, "", 400},
		{"range", `{"interface":"ens18","address":"192.168.0.1-192.168.0.20"}`, "", 400},
		{"multiple_ips", `{"interface":"ens18","address":["192.168.0.112","192.168.0.113"]}`, "", 400},
		{"ipv6", `{"interface":"ens18","address":"2001:db8::1"}`, "", 400},
		{"mapped_ipv6", `{"interface":"ens18","address":"::ffff:192.168.0.112"}`, "", 400},
		{"address_space", `{"interface":"ens18","address":"192.168.0.112 "}`, "", 400},
		{"query", validIPProbeBody, "?count=3", 400},
		{"empty_query_value", validIPProbeBody, "?foo", 400},
		{"bare_query", validIPProbeBody, "?", 400},
		{"oversized", validIPProbeBody + strings.Repeat(" ", maxIPProbeBody), "", 413},
		{"oversized_invalid_json", strings.Repeat("x", maxIPProbeBody+1), "", 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, result := sendIPProbeRequest(t, server.Client(), http.MethodPost, server.URL+ipProbePath+test.query, test.body)
			if response.StatusCode != test.status || result.Error == "" {
				t.Fatalf("HTTP %d, expected %d; %+v", response.StatusCode, test.status, result)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid requests reached the prober %d times", calls.Load())
	}
	body := validIPProbeBody + strings.Repeat(" ", maxIPProbeBody-len(validIPProbeBody))
	response, _ := sendIPProbeRequest(t, server.Client(), http.MethodPost, server.URL+ipProbePath, body)
	if response.StatusCode != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("exactly 4 KiB must be accepted: HTTP %d, calls=%d", response.StatusCode, calls.Load())
	}
}

func TestIPProbeHTTPMethodAndRoute(t *testing.T) {
	service := newIPProbeTestService(t, func(context.Context, IPProbeRequest) (IPProbeResult, error) {
		return IPProbeResult{}, errors.New("method/unknown path must not call the prober")
	})
	server := httptest.NewServer(service)
	defer server.Close()
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		response, result := sendIPProbeRequest(t, server.Client(), method, server.URL+ipProbePath, validIPProbeBody)
		if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodPost || result.Error != "method_not_allowed" {
			t.Fatalf("%s: unexpected method response: HTTP %d, %+v", method, response.StatusCode, result)
		}
	}
	response, _ := sendIPProbeRequest(t, server.Client(), http.MethodPost, server.URL+ipProbePath+"/", validIPProbeBody)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unexpected route accepted: HTTP %d", response.StatusCode)
	}
}

func TestIPProbeHTTPUnknownNeverAvailable(t *testing.T) {
	for _, test := range []struct {
		name         string
		resultStatus string
		err          error
		status       int
	}{
		{"unsupported", IPProbeStatusNoConflictDetected, &ProbeError{Status: 501, Code: "unsupported_platform", Message: "Linux required"}, 501},
		{"permission", IPProbeStatusNoConflictDetected, &ProbeError{Status: 503, Code: "socket_permission_denied", Message: "CAP_NET_RAW required"}, 503},
		{"invalid_interface", IPProbeStatusNoConflictDetected, &ProbeError{Status: 400, Code: "invalid_interface", Message: "interface does not exist"}, 400},
		{"timeout", IPProbeStatusNoConflictDetected, context.DeadlineExceeded, 504},
		{"canceled", IPProbeStatusNoConflictDetected, context.Canceled, 503},
		{"unexpected_error", IPProbeStatusNoConflictDetected, errors.New("socket read failed"), 503},
		{"unknown_without_error", IPProbeStatusUnknown, nil, 503},
		{"empty_status", "", nil, 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newIPProbeTestService(t, func(context.Context, IPProbeRequest) (IPProbeResult, error) {
				available := true
				return IPProbeResult{Status: test.resultStatus, Available: &available}, test.err
			})
			server := httptest.NewServer(service)
			defer server.Close()
			response, result := sendIPProbeRequest(t, server.Client(), http.MethodPost, server.URL+ipProbePath, validIPProbeBody)
			if response.StatusCode != test.status {
				t.Fatalf("HTTP %d, expected %d", response.StatusCode, test.status)
			}
			requireUnknownIPProbe(t, result)
		})
	}
}

func TestIPProbeHTTPConcurrencyAndCancellation(t *testing.T) {
	started := make(chan struct{}, 8)
	completed := make(chan string, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var active, maximum atomic.Int32
	service := newIPProbeTestService(t, func(ctx context.Context, _ IPProbeRequest) (IPProbeResult, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); current > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			return IPProbeResult{Status: IPProbeStatusNoConflictDetected}, nil
		case <-ctx.Done():
			// Deliberately return a success to verify that request cancellation
			// still overrides the observation at the HTTP boundary.
			return IPProbeResult{Status: IPProbeStatusNoConflictDetected}, nil
		}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.ServeHTTP(w, r)
		completed <- r.Header.Get("X-Test-ID")
	}))
	defer server.Close()
	// Release before server.Close even if a failure leaves requests blocked.
	defer releaseOnce.Do(func() { close(release) })
	results := make(chan error, 5)
	launch := func(ctx context.Context, id string) {
		go func() {
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+ipProbePath, strings.NewReader(validIPProbeBody))
			if err != nil {
				results <- err
				return
			}
			request.Header.Set("X-Test-ID", id)
			response, err := server.Client().Do(request)
			if err == nil {
				_, err = io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if err == nil && response.StatusCode != http.StatusOK {
					err = fmt.Errorf("request %s: HTTP %d", id, response.StatusCode)
				}
			}
			results <- err
		}()
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	for i := range maxConcurrentIPProbes {
		ctx := context.Background()
		if i == 0 {
			ctx = firstCtx
		}
		launch(ctx, fmt.Sprintf("slot-%d", i))
	}
	for range maxConcurrentIPProbes {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("four probes did not start concurrently")
		}
	}
	before := time.Now()
	response, result := sendIPProbeRequest(t, server.Client(), http.MethodPost, server.URL+ipProbePath, validIPProbeBody)
	if response.StatusCode != http.StatusServiceUnavailable || result.Error != "probe_busy" || time.Since(before) > time.Second {
		t.Fatalf("saturation must fail immediately: HTTP %d, %+v", response.StatusCode, result)
	}
	requireUnknownIPProbe(t, result)
	cancelFirst()
	for finished := false; !finished; {
		select {
		case id := <-completed:
			finished = id == "slot-0"
		case <-time.After(3 * time.Second):
			t.Fatal("canceled request did not finish and release its slot")
		}
	}
	if len(service.ipProbeSlots) != maxConcurrentIPProbes-1 {
		t.Fatalf("canceled probe leaked its slot: %d", len(service.ipProbeSlots))
	}
	launch(context.Background(), "replacement")
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement probe did not acquire the canceled request's slot")
	}
	releaseOnce.Do(func() { close(release) })
	canceled := 0
	for range 5 {
		select {
		case err := <-results:
			if errors.Is(err, context.Canceled) {
				canceled++
			} else if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("probe requests did not finish")
		}
	}
	if canceled != 1 || maximum.Load() != maxConcurrentIPProbes || active.Load() != 0 || len(service.ipProbeSlots) != 0 {
		t.Fatalf("bad concurrency cleanup: canceled=%d maximum=%d active=%d slots=%d", canceled, maximum.Load(), active.Load(), len(service.ipProbeSlots))
	}
}

func TestIPProbeCanceledContextOverridesSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	service := newIPProbeTestService(t, func(context.Context, IPProbeRequest) (IPProbeResult, error) {
		cancel()
		return IPProbeResult{Status: IPProbeStatusNoConflictDetected}, nil
	})
	request := httptest.NewRequest(http.MethodPost, ipProbePath, strings.NewReader(validIPProbeBody)).WithContext(ctx)
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	var result ipProbeHTTPResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable || len(service.ipProbeSlots) != 0 {
		t.Fatalf("canceled probe must return 503 and release its slot: HTTP %d slots=%d", response.Code, len(service.ipProbeSlots))
	}
	requireUnknownIPProbe(t, result)
}
