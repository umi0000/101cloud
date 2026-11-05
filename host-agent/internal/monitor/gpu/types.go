package gpu

import "time"

// Sample is raw host-wide inventory for internal attribution, never an API DTO.
// No utilization observation is not evidence that a GPU has no processes.
type Sample struct {
	CollectedAt time.Time `json:"collected_at"`
	Devices     []Device  `json:"devices"`
}

// Device separates inventory availability from process-utilization availability.
// AttributionComplete excludes known MIG/MPS ambiguity, not all possible causes
// of attribution uncertainty. Consumers must still establish process ownership.
type Device struct {
	UUID                 string              `json:"uuid"`
	Contexts             []Process           `json:"contexts"`
	Utilization          []UtilizationSample `json:"utilization"`
	ComputeSupported     bool                `json:"compute_supported"`
	GraphicsSupported    bool                `json:"graphics_supported"`
	UtilizationSupported bool                `json:"utilization_supported"`
	// UtilizationNoSamples distinguishes normal empty output from a failed query.
	UtilizationNoSamples bool   `json:"utilization_no_samples"`
	AttributionComplete  bool   `json:"attribution_complete"`
	Error                string `json:"error,omitempty"`
}

// Process retains host identity only inside the attribution boundary. Memory is
// nil for NVML_VALUE_NOT_AVAILABLE, never an invented zero.
type Process struct {
	PID             uint32  `json:"-"`
	UsedMemoryBytes *uint64 `json:"used_memory_bytes"`
}

// UtilizationSample is an observation, not a precise share of device capacity.
// Multiple timestamps may be returned for one process.
type UtilizationSample struct {
	PID             uint32  `json:"-"`
	TimestampMicros uint64  `json:"timestamp_micros"`
	SMPercent       float64 `json:"sm_percent"`
	// Activity includes memory and video engines even when SM utilization is zero.
	Activity bool `json:"-"`
}
