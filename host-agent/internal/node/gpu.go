package node

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/lxc/incus/v6/shared/api"
)

var (
	gpuUUIDPattern     = regexp.MustCompile(`^GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	managedNamePattern = regexp.MustCompile(`^agent-gpu-(0|[1-9][0-9]*)$`)
)

type gpuRequest struct {
	GPUUUIDs  []string `json:"gpu_uuids"`
	AllowLive bool     `json:"allow_live"`
	AddOnly   bool     `json:"add_only"`
}

type gpuResponse struct {
	Project            string        `json:"project"`
	Instance           string        `json:"instance"`
	RequestedGPUUUIDs  []string      `json:"requested_gpu_uuids"`
	OperationID        string        `json:"operation_id,omitempty"`
	OperationCompleted bool          `json:"operation_completed"`
	ConfigurationState string        `json:"configuration_state"`
	Actual             *api.Instance `json:"actual,omitempty"`
	ETag               string        `json:"etag,omitempty"`
	Error              string        `json:"error,omitempty"`
	Message            string        `json:"message,omitempty"`
	ObservationError   string        `json:"observation_error,omitempty"`
	ResourceRelease    string        `json:"resource_release"`
	Warnings           []string      `json:"warnings"`
}

type requestError struct {
	status  int
	code    string
	message string
}

func (e *requestError) Error() string { return e.message }

func invalidGPU(code, message string) error {
	return &requestError{status: http.StatusBadRequest, code: code, message: message}
}

func conflictGPU(message string) error {
	return &requestError{status: http.StatusConflict, code: "gpu_configuration_conflict", message: message}
}

func validateUUIDs(uuids []string) error {
	seen := make(map[string]bool, len(uuids))
	for _, uuid := range uuids {
		if !gpuUUIDPattern.MatchString(uuid) {
			return invalidGPU("invalid_gpu_uuid", fmt.Sprintf("%q is not an NVIDIA GPU UUID; PCI addresses and MIG UUIDs are not accepted", uuid))
		}
		key := strings.ToLower(uuid)
		if seen[key] {
			return invalidGPU("duplicate_gpu_uuid", fmt.Sprintf("GPU UUID %q is repeated", uuid))
		}
		seen[key] = true
	}
	return nil
}

// selectedDevices resolves only NVIDIA UUIDs advertised by this node. It never
// treats a caller-supplied value as a PCI address or an Incus device selector.
func selectedDevices(resources *api.Resources, uuids []string) (map[string]map[string]string, error) {
	if err := validateUUIDs(uuids); err != nil {
		return nil, err
	}
	devices := make(map[string]map[string]string, len(uuids))
	if len(uuids) == 0 {
		return devices, nil
	}
	if resources == nil {
		return nil, errors.New("node GPU inventory is unavailable")
	}
	type inventoryGPU struct {
		pci   string
		count int
	}
	byUUID := make(map[string]inventoryGPU, len(resources.GPU.Cards))
	for _, card := range resources.GPU.Cards {
		if card.Nvidia != nil && card.Nvidia.UUID != "" {
			key := strings.ToLower(card.Nvidia.UUID)
			entry := byUUID[key]
			entry.pci = card.PCIAddress
			entry.count++
			byUUID[key] = entry
		}
	}
	if len(byUUID) == 0 {
		return nil, &requestError{status: http.StatusUnprocessableEntity, code: "nvidia_uuid_unsupported", message: "this node does not advertise NVIDIA GPU UUIDs; non-NVIDIA GPUs are not supported by this endpoint"}
	}
	seenPCI := make(map[string]bool, len(uuids))
	for index, uuid := range uuids {
		entry := byUUID[strings.ToLower(uuid)]
		if entry.count == 0 {
			return nil, invalidGPU("gpu_uuid_not_found", fmt.Sprintf("GPU UUID %q is not advertised by this node", uuid))
		}
		if entry.count != 1 || entry.pci == "" || seenPCI[entry.pci] {
			return nil, conflictGPU(fmt.Sprintf("GPU UUID %q does not have an unambiguous node-local PCI mapping", uuid))
		}
		seenPCI[entry.pci] = true
		devices[fmt.Sprintf("agent-gpu-%d", index)] = map[string]string{
			"type":    "gpu",
			"gputype": "physical",
			"pci":     entry.pci,
		}
	}
	return devices, nil
}

func checkGPUConflicts(instance *api.Instance, profiles []*api.Profile) error {
	// Profiles are inspected even when masked by local devices: removing a
	// local device would otherwise silently reveal an inherited GPU device.
	for _, profile := range profiles {
		for name, device := range profile.Devices {
			if device["type"] == "gpu" || managedNamePattern.MatchString(name) {
				return conflictGPU(fmt.Sprintf("profile %q defines GPU or reserved device %q; this endpoint does not modify profiles", profile.Name, name))
			}
		}
	}
	for name, device := range instance.Devices {
		managed := managedNamePattern.MatchString(name)
		if device["type"] == "gpu" && !managed {
			return conflictGPU(fmt.Sprintf("unmanaged GPU device %q exists; it will not be removed", name))
		}
		if managed && device["type"] != "gpu" {
			return conflictGPU(fmt.Sprintf("reserved device name %q is used by a non-GPU device", name))
		}
	}
	for name, device := range instance.ExpandedDevices {
		if device["type"] != "gpu" {
			continue
		}
		local, ok := instance.Devices[name]
		if !managedNamePattern.MatchString(name) || !ok || local["type"] != "gpu" {
			return conflictGPU(fmt.Sprintf("expanded GPU device %q is not a local agent-managed device; it will not be removed", name))
		}
	}
	return nil
}

func planGPUUpdate(instance *api.Instance, profiles []*api.Profile, selected map[string]map[string]string, addOnly bool) (api.InstancePut, error) {
	if err := checkGPUConflicts(instance, profiles); err != nil {
		return api.InstancePut{}, err
	}
	put := instance.Writable()
	put.Devices = make(map[string]map[string]string, len(instance.Devices)+len(selected))
	if addOnly {
		wanted := make(map[string]bool, len(selected))
		for _, device := range selected {
			wanted[metricsPCI(device["pci"])] = true
		}
		existing := make(map[string]bool, len(instance.Devices))
		for name, device := range instance.Devices {
			if managedNamePattern.MatchString(name) {
				pci := metricsPCI(device["pci"])
				if pci == "" || (device["gputype"] != "" && device["gputype"] != "physical") || device["id"] != "" || device["vendorid"] != "" || device["productid"] != "" || !wanted[pci] || existing[pci] {
					return api.InstancePut{}, conflictGPU("add_only requires every existing managed physical GPU to remain in the requested node-local set")
				}
				if expanded, ok := instance.ExpandedDevices[name]; ok && !maps.Equal(expanded, device) {
					return api.InstancePut{}, conflictGPU("add_only cannot resolve inconsistent local and expanded GPU configuration")
				}
				existing[pci] = true
			}
			// Retain the exact device name and attributes; replacing an identical
			// PCI under another name can detach an actively used Incus device.
			put.Devices[name] = device
		}
		next := 0
		for _, name := range slices.Sorted(maps.Keys(selected)) {
			device := selected[name]
			if existing[metricsPCI(device["pci"])] {
				continue
			}
			for {
				name = fmt.Sprintf("agent-gpu-%d", next)
				next++
				if _, used := put.Devices[name]; !used {
					break
				}
			}
			put.Devices[name] = device
		}
		return put, nil
	}
	for name, device := range instance.Devices {
		if !managedNamePattern.MatchString(name) {
			put.Devices[name] = device
		}
	}
	for name, device := range selected {
		put.Devices[name] = device
	}
	return put, nil
}

func (s *Service) configureGPU(w http.ResponseWriter, r *http.Request, name string) {
	project, err := requestProject(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_project", err.Error())
		return
	}
	request, err := decodeGPURequest(w, r)
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, "invalid_json", err.Error())
		return
	}
	if err := validateUUIDs(request.GPUUUIDs); err != nil {
		writeRequestError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), updateTimeout)
	defer cancel()
	release, err := s.acquire(ctx, instanceKey{project, name})
	if err != nil {
		writeError(w, http.StatusRequestTimeout, "request_cancelled", err.Error())
		return
	}
	defer release()
	client, err := s.connect(ctx, project)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "incus_unavailable", err.Error())
		return
	}
	defer closeIncus(client)
	server, _, err := client.GetServer()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "incus_unavailable", err.Error())
		return
	}
	instance, etag, err := client.GetInstance(name)
	if err != nil {
		writeError(w, upstreamStatus(err), "instance_read_failed", err.Error())
		return
	}
	if server.Environment.ServerClustered && (server.Environment.ServerName == "" || instance.Location != server.Environment.ServerName) {
		writeError(w, http.StatusConflict, "instance_not_local", "instance is not located on this node; send the request to the instance's host agent")
		return
	}
	if etag == "" {
		writeError(w, http.StatusBadGateway, "missing_etag", "Incus did not provide an instance ETag; refusing an unguarded update")
		return
	}
	profiles := make([]*api.Profile, 0, len(instance.Profiles))
	for _, profileName := range instance.Profiles {
		profile, _, err := client.GetProfile(profileName)
		if err != nil {
			writeError(w, upstreamStatus(err), "profile_read_failed", err.Error())
			return
		}
		profiles = append(profiles, profile)
	}
	var resources *api.Resources
	if len(request.GPUUUIDs) > 0 {
		resources, err = client.GetServerResources()
		if err != nil {
			writeError(w, http.StatusBadGateway, "incus_resources_failed", err.Error())
			return
		}
	}
	selected, err := selectedDevices(resources, request.GPUUUIDs)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	put, err := planGPUUpdate(instance, profiles, selected, request.AddOnly)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	if request.AddOnly {
		// Observe against the preserved names/attributes, not request ordering.
		clear(selected)
		for name, device := range put.Devices {
			if managedNamePattern.MatchString(name) {
				selected[name] = device
			}
		}
	}
	state, _, err := client.GetInstanceState(name)
	if err != nil {
		writeError(w, upstreamStatus(err), "instance_state_read_failed", err.Error())
		return
	}
	// Frozen, transitional and unknown states are not treated as stopped.
	if state.StatusCode != api.Stopped && !request.AllowLive {
		writeError(w, http.StatusConflict, "instance_not_stopped", "GPU changes require a stopped instance; allow_live=true explicitly permits a best-effort live update")
		return
	}
	response := gpuResponse{
		Project:            project,
		Instance:           name,
		RequestedGPUUUIDs:  request.GPUUUIDs,
		ConfigurationState: "unknown",
		ResourceRelease:    "not_verified",
		Warnings:           []string{"Only Incus device configuration is managed. Removing configuration does not prove GPU memory, device handles or compute contexts have been released; no process is killed and no NVML quota is enforced."},
	}
	if request.AllowLive {
		response.Warnings = append(response.Warnings, "Live GPU changes do not reclaim existing GPU contexts or revoke already-open device handles. Incus may reject hotplug, and stopping/restarting the instance may be required.")
	}
	response.Warnings = append(response.Warnings, "Instance ETag protects configuration writes, not concurrent lifecycle, profile or cluster-placement changes by other Incus clients.")
	op, updateErr := client.UpdateInstance(name, put, etag)
	if updateErr == nil {
		if op == nil {
			updateErr = errors.New("Incus returned no update operation")
		} else {
			response.OperationID = op.Get().ID
			updateErr = op.WaitContext(ctx)
			if updateErr == nil && op.Get().StatusCode != api.Success {
				updateErr = fmt.Errorf("Incus update operation is not successful (status %d); completion remains uncertain", op.Get().StatusCode)
			}
			response.OperationCompleted = updateErr == nil
		}
	}
	// A timed-out PUT or failed asynchronous operation may have applied all or
	// part of the configuration. Never infer rollback, and observe independently
	// of a disconnected caller's context. This is a read, never a retry/repair.
	s.observeGPU(project, name, &response)
	if updateErr != nil {
		response.Error = "gpu_update_failed_or_uncertain"
		response.Message = updateErr.Error() + "; rollback is not assumed; inspect actual configuration or reconcile after the operation finishes"
		writeJSON(w, upstreamStatus(updateErr), response)
		return
	}
	if response.Actual == nil {
		response.Error = "gpu_update_state_unknown"
		response.Message = "Incus operation completed but actual configuration could not be read; resource release is not verified"
		writeJSON(w, http.StatusBadGateway, response)
		return
	}
	if !matchesSelected(response.Actual, selected) {
		response.Error = "gpu_configuration_changed"
		response.Message = "observed GPU configuration differs from the requested configuration after the operation; no rollback was attempted"
		writeJSON(w, http.StatusConflict, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Service) observeGPU(project, name string, response *gpuResponse) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := s.connect(ctx, project)
	if err != nil {
		response.ObservationError = err.Error()
		return
	}
	defer closeIncus(client)
	actual, etag, err := client.GetInstance(name)
	if err != nil {
		response.ObservationError = err.Error()
		return
	}
	response.Actual = actual
	response.ETag = etag
	response.ConfigurationState = "observed"
}

func matchesSelected(instance *api.Instance, selected map[string]map[string]string) bool {
	managedCount := 0
	for name, device := range instance.Devices {
		if managedNamePattern.MatchString(name) {
			managedCount++
			wanted, ok := selected[name]
			if !ok || !maps.Equal(device, wanted) {
				return false
			}
		} else if device["type"] == "gpu" {
			return false
		}
	}
	if managedCount != len(selected) {
		return false
	}
	for name, device := range instance.ExpandedDevices {
		if device["type"] == "gpu" {
			wanted, ok := selected[name]
			if !ok || !maps.Equal(device, wanted) {
				return false
			}
		}
	}
	return true
}

func upstreamStatus(err error) int {
	if status, ok := api.StatusErrorMatch(err); ok && slices.Contains([]int{400, 403, 404, 409, 412, 422, 503}, status) {
		return status
	}
	return http.StatusBadGateway
}

func writeRequestError(w http.ResponseWriter, err error) {
	var requestErr *requestError
	if errors.As(err, &requestErr) {
		writeError(w, requestErr.status, requestErr.code, requestErr.message)
		return
	}
	writeError(w, http.StatusBadGateway, "gpu_inventory_error", err.Error())
}
