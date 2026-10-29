package node

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"101cloud/hostagent/internal/monitor"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/units"
)

type metricsInput struct {
	Instances []monitor.Instance
	Inventory []monitor.GPUDevice
	Complete  bool
	Errors    []string
}

// Metrics always discovers the complete local node, independent of the API's
// output project filter. A remote cluster member's PID is never a local PID.
func (s *Service) collectMetricsInput(ctx context.Context) metricsInput {
	out := metricsInput{Instances: []monitor.Instance{}, Inventory: []monitor.GPUDevice{}, Errors: []string{}}
	client, err := s.connect(ctx, "default")
	if err != nil {
		out.Errors = append(out.Errors, "incus_connection_unavailable")
		return out
	}
	defer closeIncus(client)
	server, _, err := client.GetServer()
	if err != nil {
		out.Errors = append(out.Errors, "incus_server_unavailable")
		return out
	}
	full, err := client.GetInstancesFullAllProjects(api.InstanceTypeAny)
	if err != nil {
		out.Errors = append(out.Errors, "instance_inventory_unavailable")
		return out
	}
	counterObservedAt := time.Now()
	out.Complete = true
	resources, err := client.GetServerResources()
	if err != nil {
		out.Errors = append(out.Errors, "gpu_device_inventory_unavailable")
	} else {
		out.Inventory = metricsDeviceInventory(resources)
	}
	for _, instance := range full {
		if server.Environment.ServerClustered {
			if server.Environment.ServerName == "" || instance.Location == "" {
				out.Complete = false
				out.Errors = append(out.Errors, "instance_location_unknown")
				continue
			}
			if instance.Location != server.Environment.ServerName {
				continue
			}
		}
		project := instance.Project
		if project == "" {
			project = "default"
		}
		item := monitor.Instance{Project: project, Name: instance.Name, UUID: instance.Config["volatile.uuid"], Type: instance.Type, State: instance.Status, ConfiguredGPUs: []string{}, CPUCollectedAt: counterObservedAt}
		if item.UUID == "" {
			out.Complete = false
			out.Errors = append(out.Errors, "instance_identity_unavailable")
		}
		if instance.State == nil {
			out.Complete = false
			out.Errors = append(out.Errors, "instance_state_unavailable")
		} else {
			state := instance.State
			item.InitPID = int(state.Pid)
			item.StartedAt = state.StartedAt
			item.State = state.Status
			if state.StatusCode == api.Running || state.StatusCode == api.Frozen {
				item.CPUUsageNS = metricsNonnegative(state.CPU.Usage)
				if state.CPU.AllocatedTime > 0 {
					item.CPUAllocatedNS = metricsNonnegative(state.CPU.AllocatedTime)
				} else if count, err := strconv.ParseUint(instance.ExpandedConfig["limits.cpu"], 10, 32); err == nil && count > 0 && count <= 65536 {
					value := count * 1_000_000_000
					item.CPUAllocatedNS = &value
				}
				item.MemoryUsageBytes = metricsNonnegative(state.Memory.Usage)
				item.MemoryPeakBytes = metricsNonnegative(state.Memory.UsagePeak)
				if state.Memory.Total > 0 {
					item.MemoryLimitBytes = metricsNonnegative(state.Memory.Total)
				} else if limit, err := units.ParseByteSizeString(instance.ExpandedConfig["limits.memory"]); err == nil && limit > 0 {
					item.MemoryLimitBytes = metricsNonnegative(limit)
				}
			}
		}
		if resources != nil {
			item.ConfiguredGPUs, item.GPUConfigurationError = metricsConfiguredGPUs(instance.ExpandedDevices, resources)
		} else {
			for _, device := range instance.ExpandedDevices {
				if device["type"] == "gpu" {
					item.GPUConfigurationError = "gpu_configuration_unresolved"
				}
			}
		}
		out.Instances = append(out.Instances, item)
	}
	return out
}

func metricsNonnegative(value int64) *uint64 {
	if value < 0 {
		return nil
	}
	result := uint64(value)
	return &result
}

func metricsDeviceInventory(resources *api.Resources) []monitor.GPUDevice {
	result := make([]monitor.GPUDevice, 0)
	for _, card := range resources.GPU.Cards {
		if card.Nvidia == nil || card.Nvidia.UUID == "" {
			continue
		}
		device := monitor.GPUDevice{UUID: card.Nvidia.UUID, DevicePaths: []string{}}
		if name := card.Nvidia.CardName; name != "" && filepath.Base(name) == name && strings.HasPrefix(name, "nvidia") {
			device.DevicePaths = append(device.DevicePaths, filepath.Join("/dev", name))
		}
		if card.DRM != nil {
			for _, name := range []string{card.DRM.CardName, card.DRM.RenderName, card.DRM.ControlName} {
				if name != "" && filepath.Base(name) == name {
					path := filepath.Join("/dev/dri", name)
					// Legacy Incus can report controlD* even when the kernel has
					// never created that optional node. Any surviving unknown FD
					// still makes the reference scan incomplete; permission errors
					// are retained here rather than treated as absent devices.
					if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
						continue
					}
					device.DevicePaths = append(device.DevicePaths, path)
				}
			}
		}
		result = append(result, device)
	}
	return result
}

func metricsConfiguredGPUs(devices map[string]map[string]string, resources *api.Resources) ([]string, string) {
	result := make([]string, 0)
	seen := make(map[string]bool)
	problem := ""
	for _, device := range devices {
		if device["type"] != "gpu" {
			continue
		}
		if kind := device["gputype"]; kind != "" && kind != "physical" {
			problem = "gpu_configuration_unresolved"
			continue
		}
		matched := false
		for _, card := range resources.GPU.Cards {
			if device["pci"] != "" && !strings.EqualFold(metricsPCI(device["pci"]), metricsPCI(card.PCIAddress)) {
				continue
			}
			if device["vendorid"] != "" && !strings.EqualFold(device["vendorid"], card.VendorID) {
				continue
			}
			if device["productid"] != "" && !strings.EqualFold(device["productid"], card.ProductID) {
				continue
			}
			if device["id"] != "" && (card.DRM == nil || device["id"] != strconv.FormatUint(card.DRM.ID, 10)) {
				continue
			}
			matched = true
			if card.Nvidia == nil || card.Nvidia.UUID == "" {
				problem = "gpu_configuration_unresolved"
				continue
			}
			if !seen[card.Nvidia.UUID] {
				result = append(result, card.Nvidia.UUID)
				seen[card.Nvidia.UUID] = true
			}
		}
		if !matched {
			problem = "gpu_configuration_unresolved"
		}
	}
	sort.Strings(result)
	return result, problem
}

func metricsPCI(value string) string {
	value = strings.ToLower(value)
	if strings.Count(value, ":") == 1 {
		return "0000:" + value
	}
	return value
}
