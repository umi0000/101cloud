package gpu

import (
	"errors"
	"sort"
	"strings"
	"time"
)

const (
	nvSuccess          int32  = 0
	nvNotSupported     int32  = 3
	nvNotFound         int32  = 6
	nvInsufficientSize int32  = 7
	nvGPULost          int32  = 15
	nvInvalidData      int32  = -1
	maxDevices         uint32 = 4096
	maxEntries         uint32 = 65536
)

// ABI source: NVIDIA's official nvml.h (NVML 13.4.92),
// https://github.com/NVIDIA/go-nvml/blob/main/pkg/nvml/nvml.h
// nvmlProcessInfo_v2_t is uint32 pid, 4 bytes alignment padding, uint64
// usedGpuMemory, uint32 gpuInstanceId, uint32 computeInstanceId: 24 bytes.
// _v3 entry points also take nvmlProcessInfo_t (an alias of v2), NOT the
// 32-byte nvmlProcessDetail_v1_t. We deliberately bind ONLY _v2 entry points,
// with no guessed fallback to unversioned/v3 layouts. On Linux amd64/arm64
// unsigned int is 32 bits and unsigned long long is 64 bits with alignment 8.
type processInfoV2 struct {
	PID               uint32
	_                 uint32
	UsedMemory        uint64
	GPUInstanceID     uint32
	ComputeInstanceID uint32
}

// nvmlProcessUtilizationSample_t: pid@0, timeStamp@8, sm/mem/enc/dec@16/20/24/28.
type processUtilization struct {
	PID       uint32
	_         uint32
	Timestamp uint64
	SM        uint32
	Memory    uint32
	Encoder   uint32
	Decoder   uint32
}

type processFunc func(uintptr, *uint32, *processInfoV2) int32

type nvmlBackend struct {
	count       func(*uint32) int32
	device      func(uint32, *uintptr) int32
	uuid        func(uintptr, *byte, uint32) int32
	compute     processFunc
	graphics    processFunc
	mps         processFunc
	utilization func(uintptr, *processUtilization, *uint32, uint64) int32
	mig         func(uintptr, *uint32, *uint32) int32
	shutdown    func() int32
	unload      func() error
}

func (b *nvmlBackend) close() error {
	if b.shutdown != nil && b.shutdown() != nvSuccess {
		// Do not unload when shutdown failed: NVML may still own references.
		return errors.New("NVML shutdown failed")
	}
	if b.unload != nil {
		return b.unload()
	}
	return nil
}

func status(code int32) string {
	switch code {
	case nvNotSupported:
		return "unsupported"
	case nvNotFound:
		return "no observation"
	case nvGPULost:
		return "GPU lost"
	case nvInvalidData:
		return "invalid data"
	case nvInsufficientSize:
		return "inventory changed or exceeded limit"
	default:
		return "unavailable"
	}
}

func (b *nvmlBackend) collect(since uint64) (Sample, error) {
	s := Sample{Devices: make([]Device, 0)}
	var n uint32
	if b.count(&n) != nvSuccess || n > maxDevices {
		s.CollectedAt = time.Now()
		return s, errors.New("GPU device enumeration unavailable")
	}
	var inventoryError error
	seen := make(map[string]struct{}, n)
	for i := range n {
		var h uintptr
		if code := b.device(i, &h); code != nvSuccess || h == 0 {
			s.Devices = append(s.Devices, Device{Error: "GPU handle " + status(code)})
			inventoryError = errors.New("GPU inventory incomplete")
			continue
		}
		var uuid [80]byte // NVML_DEVICE_UUID_BUFFER_SIZE
		code := b.uuid(h, &uuid[0], uint32(len(uuid)))
		end := 0
		for end < len(uuid) && uuid[end] != 0 {
			end++
		}
		id := string(uuid[:end])
		if code != nvSuccess || !validUUID(id) {
			s.Devices = append(s.Devices, Device{Error: "GPU identity unavailable"})
			inventoryError = errors.New("GPU inventory incomplete")
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			s.Devices = append(s.Devices, Device{Error: "GPU identity invalid data"})
			inventoryError = errors.New("GPU inventory incomplete")
			continue
		}
		seen[id] = struct{}{}
		s.Devices = append(s.Devices, b.collectDevice(h, id, since))
	}
	s.CollectedAt = time.Now()
	return s, inventoryError
}

