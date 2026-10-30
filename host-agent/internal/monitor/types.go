package monitor

import (
	"context"
	"time"

	"101cloud/hostagent/internal/monitor/gpu"
)

// Instance is private collection input, never an API response. CPUAllocatedNS
// means allocated CPU nanoseconds per wall-clock second (one CPU = 1e9).
// ConfiguredGPUs contains UUIDs, not device paths; unresolved assignments must
// remain explicit so they cannot become evidence that a GPU is detached.
type Instance struct {
	Project               string    `json:"-"`
	Name                  string    `json:"-"`
	UUID                  string    `json:"-"`
	Type                  string    `json:"-"`
	State                 string    `json:"-"`
	InitPID               int       `json:"-"`
	StartedAt             time.Time `json:"-"`
	ConfiguredGPUs        []string  `json:"-"`
	CPUUsageNS            *uint64   `json:"-"`
	CPUAllocatedNS        *uint64   `json:"-"`
	CPUCollectedAt        time.Time `json:"-"`
	MemoryUsageBytes      *uint64   `json:"-"`
	MemoryLimitBytes      *uint64   `json:"-"`
	GPUConfigurationError string    `json:"-"`
	MemoryPeakBytes       *uint64   `json:"-"`
}

type GPUReader interface {
	Query(context.Context, uint64) (gpu.Sample, error)
}

// GPUDevice maps all per-card device nodes (including DRM render nodes) to a
// physical UUID. Control/uvm nodes shared by multiple cards are not per-card.
// Rdevs is useful when the provider already resolved device node identities.
type GPUDevice struct {
	UUID        string   `json:"-"`
	DevicePaths []string `json:"-"`
	Rdevs       []uint64 `json:"-"`
}

type Options struct {
	Reader       GPUReader
	ProcRoot     string
	Inventory    []GPUDevice
	MaxProcesses int
	MaxFDs       int
	MaxSampleAge time.Duration
	Now          func() time.Time
}

type Quality struct {
	Complete bool `json:"complete"`
	// AttributionComplete covers context inventory, ownership and FD references,
	// independently of optional utilization metric observations.
	AttributionComplete bool     `json:"attribution_complete"`
	Reasons             []string `json:"reasons"`
}

type CPUMetrics struct {
	CollectedAt       time.Time `json:"collected_at"`
	UsageNS           *uint64   `json:"usage_ns"`
	AllocatedNS       *uint64   `json:"allocated_ns_per_second"`
	DeltaNS           *uint64   `json:"delta_ns"`
	WindowSeconds     *float64  `json:"window_seconds"`
	UsedCores         *float64  `json:"used_cores"`
	AllocationPercent *float64  `json:"allocation_percent"`
}

type MemoryMetrics struct {
	UsageBytes *uint64 `json:"usage_bytes"`
	LimitBytes *uint64 `json:"limit_bytes"`
	PeakBytes  *uint64 `json:"peak_bytes"`
}

type GPUMetrics struct {
	UUID       string `json:"uuid"`
	Configured bool   `json:"configured"`
	Observed   bool   `json:"observed"`
	// Distinct attributed GPU processes, not the number of CUDA context objects.
	ContextCount            *int     `json:"process_count"`
	ReferencingProcessCount *int     `json:"referencing_process_count"`
	UsedMemoryBytes         *uint64  `json:"used_memory_bytes"`
	SMUtilizationPercent    *float64 `json:"sm_utilization_percent"`
	// Utilization is the sum of each attributed process's latest SM sample,
	// capped at 100. It is approximate activity, NOT a compute-capacity share.
	UtilizationMethod string    `json:"utilization_method"`
	CollectedAt       time.Time `json:"collected_at"`
	State             string    `json:"state"`
	Quality           Quality   `json:"quality"`
}

type InstanceMetrics struct {
	Project   string        `json:"project"`
	Name      string        `json:"name"`
	UUID      string        `json:"uuid"`
	Type      string        `json:"type"`
	State     string        `json:"state"`
	StartedAt time.Time     `json:"started_at"`
	CPU       CPUMetrics    `json:"cpu"`
	Memory    MemoryMetrics `json:"memory"`
	GPUs      []GPUMetrics  `json:"gpus"`
	GPUState  string        `json:"gpu_state"`
	Quality   Quality       `json:"quality"`
}

// AnonymousCounts intentionally contains no process identifiers or details.
type AnonymousCounts struct {
	UnattributedContexts        int `json:"unattributed_contexts"`
	UnresolvedGPUReferences     int `json:"unresolved_gpu_references"`
	UnattributedActivitySamples int `json:"unattributed_activity_samples"`
}

type Snapshot struct {
	CollectedAt time.Time         `json:"collected_at"`
	Instances   []InstanceMetrics `json:"instances"`
	Quality     Quality           `json:"quality"`
	// HostQuality captures node-wide evidence before per-instance quality merges.
	// Complete attribution is not a claim that GPUs are idle.
	HostQuality Quality         `json:"host_quality"`
	Anonymous   AnonymousCounts `json:"anonymous"`
}
