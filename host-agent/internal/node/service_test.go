package node

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lxc/incus/v6/shared/api"
)

const (
	firstUUID  = "GPU-11111111-1111-1111-1111-111111111111"
	secondUUID = "GPU-22222222-2222-2222-2222-222222222222"
)

func testResources() *api.Resources {
	return &api.Resources{
		Memory: api.ResourcesMemory{Total: 64 << 30, Used: 4 << 30},
		GPU: api.ResourcesGPU{Total: 2, Cards: []api.ResourcesGPUCard{
			{PCIAddress: "0000:01:00.0", Product: "NVIDIA A100-SXM4-40GB", Nvidia: &api.ResourcesGPUCardNvidia{UUID: firstUUID, Model: "NVIDIA A100-SXM4-40GB"}},
			{PCIAddress: "0000:41:00.0", Product: "NVIDIA GeForce RTX 4090", Nvidia: &api.ResourcesGPUCardNvidia{UUID: secondUUID, Model: "NVIDIA GeForce RTX 4090"}},
		}},
	}
}

func TestUUIDSelection(t *testing.T) {
	selected, err := selectedDevices(testResources(), []string{secondUUID, firstUUID})
	if err != nil {
		t.Fatal(err)
	}
	if selected["agent-gpu-0"]["pci"] != "0000:41:00.0" || selected["agent-gpu-1"]["pci"] != "0000:01:00.0" {
		t.Fatalf("UUIDs were not resolved against local inventory: %#v", selected)
	}
	for _, test := range []struct {
		name      string
		uuids     []string
		resources *api.Resources
	}{
		{"pci_rejected", []string{"0000:01:00.0"}, testResources()},
		{"duplicate_rejected", []string{firstUUID, firstUUID}, testResources()},
		{"case_duplicate_rejected", []string{firstUUID, strings.ToLower(firstUUID)}, testResources()},
		{"unknown_rejected", []string{"GPU-33333333-3333-3333-3333-333333333333"}, testResources()},
		{"non_nvidia_unsupported", []string{firstUUID}, &api.Resources{GPU: api.ResourcesGPU{Cards: []api.ResourcesGPUCard{{PCIAddress: "0000:01:00.0", Vendor: "AMD"}}}}},
		{"missing_pci_rejected", []string{firstUUID}, &api.Resources{GPU: api.ResourcesGPU{Cards: []api.ResourcesGPUCard{{Nvidia: &api.ResourcesGPUCardNvidia{UUID: firstUUID}}}}}},
		{"ambiguous_uuid_rejected", []string{firstUUID}, &api.Resources{GPU: api.ResourcesGPU{Cards: []api.ResourcesGPUCard{
			{PCIAddress: "0000:01:00.0", Nvidia: &api.ResourcesGPUCardNvidia{UUID: firstUUID}},
			{PCIAddress: "0000:02:00.0", Nvidia: &api.ResourcesGPUCardNvidia{UUID: firstUUID}},
		}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := selectedDevices(test.resources, test.uuids); err == nil {
				t.Fatal("unsafe GPU selection accepted")
			}
		})
	}
	if selected, err := selectedDevices(nil, []string{}); err != nil || len(selected) != 0 {
		t.Fatalf("empty selection should not require available GPU hardware: %#v, %v", selected, err)
	}
}

func TestGPUConflictPlanning(t *testing.T) {
	base := api.Instance{InstancePut: api.InstancePut{
		Config: map[string]string{"security.nesting": "true"},
		Devices: map[string]map[string]string{
			"root":        {"type": "disk", "pool": "default", "path": "/"},
			"agent-gpu-0": {"type": "gpu", "pci": "0000:01:00.0"},
		},
	}}
	put, err := planGPUUpdate(&base, nil, map[string]map[string]string{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(put.Devices) != 1 || !maps.Equal(put.Devices["root"], base.Devices["root"]) || !maps.Equal(put.Config, base.Config) {
		t.Fatalf("release changed unrelated configuration: %#v", put)
	}
	if _, ok := base.Devices["agent-gpu-0"]; !ok {
		t.Fatal("planning mutated the original instance")
	}
	for _, test := range []struct {
		name     string
		local    map[string]map[string]string
		expanded map[string]map[string]string
		profiles []*api.Profile
	}{
		{"unmanaged_gpu", map[string]map[string]string{"user-gpu": {"type": "gpu"}}, nil, nil},
		{"reserved_non_gpu", map[string]map[string]string{"agent-gpu-0": {"type": "disk"}}, nil, nil},
		{"inherited_gpu", nil, map[string]map[string]string{"profile-gpu": {"type": "gpu"}}, nil},
		{"inherited_managed_name", nil, map[string]map[string]string{"agent-gpu-0": {"type": "gpu"}}, nil},
		{"masked_profile_gpu", base.Devices, nil, []*api.Profile{{Name: "default", ProfilePut: api.ProfilePut{Devices: map[string]map[string]string{"agent-gpu-0": {"type": "gpu"}}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			instance := api.Instance{InstancePut: api.InstancePut{Devices: test.local}, ExpandedDevices: test.expanded}
			before, _ := json.Marshal(instance)
			if _, err := planGPUUpdate(&instance, test.profiles, nil, false); err == nil {
				t.Fatal("conflicting configuration was accepted")
			}
			after, _ := json.Marshal(instance)
			if string(before) != string(after) {
				t.Fatal("conflicting devices were modified")
			}
		})
	}
}

func TestGPUAddOnlyPlanning(t *testing.T) {
	for _, order := range [][]string{{firstUUID, secondUUID}, {secondUUID, firstUUID}} {
		instance := api.Instance{InstancePut: api.InstancePut{
			Config: map[string]string{"security.nesting": "true", "user.owner": "unchanged"},
			Devices: map[string]map[string]string{
				"root":        {"type": "disk", "pool": "default", "path": "/"},
				"agent-gpu-7": {"type": "gpu", "gputype": "physical", "pci": "01:00.0", "uid": "1000", "gid": "1000", "mode": "0660"},
			},
		}}
		selected, err := selectedDevices(testResources(), order)
		if err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(instance)
		put, err := planGPUUpdate(&instance, nil, selected, true)
		if err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(instance)
		if string(before) != string(after) || len(put.Devices) != 3 || !maps.Equal(put.Config, instance.Config) || !maps.Equal(put.Devices["agent-gpu-7"], instance.Devices["agent-gpu-7"]) || !maps.Equal(put.Devices["root"], instance.Devices["root"]) || put.Devices["agent-gpu-0"]["pci"] != "0000:41:00.0" {
			t.Fatalf("addition renamed/replaced an existing device or unrelated config: %+v", put)
		}
	}
	for _, mode := range []string{"implicit_remove", "empty", "unknown_pci", "missing_pci", "nonphysical", "selector", "duplicate_pci", "expanded_mismatch", "unmanaged", "profile"} {
		t.Run(mode, func(t *testing.T) {
			instance := api.Instance{InstancePut: api.InstancePut{Devices: map[string]map[string]string{
				"agent-gpu-0": {"type": "gpu", "gputype": "physical", "pci": "0000:01:00.0"},
			}}}
			uuids := []string{firstUUID, secondUUID}
			var profiles []*api.Profile
			switch mode {
			case "implicit_remove":
				uuids = []string{secondUUID}
			case "empty":
				uuids = nil
			case "unknown_pci":
				instance.Devices["agent-gpu-0"]["pci"] = "0000:81:00.0"
			case "missing_pci":
				delete(instance.Devices["agent-gpu-0"], "pci")
			case "nonphysical":
				instance.Devices["agent-gpu-0"]["gputype"] = "mig"
			case "selector":
				instance.Devices["agent-gpu-0"]["id"] = "0"
			case "duplicate_pci":
				instance.Devices["agent-gpu-7"] = maps.Clone(instance.Devices["agent-gpu-0"])
			case "expanded_mismatch":
				instance.ExpandedDevices = map[string]map[string]string{"agent-gpu-0": {"type": "gpu", "pci": "0000:41:00.0"}}
			case "unmanaged":
				instance.Devices["manual"] = map[string]string{"type": "gpu", "pci": "0000:41:00.0"}
			case "profile":
				profiles = []*api.Profile{{Name: "gpu", ProfilePut: api.ProfilePut{Devices: map[string]map[string]string{"gpu": {"type": "gpu"}}}}}
			}
			selected, err := selectedDevices(testResources(), uuids)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(instance)
			if _, err := planGPUUpdate(&instance, profiles, selected, true); err == nil {
				t.Fatal("unsafe additive plan accepted")
			}
			after, _ := json.Marshal(instance)
			if string(before) != string(after) {
				t.Fatal("rejected addition mutated existing configuration")
			}
		})
	}
}

func TestSDKGPUAddOnlyPreservesNativeDevices(t *testing.T) {
	for _, mode := range []string{"append", "reorder", "native_extra", "etag-gpu-conflict", "live_consent"} {
		t.Run(mode, func(t *testing.T) {
			fixtureMode := ""
			if mode == "etag-gpu-conflict" {
				fixtureMode = mode
			}
			service, fixture := newFixture(t, fixtureMode)
			instance := fixture.instances["default"]
			instance.Status, instance.StatusCode = "Running", api.Running
			instance.Devices["agent-gpu-0"]["uid"] = "1000"
			original := maps.Clone(instance.Devices["agent-gpu-0"])
			uuids := []string{secondUUID, firstUUID}
			if mode == "reorder" || mode == "native_extra" {
				instance.Devices["agent-gpu-7"] = map[string]string{"type": "gpu", "gputype": "physical", "pci": "0000:41:00.0", "mode": "0660"}
			}
			if mode == "native_extra" {
				uuids = []string{firstUUID}
			}
			body, err := json.Marshal(gpuRequest{GPUUUIDs: uuids, AllowLive: mode != "live_consent", AddOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			response := invokeGPU(service, "", string(body))
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if !maps.Equal(fixture.instances["default"].Devices["agent-gpu-0"], original) {
				t.Fatal("busy GPU device was renamed or changed")
			}
			switch mode {
			case "native_extra", "live_consent":
				if response.Code != http.StatusConflict || fixture.puts != 0 {
					t.Fatalf("unsafe add-only write reached Incus: %d %s", response.Code, response.Body)
				}
			case "etag-gpu-conflict":
				if response.Code != http.StatusPreconditionFailed || fixture.puts != 1 || fixture.waits != 0 || fixture.instances["default"].Devices["agent-gpu-7"]["pci"] != "0000:41:00.0" {
					t.Fatalf("racing native GPU was removed or update retried: %d %s", response.Code, response.Body)
				}
			default:
				if response.Code != http.StatusOK || fixture.puts != 1 || fixture.ifMatch != `"v1"` || len(fixture.instances["default"].Devices) != 3 {
					t.Fatalf("safe addition failed: %d %s", response.Code, response.Body)
				}
				if mode == "reorder" && fixture.instances["default"].Devices["agent-gpu-7"]["mode"] != "0660" {
					t.Fatal("reordered request changed existing GPU attributes")
				}
			}
		})
	}
}

// incusFixture is a stateful Unix-socket protocol fixture, not an echo server.
// PUT creates a pending operation, wait applies configuration, ETags reject stale
// writes, and failure modes deliberately apply state before returning an error.
// All HTTP paths are exercised through the real Incus SDK used by New.
type incusFixture struct {
	mu             sync.Mutex
	instances      map[string]*api.Instance
	versions       map[string]int
	pending        map[string]api.InstancePut
	profiles       map[string]*api.Profile
	resources      *api.Resources
	mode           string
	puts           int
	waits          int
	readsAfterWait int
	ifMatch        string
	projects       []string
}

func newFixture(t *testing.T, mode string) (*Service, *incusFixture) {
	t.Helper()
	fixture := &incusFixture{
		instances: make(map[string]*api.Instance),
		versions:  map[string]int{"default": 1, "tenant": 1},
		pending:   make(map[string]api.InstancePut),
		profiles:  make(map[string]*api.Profile),
		resources: testResources(),
		mode:      mode,
	}
	for _, project := range []string{"default", "tenant"} {
		fixture.instances[project] = &api.Instance{
			Name:       "worker",
			Project:    project,
			Status:     "Stopped",
			StatusCode: api.Stopped,
			InstancePut: api.InstancePut{
				Config: map[string]string{"user.owner": project},
				Devices: map[string]map[string]string{
					"root":        {"type": "disk", "pool": "default", "path": "/"},
					"agent-gpu-0": {"type": "gpu", "gputype": "physical", "pci": "0000:01:00.0"},
				},
			},
		}
	}
	// Keep socket paths below Unix sockaddr limits on both Darwin and Linux.
	dir, err := os.MkdirTemp("", "ha-node-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "incus.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: fixture, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	service, err := New(socket)
	if err != nil {
		t.Fatal(err)
	}
	service.networkCollector = func(context.Context) NetworkInfo {
		return NetworkInfo{Interfaces: []NetworkInterface{}, SystemDNS: SystemDNS{Servers: []string{}, StubServers: []string{}, SearchDomains: []string{}, Source: "test"}, Errors: []string{}}
	}
	return service, fixture
}

func (f *incusFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	project := r.URL.Query().Get("project")
	if project == "" {
		project = "default"
	}
	f.projects = append(f.projects, project)
	instance := f.instances[project]
	if instance == nil {
		fixtureError(w, http.StatusNotFound, "unknown project")
		return
	}
	switch {
	case r.URL.Path == "/1.0":
		fixtureSync(w, map[string]any{
			"api_extensions": []string{"resources", "instances", "projects"},
			"environment":    map[string]any{"server_name": "node-one", "server": "incus"},
		})
	case r.URL.Path == "/1.0/resources":
		fixtureSync(w, f.resources)
	case strings.HasPrefix(r.URL.Path, "/1.0/profiles/"):
		profile := f.profiles[strings.TrimPrefix(r.URL.Path, "/1.0/profiles/")]
		if profile == nil {
			fixtureError(w, http.StatusNotFound, "unknown profile")
			return
		}
		fixtureSync(w, profile)
	case r.URL.Path == "/1.0/instances/worker/state":
		fixtureSync(w, api.InstanceState{Status: instance.Status, StatusCode: instance.StatusCode})
	case r.URL.Path == "/1.0/instances/worker" && r.Method == http.MethodGet:
		if f.waits > 0 {
			f.readsAfterWait++
			if f.mode == "observation-failure" {
				fixtureError(w, http.StatusServiceUnavailable, "daemon unavailable after operation")
				return
			}
		}
		w.Header().Set("ETag", fmt.Sprintf(`"v%d"`, f.versions[project]))
		fixtureSync(w, instance)
	case r.URL.Path == "/1.0/instances/worker" && r.Method == http.MethodPut:
		f.puts++
		f.ifMatch = r.Header.Get("If-Match")
		if f.mode == "etag-conflict" {
			instance.Config["user.external"] = "changed"
			f.versions[project]++
		}
		if f.mode == "etag-gpu-conflict" {
			instance.Devices["agent-gpu-7"] = map[string]string{"type": "gpu", "gputype": "physical", "pci": "0000:41:00.0"}
			f.versions[project]++
		}
		if f.ifMatch != fmt.Sprintf(`"v%d"`, f.versions[project]) {
			fixtureError(w, http.StatusPreconditionFailed, "ETag mismatch")
			return
		}
		var put api.InstancePut
		if err := json.NewDecoder(r.Body).Decode(&put); err != nil {
			fixtureError(w, http.StatusBadRequest, err.Error())
			return
		}
		f.pending[project] = put
		fixtureResponse(w, "async", http.StatusAccepted, api.Operation{ID: "gpu-update", StatusCode: api.Running})
	case r.URL.Path == "/1.0/operations/gpu-update/wait":
		f.waits++
		put, ok := f.pending[project]
		if !ok {
			fixtureError(w, http.StatusNotFound, "no operation in this project")
			return
		}
		instance.InstancePut = put
		instance.ExpandedDevices = put.Devices
		f.versions[project]++
		delete(f.pending, project)
		op := api.Operation{ID: "gpu-update", StatusCode: api.Success}
		if f.mode == "operation-failure" {
			op.StatusCode = api.Failure
			op.Err = "device hotplug failed after configuration changed"
		}
		if f.mode == "operation-pending" {
			op.StatusCode = api.Running
		}
		fixtureSync(w, op)
	default:
		fixtureError(w, http.StatusNotFound, "unexpected fixture endpoint "+r.Method+" "+r.URL.Path)
	}
}

func fixtureResponse(w http.ResponseWriter, kind string, code int, metadata any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": kind, "status": "Success", "status_code": code, "metadata": metadata, "operation": "/1.0/operations/gpu-update"})
}

func fixtureSync(w http.ResponseWriter, metadata any) {
	fixtureResponse(w, "sync", http.StatusOK, metadata)
}

func fixtureError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": message, "error_code": status})
}

func invokeGPU(service *Service, query, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPut, "/agent/v1/instances/worker/gpu"+query, strings.NewReader(body))
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	return response
}

func TestSDKGPUUpdateAndProjectIsolation(t *testing.T) {
	service, fixture := newFixture(t, "")
	response := invokeGPU(service, "?project=tenant", `{"gpu_uuids":["`+secondUUID+`"],"allow_live":false}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.ifMatch != `"v1"` || fixture.waits != 1 || fixture.puts != 1 || fixture.readsAfterWait != 1 {
		t.Fatalf("ETag/write/wait/observation contract not exercised: %#v", fixture)
	}
	if fixture.instances["tenant"].Devices["agent-gpu-0"]["pci"] != "0000:41:00.0" {
		t.Fatal("tenant did not receive UUID-selected PCI device")
	}
	if fixture.instances["default"].Devices["agent-gpu-0"]["pci"] != "0000:01:00.0" {
		t.Fatal("project update leaked into default project")
	}
	for _, project := range fixture.projects {
		if project != "tenant" {
			t.Fatalf("SDK call escaped requested project: %q", project)
		}
	}
	var result gpuResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OperationCompleted || result.ConfigurationState != "observed" || result.ResourceRelease != "not_verified" || result.Actual == nil {
		t.Fatalf("incorrect update result: %#v", result)
	}
}

func TestRunningRequiresExplicitLiveConsent(t *testing.T) {
	for _, state := range []api.StatusCode{api.Running, api.Frozen, api.Starting, api.Error} {
		t.Run(state.String(), func(t *testing.T) {
			service, fixture := newFixture(t, "")
			fixture.instances["default"].StatusCode = state
			response := invokeGPU(service, "", `{"gpu_uuids":[]}`)
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if response.Code != http.StatusConflict || fixture.puts != 0 {
				t.Fatalf("active instance was modified: %d %s", response.Code, response.Body)
			}
		})
	}
	service, fixture := newFixture(t, "")
	fixture.instances["default"].StatusCode = api.Running
	response := invokeGPU(service, "", `{"gpu_uuids":[],"allow_live":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("explicit live update failed: %d %s", response.Code, response.Body)
	}
	var result gpuResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.instances["default"].Devices) != 1 || fixture.instances["default"].Devices["root"]["type"] != "disk" {
		t.Fatal("empty selection must remove only agent GPU configuration")
	}
	if !strings.Contains(strings.Join(result.Warnings, " "), "do not reclaim existing GPU contexts") || result.ResourceRelease != "not_verified" {
		t.Fatal("live update falsely implied resource reclamation")
	}
}

func TestFailureDoesNotAssumeRollback(t *testing.T) {
	for _, test := range []struct {
		mode      string
		status    int
		known     bool
		completed bool
		changed   bool
	}{
		{"operation-failure", http.StatusBadGateway, true, false, true},
		{"operation-pending", http.StatusBadGateway, true, false, true},
		{"observation-failure", http.StatusBadGateway, false, true, true},
		{"etag-conflict", http.StatusPreconditionFailed, true, false, false},
	} {
		t.Run(test.mode, func(t *testing.T) {
			service, fixture := newFixture(t, test.mode)
			response := invokeGPU(service, "", `{"gpu_uuids":["`+secondUUID+`"]}`)
			if response.Code != test.status {
				t.Fatalf("status %d, want %d: %s", response.Code, test.status, response.Body)
			}
			var result gpuResponse
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if (result.Actual != nil) != test.known || result.OperationCompleted != test.completed {
				t.Fatalf("uncertainty misrepresented: %#v", result)
			}
			if !test.known && (result.ConfigurationState != "unknown" || result.ObservationError == "") {
				t.Fatalf("missing explicit unknown-state report: %#v", result)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			actualPCI := fixture.instances["default"].Devices["agent-gpu-0"]["pci"]
			if (actualPCI == "0000:41:00.0") != test.changed || fixture.puts != 1 {
				t.Fatalf("failure retried or rolled back an uncertain update: %q, puts=%d", actualPCI, fixture.puts)
			}
			if test.known && result.Actual.Devices["agent-gpu-0"]["pci"] != actualPCI {
				t.Fatal("failure response did not report observed actual configuration")
			}
			if test.mode == "etag-conflict" && result.Actual.Config["user.external"] != "changed" {
				t.Fatal("external change was overwritten or hidden")
			}
		})
	}
}

func TestHTTPRejectsConflictingGPUDevices(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		t.Run(fmt.Sprintf("inherited=%v", inherited), func(t *testing.T) {
			service, fixture := newFixture(t, "")
			if inherited {
				fixture.instances["default"].Profiles = []string{"gpu-profile"}
				fixture.profiles["gpu-profile"] = &api.Profile{Name: "gpu-profile", ProfilePut: api.ProfilePut{Devices: map[string]map[string]string{"agent-gpu-0": {"type": "gpu", "pci": "0000:41:00.0"}}}}
			} else {
				fixture.instances["default"].Devices["user-gpu"] = map[string]string{"type": "gpu", "pci": "0000:41:00.0"}
			}
			before, _ := json.Marshal(fixture.instances["default"])
			response := invokeGPU(service, "", `{"gpu_uuids":[]}`)
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			after, _ := json.Marshal(fixture.instances["default"])
			if response.Code != http.StatusConflict || fixture.puts != 0 || string(before) != string(after) {
				t.Fatalf("conflict was not safely rejected: %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestRequestValidation(t *testing.T) {
	service, fixture := newFixture(t, "")
	for _, body := range []string{
		``, `{`, `null`, `[]`, `{}`, `{"gpu_uuids":null}`,
		`{"gpu_uuids":[],"unknown":true}`, `{"gpu_uuids":[],"allow_live":null}`,
		`{"gpu_uuids":[],"allow_live":"true"}`, `{"gpu_uuids":[]} {"gpu_uuids":[]}`,
		`{"gpu_uuids":[],"add_only":null}`, `{"gpu_uuids":[],"add_only":"true"}`,
		`{"gpu_uuids":[],"add_only":1}`, `{"gpu_uuids":[],"add_only":true,"add_only":false}`,
		`{"gpu_uuids":[],"gpu_uuids":[]}`, `{"gpu_uuids":["0000:01:00.0"]}`,
		`{"gpu_uuids":["` + firstUUID + `","` + firstUUID + `"]}`,
	} {
		t.Run(body, func(t *testing.T) {
			response := invokeGPU(service, "", body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid request accepted: %d %s", response.Code, response.Body)
			}
		})
	}
	response := invokeGPU(service, "", strings.Repeat(" ", maxRequestBody)+`{"gpu_uuids":[]}`)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request not rejected: %d %s", response.Code, response.Body)
	}
	for _, query := range []string{"?project=", "?project=a&project=b", "?project=tenant&target=remote", "?project=%ZZ"} {
		if response := invokeGPU(service, query, `{"gpu_uuids":[]}`); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid query %q accepted: %d %s", query, response.Code, response.Body)
		}
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.puts != 0 || len(fixture.projects) != 0 {
		t.Fatal("invalid requests reached the Incus daemon")
	}
}

func TestResourcesGPUInventoryAndLoad(t *testing.T) {
	service, fixture := newFixture(t, "")
	value := func(v float64) *float64 { return &v }
	known := []GPUInfo{
		{UUID: firstUUID, PCIAddress: "0000:01:00.0", Model: "NVIDIA A100-SXM4-40GB", UtilizationPercent: value(20), MemoryUsedMiB: value(100), MemoryTotalMiB: value(200), MemoryUtilizationPercent: value(50), LoadPercent: value(35)},
		{UUID: secondUUID, PCIAddress: "0000:41:00.0", Model: "NVIDIA GeForce RTX 4090", UtilizationPercent: value(80), MemoryUsedMiB: value(1000), MemoryTotalMiB: value(4000), MemoryUtilizationPercent: value(25), LoadPercent: value(52.5)},
	}
	// Deliberately reverse telemetry order: the collector must join inventory by
	// identity, not by the position returned by the telemetry command.
	telemetry := secondUUID + ", NVIDIA GeForce RTX 4090, 80, 1000, 4000\n" +
		firstUUID + ", NVIDIA A100-SXM4-40GB, 20, 100, 200\n"
	unknownTelemetry := secondUUID + ", NVIDIA GeForce RTX 4090, 80, 1000, 4000\n" +
		firstUUID + ", NVIDIA A100-SXM4-40GB, N/A, 100, 200\n"
	unknown := append([]GPUInfo(nil), known...)
	unknown[0] = GPUInfo{UUID: firstUUID, PCIAddress: "0000:01:00.0", Model: "NVIDIA A100-SXM4-40GB"}
	failed := []GPUInfo{
		{UUID: firstUUID, PCIAddress: "0000:01:00.0", Model: "NVIDIA A100-SXM4-40GB"},
		{UUID: secondUUID, PCIAddress: "0000:41:00.0", Model: "NVIDIA GeForce RTX 4090"},
	}
	mixedResources := testResources()
	mixedResources.GPU.Total = 3
	mixedResources.GPU.Cards = append(mixedResources.GPU.Cards, api.ResourcesGPUCard{
		PCIAddress: "0000:81:00.0", Product: "AMD Radeon RX 7900 XTX", Vendor: "Advanced Micro Devices, Inc.",
	})
	mixed := append(append([]GPUInfo(nil), known...), GPUInfo{PCIAddress: "0000:81:00.0", Model: "AMD Radeon RX 7900 XTX"})
	noGPUs := testResources()
	noGPUs.GPU = api.ResourcesGPU{Cards: []api.ResourcesGPUCard{}}
	for _, tc := range []struct {
		name          string
		resources     *api.Resources
		telemetry     string
		collectionErr error
		gpus          []GPUInfo
		average       *float64
	}{
		{name: "all models and mean compute-memory load", resources: testResources(), telemetry: telemetry, gpus: known, average: value(43.75)},
		{name: "unknown utilization preserves inventory with null metrics", resources: testResources(), telemetry: unknownTelemetry, gpus: unknown},
		{name: "unavailable telemetry preserves hardware", resources: testResources(), collectionErr: fmt.Errorf("telemetry unavailable"), gpus: failed},
		{name: "non NVIDIA model remains visible", resources: mixedResources, telemetry: telemetry, gpus: mixed},
		{name: "no GPUs returns empty array", resources: noGPUs, gpus: []GPUInfo{}},
		{name: "next request observes recovered telemetry", resources: testResources(), telemetry: telemetry, gpus: known, average: value(43.75)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture.mu.Lock()
			fixture.resources = tc.resources
			fixture.mu.Unlock()
			collected := false
			service.gpuCollector = func(ctx context.Context, resources *api.Resources) ([]GPUInfo, *float64) {
				collected = true
				if _, ok := ctx.Deadline(); !ok {
					t.Error("collector context must retain the request deadline")
				}
				if !reflect.DeepEqual(resources, tc.resources) {
					t.Errorf("collector did not receive live Incus resources: %#v", resources)
				}
				return mergeGPUInfo(resources, parseGPUOutput(tc.telemetry), tc.collectionErr)
			}
			response := httptest.NewRecorder()
			service.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/agent/v1/resources", nil))
			if response.Code != http.StatusOK || !collected {
				t.Fatalf("resource collection failed: collected=%v status=%d body=%s", collected, response.Code, response.Body)
			}
			var result struct {
				ServerResources    *api.Resources `json:"server_resources"`
				GPUs               []GPUInfo      `json:"gpus"`
				AverageLoadPercent *float64       `json:"average_load_percent"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result.ServerResources, tc.resources) {
				t.Fatalf("complete Incus resources were not preserved: %s", response.Body)
			}
			if !reflect.DeepEqual(result.AverageLoadPercent, tc.average) {
				t.Fatalf("incorrect node average: got %v want %v; body=%s", result.AverageLoadPercent, tc.average, response.Body)
			}
			if len(result.GPUs) != len(tc.gpus) {
				t.Fatalf("GPU inventory truncated: got %d want %d; body=%s", len(result.GPUs), len(tc.gpus), response.Body)
			}
			for i, gpu := range result.GPUs {
				if (gpu.Error != "") != (tc.gpus[i].LoadPercent == nil) {
					t.Errorf("GPU %d must report unknown telemetry without pretending it is idle: %+v", i, gpu)
				}
				gpu.Error = ""
				if !reflect.DeepEqual(gpu, tc.gpus[i]) {
					t.Errorf("GPU %d: got %+v want %+v", i, gpu, tc.gpus[i])
				}
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if tc.average == nil && string(payload["average_load_percent"]) != "null" {
				t.Fatalf("unknown average must explicitly be null: %s", response.Body)
			}
			if len(tc.gpus) == 0 && string(payload["gpus"]) != "[]" {
				t.Fatalf("no GPU inventory must be [] rather than null: %s", response.Body)
			}
			var cards []map[string]json.RawMessage
			if err := json.Unmarshal(payload["gpus"], &cards); err != nil {
				t.Fatal(err)
			}
			for i, card := range cards {
				for _, field := range []string{"utilization_percent", "memory_used_mib", "memory_total_mib", "memory_utilization_percent", "load_percent"} {
					if card[field] == nil {
						t.Errorf("GPU %d omits metric %q instead of reporting its value or null", i, field)
					}
				}
			}
		})
	}
}

func TestResourcesRequiresLiveIncus(t *testing.T) {
	service, err := New(filepath.Join(t.TempDir(), "missing-incus.sock"))
	if err != nil {
		t.Fatal(err)
	}
	service.gpuCollector = func(context.Context, *api.Resources) ([]GPUInfo, *float64) {
		t.Fatal("collector must not run without a live Incus resource inventory")
		return nil, nil
	}
	response := httptest.NewRecorder()
	service.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/agent/v1/resources", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("offline resources did not fail: %d %s", response.Code, response.Body)
	}
}

func TestHealth(t *testing.T) {
	service, _ := newFixture(t, "")
	health := httptest.NewRecorder()
	service.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/agent/v1/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health failed: %d %s", health.Code, health.Body)
	}
	offline, err := New(filepath.Join(t.TempDir(), "missing-incus.sock"))
	if err != nil {
		t.Fatalf("offline Incus must not prevent agent startup: %v", err)
	}
	health = httptest.NewRecorder()
	offline.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/agent/v1/health", nil))
	if health.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline Incus health did not return 503: %d %s", health.Code, health.Body)
	}
}

func TestInstanceLockCancellationAndIsolation(t *testing.T) {
	service := &Service{locks: make(map[instanceKey]*instanceLock)}
	release, err := service.acquire(context.Background(), instanceKey{"default", "worker"})
	if err != nil {
		t.Fatal(err)
	}
	otherRelease, err := service.acquire(context.Background(), instanceKey{"tenant", "worker"})
	if err != nil {
		t.Fatal(err)
	}
	otherRelease()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.acquire(ctx, instanceKey{"default", "worker"}); err != context.Canceled {
		t.Fatalf("waiting for lock ignored cancellation: %v", err)
	}
	release()
	if len(service.locks) != 0 {
		t.Fatal("instance locks were retained after requests completed")
	}
}