func validUUID(s string) bool {
	if len(s) != 40 || !strings.HasPrefix(s, "GPU-") {
		return false
	}
	for i := 4; i < len(s); i++ {
		if i == 12 || i == 17 || i == 22 || i == 27 {
			if s[i] != '-' {
				return false
			}
			continue
		}
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func readProcesses(fn processFunc, h uintptr) ([]processInfoV2, int32) {
	if fn == nil {
		return nil, nvNotSupported
	}
	var n uint32
	code := fn(h, &n, nil)
	for range 3 {
		if code == nvSuccess && n == 0 {
			return []processInfoV2{}, nvSuccess
		}
		if code != nvSuccess && code != nvInsufficientSize {
			return nil, code
		}
		if n == 0 || n > maxEntries-32 {
			return nil, nvInvalidData
		}
		buf := make([]processInfoV2, n+32)
		n = uint32(len(buf))
		code = fn(h, &n, &buf[0])
		if code == nvSuccess {
			if n > uint32(len(buf)) {
				return nil, nvInvalidData
			}
			return buf[:n], nvSuccess
		}
	}
	return nil, code
}

func (b *nvmlBackend) collectDevice(h uintptr, id string, since uint64) Device {
	d := Device{UUID: id, AttributionComplete: true, Contexts: make([]Process, 0)}
	var issues []string
	addIssue := func(message string) { issues = append(issues, message); d.AttributionComplete = false }
	type memoryObservation struct {
		bytes uint64
		known bool
	}
	contexts := make(map[uint32]memoryObservation)
	migProcesses := false
	merge := func(records []processInfoV2) bool {
		valid := true
		for _, p := range records {
			if p.PID == 0 {
				valid = false
				continue
			}
			if p.GPUInstanceID != ^uint32(0) || p.ComputeInstanceID != ^uint32(0) {
				migProcesses = true
			}
			memory := memoryObservation{bytes: p.UsedMemory, known: p.UsedMemory != ^uint64(0)}
			previous, exists := contexts[p.PID]
			if !exists {
				contexts[p.PID] = memory
			} else if !previous.known || !memory.known {
				contexts[p.PID] = memoryObservation{}
			} else if memory.bytes > previous.bytes {
				// Each API reports total process memory, not an additive share.
				contexts[p.PID] = memory
			}
		}
		return valid
	}
	for _, query := range []struct {
		name      string
		fn        processFunc
		supported *bool
	}{
		{"compute inventory", b.compute, &d.ComputeSupported},
		{"graphics inventory", b.graphics, &d.GraphicsSupported},
	} {
		records, code := readProcesses(query.fn, h)
		if code == nvSuccess && !merge(records) {
			code = nvInvalidData
		}
		*query.supported = code == nvSuccess
		if code != nvSuccess {
			addIssue(query.name + " " + status(code))
		}
	}
	mps, code := readProcesses(b.mps, h)
	if code == nvNotSupported {
		// This optional MPS query is documented for Volta and newer. On
		// Pascal/Turing variants without it, successful compute/graphics
		// inventories still observe GPU contexts, including an MPS server.
		// NVIDIA attributes client activity to that server in NVML:
		// https://docs.nvidia.com/deploy/mps/when-to-use-mps.html#limitations
		// Report the missing metric without invalidating core inventory.
		issues = append(issues, "MPS inventory unsupported")
	} else if code != nvSuccess {
		addIssue("MPS inventory " + status(code))
	} else {
		if !merge(mps) {
			addIssue("MPS inventory invalid data")
		}
		if len(mps) != 0 {
			addIssue("MPS process attribution incomplete")
		}
	}
	if b.mig == nil {
		addIssue("MIG capability unavailable")
	} else {
		var current, pending uint32
		code := b.mig(h, &current, &pending)
		if code == nvSuccess {
			if current != 0 || pending != 0 {
				addIssue("MIG attribution incomplete")
			}
		} else if code != nvNotSupported {
			addIssue("MIG capability " + status(code))
		}
	}
	d.Contexts = make([]Process, 0, len(contexts))
	for pid, memory := range contexts {
		var bytes *uint64
		if memory.known {
			value := memory.bytes
			bytes = &value
		}
		d.Contexts = append(d.Contexts, Process{PID: pid, UsedMemoryBytes: bytes})
	}
	sort.Slice(d.Contexts, func(i, j int) bool { return d.Contexts[i].PID < d.Contexts[j].PID })
	var utilizationCode int32
	d.Utilization, utilizationCode = b.readUtilization(h, since)
	d.UtilizationSupported = utilizationCode == nvSuccess
	d.UtilizationNoSamples = utilizationCode == nvNotFound
	if utilizationCode != nvSuccess && !d.UtilizationNoSamples {
		// A failed utilization query does not invalidate successful inventories.
		issues = append(issues, "process utilization "+status(utilizationCode))
	}
	if migProcesses {
		addIssue("MIG process attribution incomplete")
	}
	d.Error = strings.Join(issues, "; ")
	return d
}

func (b *nvmlBackend) readUtilization(h uintptr, since uint64) ([]UtilizationSample, int32) {
	if b.utilization == nil {
		return nil, nvNotSupported
	}
	var n uint32
	code := b.utilization(h, nil, &n, since)
	for range 3 {
		if code != nvSuccess && code != nvInsufficientSize {
			return nil, code
		}
		// Only successful empty output supplies a normal missing observation.
		if n == 0 {
			if code == nvSuccess {
				return nil, nvNotFound
			}
			return nil, nvInvalidData
		}
		if n > maxEntries-32 {
			return nil, nvInvalidData
		}
		buf := make([]processUtilization, n+32)
		n = uint32(len(buf))
		code = b.utilization(h, &buf[0], &n, since)
		if code != nvSuccess {
			continue
		}
		if n > uint32(len(buf)) {
			return nil, nvInvalidData
		}
		out := make([]UtilizationSample, 0, n)
		valid := true
		type sampleKey struct {
			pid       uint32
			timestamp uint64
		}
		seen := make(map[sampleKey]int, n)
		for _, p := range buf[:n] {
			if p.PID == 0 || p.Timestamp == 0 || p.Timestamp <= since || p.SM > 100 || p.Memory > 100 || p.Encoder > 100 || p.Decoder > 100 {
				valid = false
				continue
			}
			activity := p.SM != 0 || p.Memory != 0 || p.Encoder != 0 || p.Decoder != 0
			key := sampleKey{p.PID, p.Timestamp}
			if index, exists := seen[key]; exists {
				if float64(p.SM) > out[index].SMPercent {
					out[index].SMPercent = float64(p.SM)
				}
				out[index].Activity = out[index].Activity || activity
				continue
			}
			seen[key] = len(out)
			out = append(out, UtilizationSample{PID: p.PID, TimestampMicros: p.Timestamp, SMPercent: float64(p.SM), Activity: activity})
		}
		if !valid {
			return out, nvInvalidData
		}
		if len(out) == 0 {
			return nil, nvNotFound
		}
		return out, nvSuccess
	}
	return nil, code
}
