package node

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxc/incus/v6/shared/api"
)

func TestMetricsProviderUsesOnlyLocalClusterPIDsAndResolvesConfiguredGPU(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "incus.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	inventory := testResources()
	inventory.GPU.Cards[0].Nvidia.CardName = "nvidia0"
	local := api.InstanceFull{Instance: api.Instance{Name: "local", Project: "tenant", Location: "node-1", Type: "container", InstancePut: api.InstancePut{Config: map[string]string{"volatile.uuid": "11111111-1111-4111-8111-111111111111"}}}, State: &api.InstanceState{Status: "Running", StatusCode: api.Running, Pid: 123, StartedAt: time.Now().Add(-time.Hour), CPU: api.InstanceStateCPU{Usage: 100}, Memory: api.InstanceStateMemory{Usage: 1024}}}
	local.ExpandedConfig = map[string]string{"limits.cpu": "4", "limits.memory": "4GiB"}
	local.ExpandedDevices = map[string]map[string]string{"gpu": {"type": "gpu", "pci": "0000:01:00.0"}}
	remote := local
	remote.Name = "remote"
	remote.Location = "node-2"
	var missingLocation atomic.Bool
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/1.0":
			name := "node-1"
			if missingLocation.Load() {
				name = ""
			}
			fixtureSync(w, api.Server{ServerUntrusted: api.ServerUntrusted{APIExtensions: []string{"container_full", "instance_all_projects", "resources", "resources_gpu"}}, Environment: api.ServerEnvironment{ServerName: name, ServerClustered: true}})
		case "/1.0/instances":
			if r.URL.Query().Get("all-projects") != "true" || r.URL.Query().Get("recursion") != "2" {
				t.Error("provider did not request complete multi-project state")
			}
			fixtureSync(w, []api.InstanceFull{local, remote})
		case "/1.0/resources":
			fixtureSync(w, inventory)
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	service, err := New(socket)
	if err != nil {
		t.Fatal(err)
	}
	input := service.collectMetricsInput(context.Background())
	if !input.Complete || len(input.Instances) != 1 || input.Instances[0].Name != "local" || input.Instances[0].InitPID != 123 {
		t.Fatalf("remote cluster PID leaked into host mapping: %+v", input)
	}
	instance := input.Instances[0]
	if instance.CPUAllocatedNS == nil || *instance.CPUAllocatedNS != 4_000_000_000 || instance.MemoryLimitBytes == nil || *instance.MemoryLimitBytes != 4*1024*1024*1024 {
		t.Fatal("effective capacity fallback did not preserve explicit instance limits")
	}
	if len(instance.ConfiguredGPUs) != 1 || instance.ConfiguredGPUs[0] != firstUUID || instance.GPUConfigurationError != "" {
		t.Fatal("configured GPU did not resolve using the current node inventory")
	}
	if len(input.Inventory) == 0 || len(input.Inventory[0].DevicePaths) != 1 || input.Inventory[0].DevicePaths[0] != "/dev/nvidia0" {
		t.Fatal("physical card node mapping missing")
	}
	missingLocation.Store(true)
	input = service.collectMetricsInput(context.Background())
	if input.Complete || len(input.Instances) != 0 {
		t.Fatal("unknown cluster identity must not make remote PIDs local")
	}
}

func TestMetricsInventoryDoesNotInventMissingOptionalControlNodes(t *testing.T) {
	resources := testResources()
	resources.GPU.Cards = resources.GPU.Cards[:1]
	resources.GPU.Cards[0].Nvidia.CardName = "nvidia0"
	name := "hostagent-nonexistent-control-node"
	path := filepath.Join("/dev/dri", name)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Skip("optional-node absence precondition not met")
	}
	resources.GPU.Cards[0].DRM = &api.ResourcesGPUCardDRM{ControlName: name}
	devices := metricsDeviceInventory(resources)
	if len(devices) != 1 || len(devices[0].DevicePaths) != 1 || devices[0].DevicePaths[0] != "/dev/nvidia0" {
		t.Fatalf("missing optional control node invalidated the usable CUDA inventory: %+v", devices)
	}
}
