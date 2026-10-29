package monitor

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"101cloud/hostagent/internal/monitor/gpu"
)

type cpuBaseline struct {
	at              time.Time
	usage           uint64
	started         time.Time
	initPID         int
	initStart       uint64
	allocated       uint64
	allocationKnown bool
}

// A cached NVML utilization PID is not an identity. Only samples after an
// earlier observation of this exact process and instance epoch are admissible.
type processBaseline struct {
	start     uint64
	instance  string
	initStart uint64
	at        time.Time
}

type Engine struct {
	mu                 sync.Mutex
	reader             GPUReader
	procRoot           string
	maxProcesses       int
	maxFDs             int
	maxAge             time.Duration
	now                func() time.Time
	devices            map[uint64]string
	controlDevices     map[uint64]bool
	inventoryComplete  bool
	collectionComplete bool
	previous           map[string]cpuBaseline
	processes          map[uint32]processBaseline
	cursor             uint64
}

func NewEngine(options Options) *Engine {
	if options.Reader == nil {
		options.Reader = gpu.NewReader()
	}
	if options.ProcRoot == "" {
		options.ProcRoot = "/proc"
	}
	if options.MaxProcesses <= 0 || options.MaxProcesses > 65536 {
		options.MaxProcesses = 65536
	}
	if options.MaxFDs <= 0 || options.MaxFDs > 262144 {
		options.MaxFDs = 262144
	}
	if options.MaxSampleAge <= 0 {
		options.MaxSampleAge = 15 * time.Second
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	e := &Engine{reader: options.Reader, procRoot: options.ProcRoot, maxProcesses: options.MaxProcesses, maxFDs: options.MaxFDs, maxAge: options.MaxSampleAge, now: options.Now, collectionComplete: true, previous: make(map[string]cpuBaseline)}
	e.setInventory(options.Inventory)
	return e
}

// Close does not take the sampling lock; native work can still be in flight.
func (e *Engine) Close() error {
	if closer, ok := e.reader.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// SetInventory refreshes physical device identities; stale rdev mappings are
// never retained across inventory refreshes. Call before each collection.
func (e *Engine) SetInventory(inventory []GPUDevice) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setInventory(inventory)
}

func (e *Engine) SetCollectionComplete(complete bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.collectionComplete = complete
}

func (e *Engine) setInventory(inventory []GPUDevice) {
	complete := len(inventory) > 0 && len(inventory) <= 256
	if len(inventory) > 256 {
		inventory = inventory[:256]
	}
	e.devices = make(map[uint64]string)
	e.controlDevices = make(map[uint64]bool)
	e.inventoryComplete = complete
	for _, device := range inventory {
		if len(device.DevicePaths) > 64 || len(device.Rdevs) > 64 {
			e.inventoryComplete = false
			continue
		}
		if device.UUID == "" {
			e.inventoryComplete = false
			continue
		}
		add := func(number uint64) {
			if old, exists := e.devices[number]; exists && old != device.UUID {
				e.devices[number] = ""
				e.inventoryComplete = false
			} else {
				e.devices[number] = device.UUID
			}
		}
		for _, number := range device.Rdevs {
			add(number)
		}
		for _, name := range device.DevicePaths {
			info, err := os.Stat(name)
			if err != nil || info.Mode()&os.ModeCharDevice == 0 {
				e.inventoryComplete = false
				continue
			}
			number, ok := deviceNumber(info)
			if !ok {
				e.inventoryComplete = false
				continue
			}
			// Shared NVIDIA control nodes do not identify a physical card.
			leaf := filepath.Base(name)
			if leaf == "nvidiactl" || leaf == "nvidia-uvm" || leaf == "nvidia-uvm-tools" {
				e.controlDevices[number] = true
				e.inventoryComplete = false
				continue
			}
			add(number)
		}
		if len(device.Rdevs) == 0 && len(device.DevicePaths) == 0 {
			e.inventoryComplete = false
		}
	}
	for _, name := range []string{"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools"} {
		if info, err := os.Stat(name); err == nil {
			if number, ok := deviceNumber(info); ok {
				e.controlDevices[number] = true
			}
		}
	}
}

func cloneUint(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func quality(reasons ...string) Quality {
	return Quality{Complete: len(reasons) == 0, AttributionComplete: len(reasons) == 0, Reasons: append([]string{}, reasons...)}
}

func addReason(q *Quality, reason string) {
	for _, current := range q.Reasons {
		if current == reason {
			return
		}
	}
	q.Reasons = append(q.Reasons, reason)
	q.Complete = false
	q.AttributionComplete = false
}

func addMetricReason(q *Quality, reason string) {
	complete := q.AttributionComplete
	addReason(q, reason)
	q.AttributionComplete = complete
}

func mergeQuality(dst *Quality, src Quality) {
	complete := dst.AttributionComplete && src.AttributionComplete
	for _, reason := range src.Reasons {
		addReason(dst, reason)
	}
	dst.AttributionComplete = complete
}

// Sample emits only immutable aggregation DTOs. idle_candidate means that no
// references were observed in this bounded collection, not permission to release
// a device. There is deliberately no release action or policy threshold here.
func (e *Engine) Sample(ctx context.Context, instances []Instance) Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	out := Snapshot{CollectedAt: now, Instances: make([]InstanceMetrics, len(instances)), Quality: quality()}
	proc := procSnapshot{records: make(map[uint32]processRecord), roots: make(map[int]processRecord), references: make(map[int]map[string]int)}
	if supportsProcessAttribution() {
		proc = e.scanProcesses(ctx, instances)
	}
	if !proc.complete {
		addReason(&out.Quality, "process_scan_incomplete")
	}
	if !e.collectionComplete {
		addReason(&out.Quality, "instance_inventory_incomplete")
	}
	if !e.inventoryComplete {
		addReason(&out.Quality, "device_inventory_incomplete")
	}
	raw, err := e.reader.Query(ctx, e.cursor)
	now = e.now()
	out.CollectedAt = now
	if err != nil {
		addReason(&out.Quality, "gpu_collection_failed")
	}
	freshSample := !raw.CollectedAt.IsZero() && now.Sub(raw.CollectedAt) <= e.maxAge && !raw.CollectedAt.After(now.Add(time.Second))
	if !freshSample {
		addReason(&out.Quality, "gpu_sample_stale")
	}
	if ctx.Err() != nil {
		addReason(&out.Quality, "collection_cancelled")
	}
	// Validate each root across the complete scan and native query window.
	for owner, root := range proc.roots {
		after, err := readInstanceRoot(e.procRoot, uint32(instances[owner].InitPID))
		if err != nil || after.start != root.start || !sameGroups(after.groups, root.groups) {
			delete(proc.roots, owner)
			addReason(&out.Quality, "instance_identity_changed")
		}
	}
	owners := make(map[uint32]int)
	nextProcesses := make(map[uint32]processBaseline)
	utilizationSince := make(map[uint32]time.Time)
	for _, device := range raw.Devices {
		for _, process := range device.Contexts {
			if _, seen := owners[process.PID]; seen {
				continue
			}
			owner := -1
			before, exists := proc.records[process.PID]
			if exists && before.owner >= 0 {
				after, readErr := readProcess(e.procRoot, process.PID)
				if _, valid := proc.roots[before.owner]; valid && readErr == nil && before.start == after.start && sameGroups(before.groups, after.groups) {
					owner = before.owner
					instance := instances[owner]
					key := instance.Project + "\x00" + instance.Name + "\x00" + instance.UUID
					identity := processBaseline{start: before.start, instance: key, initStart: proc.roots[owner].start, at: now}
					nextProcesses[process.PID] = identity
					previous, seen := e.processes[process.PID]
					if seen && previous.start == identity.start && previous.instance == identity.instance && previous.initStart == identity.initStart {
						utilizationSince[process.PID] = previous.at
					}
				}
			}
			owners[process.PID] = owner
		}
	}
	out.Anonymous.UnresolvedGPUReferences = proc.unresolved
	devices := make(map[string]gpu.Device)
	for _, device := range raw.Devices {
		// Utilization buffers outlive contexts. Never attach a cached PID sample
		// to a vanished or newly reused identity, or silently drop its activity.
		for _, sample := range device.Utilization {
			timestamp := time.UnixMicro(int64(sample.TimestampMicros))
			if sample.TimestampMicros > math.MaxInt64 || now.Sub(timestamp) > e.maxAge || timestamp.After(now.Add(time.Second)) {
				addMetricReason(&out.Quality, "gpu_utilization_stale")
				continue
			}
			if !(sample.Activity || sample.SMPercent > 0) {
				continue
			}
			owner, owned := owners[sample.PID]
			since, identityKnown := utilizationSince[sample.PID]
			if !owned || owner < 0 || !identityKnown || timestamp.Before(since) {
				out.Anonymous.UnattributedActivitySamples++
			}
		}
		if device.UUID == "" {
			addReason(&out.Quality, "gpu_identity_unknown")
			continue
		}
		if _, exists := devices[device.UUID]; exists {
			addReason(&out.Quality, "gpu_identity_ambiguous")
		}
		devices[device.UUID] = device
		if !device.AttributionComplete || !device.ComputeSupported || !device.GraphicsSupported {
			addReason(&out.Quality, "gpu_attribution_incomplete")
		}
		if device.Error != "" {
			addMetricReason(&out.Quality, "gpu_metric_unavailable")
		}
		if !e.hasDevice(device.UUID) {
			addReason(&out.Quality, "device_inventory_incomplete")
		}
		if !device.UtilizationSupported || len(device.Utilization) == 0 {
			addMetricReason(&out.Quality, "gpu_utilization_unknown")
		}
		for _, process := range device.Contexts {
			if owners[process.PID] < 0 {
				out.Anonymous.UnattributedContexts++
			}
		}
		for _, utilization := range device.Utilization {
			if utilization.TimestampMicros > e.cursor && utilization.TimestampMicros <= uint64(now.Add(time.Second).UnixMicro()) {
				e.cursor = utilization.TimestampMicros
			}
		}
	}
	if len(devices) == 0 {
		addReason(&out.Quality, "gpu_inventory_unavailable")
	}
	if out.Anonymous.UnattributedContexts > 0 {
		addReason(&out.Quality, "unattributed_gpu_contexts")
	}
	if out.Anonymous.UnattributedActivitySamples > 0 {
		addReason(&out.Quality, "unattributed_gpu_activity")
	}
	// Context ownership and reference visibility are independent. A shared
	// control FD must block idle/release decisions, not erase verified usage.
	contextAttributionComplete := out.Quality.AttributionComplete
	if !proc.referencesComplete {
		addReason(&out.Quality, "gpu_reference_scan_incomplete")
	}
	if proc.unresolved > 0 {
		addReason(&out.Quality, "gpu_references_unresolved")
	}
	nextCPU := make(map[string]cpuBaseline, len(instances))
	hostQuality := out.Quality
	for i, instance := range instances {
		row := InstanceMetrics{Project: instance.Project, Name: instance.Name, UUID: instance.UUID, Type: instance.Type, State: instance.State, StartedAt: instance.StartedAt, GPUs: []GPUMetrics{}, GPUState: "unknown", Quality: quality()}
		row.CPU = e.cpu(instance, now, proc.roots[i].start, nextCPU)
		row.Memory = MemoryMetrics{UsageBytes: cloneUint(instance.MemoryUsageBytes), LimitBytes: cloneUint(instance.MemoryLimitBytes), PeakBytes: cloneUint(instance.MemoryPeakBytes)}
		mergeQuality(&row.Quality, hostQuality)
		rowContextComplete := contextAttributionComplete
		if _, valid := proc.roots[i]; !valid {
			addReason(&row.Quality, "instance_attribution_unknown")
			rowContextComplete = false
		}
		if instance.GPUConfigurationError != "" {
			addReason(&row.Quality, "gpu_configuration_unresolved")
			rowContextComplete = false
		}
		configured := make(map[string]bool, len(instance.ConfiguredGPUs))
		for _, uuid := range instance.ConfiguredGPUs {
			configured[uuid] = true
			if _, exists := devices[uuid]; !exists {
				addReason(&row.Quality, "configured_gpu_unresolved")
				rowContextComplete = false
			}
			if !e.hasDevice(uuid) {
				addReason(&row.Quality, "device_inventory_incomplete")
				rowContextComplete = false
			}
		}
		inUse := false
		for uuid, device := range devices {
			metric := aggregateGPU(device, i, owners, utilizationSince, proc.references[i][uuid], now, e.maxAge, raw.CollectedAt)
			metric.Configured = configured[uuid]
			if !metric.Configured && !metric.Observed {
				continue
			}
			metricContextComplete := rowContextComplete && metric.Quality.AttributionComplete
			mergeQuality(&metric.Quality, row.Quality)
			if !metricContextComplete {
				metric.ContextCount = nil
				metric.UsedMemoryBytes = nil
				metric.SMUtilizationPercent = nil
			}
			if !metric.Quality.AttributionComplete {
				metric.ReferencingProcessCount = nil
			}
			if metric.Observed {
				inUse = true
				metric.State = "in_use"
			} else if metric.Quality.AttributionComplete {
				metric.State = "idle_candidate"
			} else {
				metric.State = "unknown"
			}
			row.GPUs = append(row.GPUs, metric)
		}
		// Keep configured but unavailable cards, and old cards visible only by FD.
		missing := make(map[string]bool)
		for uuid := range configured {
			if _, exists := devices[uuid]; !exists {
				missing[uuid] = true
			}
		}
		for uuid, count := range proc.references[i] {
			if _, exists := devices[uuid]; !exists && count > 0 {
				missing[uuid] = true
			}
		}
		for uuid := range missing {
			observed := proc.references[i][uuid] > 0
			metric := GPUMetrics{UUID: uuid, Configured: configured[uuid], Observed: observed, State: "unknown", CollectedAt: now, Quality: quality("gpu_inventory_unavailable")}
			mergeQuality(&metric.Quality, row.Quality)
			if observed {
				inUse = true
				metric.State = "in_use"
			}
			row.GPUs = append(row.GPUs, metric)
		}
		for _, metric := range row.GPUs {
			mergeQuality(&row.Quality, metric.Quality)
		}
		sort.Slice(row.GPUs, func(a, b int) bool { return row.GPUs[a].UUID < row.GPUs[b].UUID })
		if inUse {
			row.GPUState = "in_use"
		} else if row.Quality.AttributionComplete {
			if len(configured) == 0 {
				row.GPUState = "not_attached"
			} else {
				row.GPUState = "idle_candidate"
			}
		}
		out.Instances[i] = row
		mergeQuality(&out.Quality, row.Quality)
	}
	e.processes = nextProcesses
	e.previous = nextCPU
	return out
}

func (e *Engine) hasDevice(uuid string) bool {
	for _, mapped := range e.devices {
		if mapped == uuid {
			return true
		}
	}
	return false
}

func (e *Engine) cpu(instance Instance, now time.Time, initStart uint64, next map[string]cpuBaseline) CPUMetrics {
	// Incus counters were observed before the GPU query. Its duration must not
	// stretch or shrink the CPU counter window.
	if !instance.CPUCollectedAt.IsZero() {
		now = instance.CPUCollectedAt
	}
	result := CPUMetrics{CollectedAt: now.UTC(), UsageNS: cloneUint(instance.CPUUsageNS), AllocatedNS: cloneUint(instance.CPUAllocatedNS)}
	key := instance.Project + "\x00" + instance.Name + "\x00" + instance.UUID
	previous, exists := e.previous[key]
	if instance.CPUUsageNS == nil || !e.collectionComplete {
		return result
	}
	baseline := cpuBaseline{at: now, usage: *instance.CPUUsageNS, started: instance.StartedAt, initPID: instance.InitPID, initStart: initStart}
	if instance.CPUAllocatedNS != nil {
		baseline.allocated = *instance.CPUAllocatedNS
		baseline.allocationKnown = true
	}
	next[key] = baseline
	validEpoch := !instance.StartedAt.IsZero() || (initStart != 0 && previous.initStart == initStart)
	if !exists || !validEpoch || !previous.started.Equal(instance.StartedAt) || previous.initPID != instance.InitPID || previous.initStart != initStart || *instance.CPUUsageNS < previous.usage || !now.After(previous.at) {
		return result
	}
	cores := float64(*instance.CPUUsageNS-previous.usage) / float64(now.Sub(previous.at).Nanoseconds())
	if math.IsNaN(cores) || math.IsInf(cores, 0) {
		return result
	}
	delta := *instance.CPUUsageNS - previous.usage
	window := now.Sub(previous.at).Seconds()
	result.DeltaNS = &delta
	result.WindowSeconds = &window
	result.UsedCores = &cores
	if instance.CPUAllocatedNS != nil && previous.allocationKnown && *instance.CPUAllocatedNS == previous.allocated && *instance.CPUAllocatedNS > 0 {
		percent := cores * 1e9 / float64(*instance.CPUAllocatedNS) * 100
		result.AllocationPercent = &percent
	}
	return result
}

func aggregateGPU(device gpu.Device, owner int, owners map[uint32]int, utilizationSince map[uint32]time.Time, fdCount int, now time.Time, maxAge time.Duration, collected time.Time) GPUMetrics {
	result := GPUMetrics{UUID: device.UUID, ReferencingProcessCount: &fdCount, CollectedAt: collected, State: "in_use", Quality: quality(), UtilizationMethod: "sum_latest_process_sm_percent_capped_100_approximate"}
	contextCount := 0
	if !device.AttributionComplete {
		addReason(&result.Quality, "gpu_attribution_incomplete")
	}
	if device.Error != "" {
		addMetricReason(&result.Quality, "gpu_metric_unavailable")
	}
	memory := uint64(0)
	memoryKnown := device.AttributionComplete
	attributed := make(map[uint32]bool)
	for _, process := range device.Contexts {
		if who, exists := owners[process.PID]; !exists || who != owner {
			continue
		}
		if attributed[process.PID] {
			continue
		}
		attributed[process.PID] = true
		contextCount++
		if process.UsedMemoryBytes == nil || ^uint64(0)-memory < *process.UsedMemoryBytes {
			memoryKnown = false
		} else {
			memory += *process.UsedMemoryBytes
		}
	}
	result.Observed = contextCount > 0 || fdCount > 0
	if device.AttributionComplete {
		result.ContextCount = &contextCount
	}
	if memoryKnown {
		result.UsedMemoryBytes = &memory
	}
	latest := make(map[uint32]gpu.UtilizationSample)
	for _, sample := range device.Utilization {
		if !attributed[sample.PID] {
			continue
		}
		if sample.TimestampMicros > latest[sample.PID].TimestampMicros {
			latest[sample.PID] = sample
		}
	}
	utilizationKnown := device.AttributionComplete && device.UtilizationSupported && contextCount > 0
	total := float64(0)
	for pid := range attributed {
		sample, exists := latest[pid]
		timestamp := time.UnixMicro(int64(sample.TimestampMicros))
		since, identityKnown := utilizationSince[pid]
		if !identityKnown || timestamp.Before(since) {
			utilizationKnown = false
			addMetricReason(&result.Quality, "gpu_utilization_identity_unconfirmed")
			continue
		}
		if !exists || sample.TimestampMicros > math.MaxInt64 || now.Sub(timestamp) > maxAge || timestamp.After(now.Add(time.Second)) || math.IsNaN(sample.SMPercent) || math.IsInf(sample.SMPercent, 0) || sample.SMPercent < 0 || sample.SMPercent > 100 {
			utilizationKnown = false
			continue
		}
		total += sample.SMPercent
	}
	if utilizationKnown {
		total = math.Min(total, 100)
		result.SMUtilizationPercent = &total
	} else {
		addMetricReason(&result.Quality, "gpu_utilization_unknown")
	}
	return result
}
