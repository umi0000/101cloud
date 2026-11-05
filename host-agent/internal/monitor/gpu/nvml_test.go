package gpu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

const testUUID = "GPU-12345678-1234-1234-1234-123456789abc"

func processFixture(pid uint32, memory uint64) processInfoV2 {
	return processInfoV2{PID: pid, UsedMemory: memory, GPUInstanceID: ^uint32(0), ComputeInstanceID: ^uint32(0)}
}

func processQuery(records ...processInfoV2) processFunc {
	return func(_ uintptr, count *uint32, output *processInfoV2) int32 {
		if output == nil {
			*count = uint32(len(records))
			if len(records) == 0 {
				return nvSuccess
			}
			return nvInsufficientSize
		}
		capacity := *count
		*count = uint32(len(records))
		if capacity < *count {
			return nvInsufficientSize
		}
		copy(unsafe.Slice(output, int(capacity)), records)
		return nvSuccess
	}
}

func fixtureBackend() *nvmlBackend {
	return &nvmlBackend{
		count:  func(n *uint32) int32 { *n = 1; return nvSuccess },
		device: func(_ uint32, h *uintptr) int32 { *h = 1; return nvSuccess },
		uuid: func(_ uintptr, output *byte, n uint32) int32 {
			copy(unsafe.Slice(output, int(n)), testUUID+"\x00")
			return nvSuccess
		},
		compute: processQuery(), graphics: processQuery(), mps: processQuery(),
		mig:         func(_ uintptr, current, pending *uint32) int32 { *current = 0; *pending = 0; return nvSuccess },
		utilization: func(_ uintptr, _ *processUtilization, _ *uint32, _ uint64) int32 { return nvNotFound },
	}
}

func TestConfirmedNVMLABI(t *testing.T) {
	var p processInfoV2
	if unsafe.Sizeof(p) != 24 || unsafe.Offsetof(p.UsedMemory) != 8 || unsafe.Offsetof(p.GPUInstanceID) != 16 || unsafe.Offsetof(p.ComputeInstanceID) != 20 {
		t.Fatal("process v2 ABI mismatch")
	}
	var u processUtilization
	if unsafe.Sizeof(u) != 32 || unsafe.Offsetof(u.Timestamp) != 8 || unsafe.Offsetof(u.SM) != 16 || unsafe.Offsetof(u.Decoder) != 28 {
		t.Fatal("process utilization ABI mismatch")
	}
}

func TestProcessConversionDeduplicatesMemory(t *testing.T) {
	b := fixtureBackend()
	b.compute = processQuery(processFixture(3123456789, 100), processFixture(22, ^uint64(0)))
	b.graphics = processQuery(processFixture(3123456789, 120), processFixture(22, 200))
	b.utilization = utilizationQuery(processUtilization{PID: 3123456789, Timestamp: 100, SM: 1})
	sample, err := b.collect(0)
	if err != nil || len(sample.Devices) != 1 {
		t.Fatal("fixture collection failed")
	}
	d := sample.Devices[0]
	if !d.ComputeSupported || !d.GraphicsSupported || !d.AttributionComplete || len(d.Contexts) != 2 {
		t.Fatal("valid inventories not recognized")
	}
	if d.Contexts[0].PID != 22 || d.Contexts[0].UsedMemoryBytes != nil {
		t.Fatal("unknown memory converted to a number")
	}
	if d.Contexts[1].PID != 3123456789 || d.Contexts[1].UsedMemoryBytes == nil || *d.Contexts[1].UsedMemoryBytes != 120 {
		t.Fatal("duplicate context memory was added or lost")
	}
	encoded, err := json.Marshal(sample)
	if err != nil {
		t.Fatal("sample encoding failed")
	}
	for _, forbidden := range []string{"3123456789", "PID", "pid", "Activity", "activity", "process_name", "command", "path"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatal("raw process details escaped JSON")
		}
		if strings.Contains(d.Error, forbidden) {
			t.Fatal("raw process details escaped errors")
		}
	}
}

func TestMissingObservationsAreNotZero(t *testing.T) {
	for _, code := range []int32{nvNotSupported, nvNotFound, nvGPULost, nvInvalidData, nvInsufficientSize, 999} {
		t.Run(status(code), func(t *testing.T) {
			b := fixtureBackend()
			b.compute = processQuery(processFixture(42, 0))
			b.utilization = func(_ uintptr, _ *processUtilization, _ *uint32, _ uint64) int32 { return code }
			d := b.collectDevice(1, testUUID, 0)
			noSamples := code == nvNotFound
			if d.UtilizationSupported || d.Utilization != nil || d.UtilizationNoSamples != noSamples || (d.Error == "") != noSamples || len(d.Contexts) != 1 || !d.AttributionComplete {
				t.Fatalf("missing utilization fabricated zero, erased contexts, or misclassified an error: %+v", d)
			}
			b.compute = func(_ uintptr, _ *uint32, _ *processInfoV2) int32 { return code }
			d = b.collectDevice(1, testUUID, 0)
			if d.ComputeSupported || d.AttributionComplete || d.Error == "" {
				t.Fatal("missing process inventory considered complete")
			}
		})
	}
}

func utilizationQuery(records ...processUtilization) func(uintptr, *processUtilization, *uint32, uint64) int32 {
	return func(_ uintptr, output *processUtilization, count *uint32, _ uint64) int32 {
		if output == nil {
			*count = uint32(len(records))
			return nvInsufficientSize
		}
		capacity := *count
		*count = uint32(len(records))
		if capacity < *count {
			return nvInsufficientSize
		}
		copy(unsafe.Slice(output, int(capacity)), records)
		return nvSuccess
	}
}

func TestUtilizationConversionAndValidation(t *testing.T) {
	b := fixtureBackend()
	b.utilization = utilizationQuery(
		processUtilization{PID: 42, Timestamp: 100, SM: 20},
		processUtilization{PID: 42, Timestamp: 100, SM: 30},
		processUtilization{PID: 42, Timestamp: 101, SM: 0},
	)
	d := b.collectDevice(1, testUUID, 99)
	if !d.UtilizationSupported || d.UtilizationNoSamples || len(d.Utilization) != 2 || d.Utilization[0].TimestampMicros != 100 || d.Utilization[0].SMPercent != 30 || d.Utilization[1].SMPercent != 0 {
		t.Fatal("valid timestamped samples not retained correctly")
	}
	for _, invalid := range []processUtilization{
		{Timestamp: 100, SM: 1}, {PID: 42, SM: 1}, {PID: 42, Timestamp: 99, SM: 1},
		{PID: 42, Timestamp: 100, SM: 101}, {PID: 42, Timestamp: 100, Memory: ^uint32(0)},
	} {
		b.utilization = utilizationQuery(invalid)
		d := b.collectDevice(1, testUUID, 99)
		if d.UtilizationSupported || d.UtilizationNoSamples || len(d.Utilization) != 0 || !strings.Contains(d.Error, "invalid data") {
			t.Fatal("invalid sample treated as a valid activity observation")
		}
	}
	b.utilization = func(_ uintptr, _ *processUtilization, n *uint32, _ uint64) int32 { *n = 0; return nvSuccess }
	if d := b.collectDevice(1, testUUID, 0); d.UtilizationSupported || d.Utilization != nil || !d.UtilizationNoSamples || d.Error != "" {
		t.Fatal("empty sample success became a failure or observed zero")
	}
}

func TestCompleteInventoriesWithNoUtilizationSamples(t *testing.T) {
	for _, mode := range []string{"not_found", "empty_success", "not_found_after_size", "empty_success_after_size"} {
		t.Run(mode, func(t *testing.T) {
			for _, active := range []bool{false, true} {
				b := fixtureBackend()
				if active {
					b.compute = processQuery(processFixture(42, 1024))
				}
				b.utilization = func(_ uintptr, output *processUtilization, n *uint32, since uint64) int32 {
					if since != 100 {
						t.Fatalf("incremental sample cursor was lost: %d", since)
					}
					if strings.HasSuffix(mode, "after_size") && output == nil {
						*n = 1
						return nvInsufficientSize
					}
					*n = 0
					if strings.HasPrefix(mode, "not_found") {
						return nvNotFound
					}
					return nvSuccess
				}
				sample, err := b.collect(100)
				if err != nil || len(sample.Devices) != 1 {
					t.Fatalf("normal empty utilization invalidated device inventory: %+v %v", sample, err)
				}
				d := sample.Devices[0]
				if !d.ComputeSupported || !d.GraphicsSupported || !d.AttributionComplete || !d.UtilizationNoSamples || d.UtilizationSupported || d.Utilization != nil || d.Error != "" {
					t.Fatalf("normal empty utilization became a failure or observed zero: %+v", d)
				}
				if active && (len(d.Contexts) != 1 || d.Contexts[0].UsedMemoryBytes == nil || *d.Contexts[0].UsedMemoryBytes != 1024) {
					t.Fatalf("empty utilization erased active process evidence: %+v", d)
				}
				if !active && len(d.Contexts) != 0 {
					t.Fatalf("empty inventories gained invented contexts: %+v", d)
				}
			}
		})
	}
}

func TestMIGAndMPSDenyCompleteAttribution(t *testing.T) {
	b := fixtureBackend()
	b.mps = processQuery(processFixture(42, 100))
	if d := b.collectDevice(1, testUUID, 0); d.AttributionComplete || !strings.Contains(d.Error, "MPS") {
		t.Fatal("MPS ambiguity hidden")
	}
	b.mps = nil
	if d := b.collectDevice(1, testUUID, 0); !d.AttributionComplete || !strings.Contains(d.Error, "MPS inventory unsupported") {
		t.Fatal("optional unsupported MPS query invalidated complete core inventories")
	}
	b.mps = processQuery()
	b.mig = func(_ uintptr, current, _ *uint32) int32 { *current = 1; return nvSuccess }
	if d := b.collectDevice(1, testUUID, 0); d.AttributionComplete || !strings.Contains(d.Error, "MIG") {
		t.Fatal("MIG ambiguity hidden")
	}
	b.mig = nil
	if d := b.collectDevice(1, testUUID, 0); d.AttributionComplete {
		t.Fatal("unknown MIG capability considered complete")
	}
}

func TestInventoryFailureAndBounds(t *testing.T) {
	b := fixtureBackend()
	b.count = func(n *uint32) int32 { *n = 2; return nvSuccess }
	b.device = func(i uint32, h *uintptr) int32 {
		if i == 1 {
			return nvGPULost
		}
		*h = 1
		return nvSuccess
	}
	s, err := b.collect(0)
	if err == nil || len(s.Devices) != 2 || !strings.Contains(s.Devices[1].Error, "GPU lost") {
		t.Fatal("partial enumeration hidden")
	}
	b.device = func(_ uint32, h *uintptr) int32 { *h = 1; return nvSuccess }
	b.uuid = func(_ uintptr, output *byte, n uint32) int32 {
		copy(unsafe.Slice(output, int(n)), "private/process/path\x00")
		return nvSuccess
	}
	s, err = b.collect(0)
	if err == nil || strings.Contains(s.Devices[0].Error, "private") || s.Devices[0].UUID != "" {
		t.Fatal("invalid identity leaked")
	}
	calls := 0
	_, code := readProcesses(func(_ uintptr, n *uint32, _ *processInfoV2) int32 {
		calls++
		*n = maxEntries + 1
		return nvInsufficientSize
	}, 1)
	if code != nvInvalidData || calls != 1 {
		t.Fatal("oversized inventory not bounded")
	}
	calls = 0
	_, code = readProcesses(func(_ uintptr, n *uint32, _ *processInfoV2) int32 { calls++; *n = 1; return nvInsufficientSize }, 1)
	if code != nvInsufficientSize || calls != 4 {
		t.Fatal("changing inventory retries not bounded")
	}
}

type blockingBackend struct {
	started chan struct{}
	release chan struct{}
	closed  chan struct{}
	calls   atomic.Int32
}

func (b *blockingBackend) collect(uint64) (Sample, error) {
	b.calls.Add(1)
	close(b.started)
	<-b.release
	return Sample{CollectedAt: time.Now()}, nil
}
func (b *blockingBackend) close() error { close(b.closed); return nil }

func TestCancellationDoesNotAccumulateNativeCalls(t *testing.T) {
	b := &blockingBackend{started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(b.release) })
	r := &Reader{open: func() (backend, error) { return b, nil }}
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() { _, err := r.Query(ctx, 0); returned <- err }()
	select {
	case <-b.started:
	case <-time.After(time.Second):
		t.Fatal("native call did not start")
	}
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("request cancellation not returned")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request remained blocked")
	}
	for range 100 {
		if _, err := r.Query(context.Background(), 0); !errors.Is(err, ErrBusy) {
			t.Fatal("inflight native work not rejected")
		}
	}
	other := NewReader()
	defer other.Close()
	if _, err := other.Query(context.Background(), 0); !errors.Is(err, ErrBusy) {
		t.Fatal("another reader bypassed global native bound")
	}
	if b.calls.Load() != 1 {
		t.Fatal("native calls accumulated after cancellation")
	}
	_ = r.Close()
	select {
	case <-b.closed:
		t.Fatal("library closed with an inflight call")
	default:
	}
	release.Do(func() { close(b.release) })
	select {
	case <-b.closed:
	case <-time.After(time.Second):
		t.Fatal("deferred close did not run")
	}
	// Synchronize with finish releasing the global slot, avoiding test leakage.
	select {
	case nativeSlot <- struct{}{}:
		<-nativeSlot
	case <-time.After(time.Second):
		t.Fatal("native slot not released")
	}
	if _, err := r.Query(context.Background(), 0); !errors.Is(err, ErrClosed) {
		t.Fatal("closed reader accepted query")
	}
}

func TestInvalidProcessInventoryCannotClaimCompleteness(t *testing.T) {
	b := fixtureBackend()
	b.compute = processQuery(processFixture(0, 42))
	d := b.collectDevice(1, testUUID, 0)
	if d.ComputeSupported || d.AttributionComplete || len(d.Contexts) != 0 || !strings.Contains(d.Error, "invalid data") {
		t.Fatal("invalid process record accepted as complete inventory")
	}
	b = fixtureBackend()
	b.count = func(n *uint32) int32 { *n = 2; return nvSuccess }
	sample, err := b.collect(0)
	if err == nil || len(sample.Devices) != 2 || sample.Devices[1].UUID != "" || sample.Devices[1].Error == "" {
		t.Fatal("duplicate device identity accepted")
	}
}

func TestExpiredDeadlineNeverStartsNativeWork(t *testing.T) {
	var opened atomic.Int32
	r := &Reader{open: func() (backend, error) { opened.Add(1); return fixtureBackend(), nil }}
	defer r.Close()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := r.Query(ctx, 0); !errors.Is(err, context.DeadlineExceeded) || opened.Load() != 0 {
		t.Fatal("expired deadline started native work or lost timeout error")
	}
}

func TestPascalUnsupportedMPSPreservesCoreInventory(t *testing.T) {
	for _, active := range []bool{false, true} {
		b := fixtureBackend()
		b.mps = func(_ uintptr, _ *uint32, _ *processInfoV2) int32 { return nvNotSupported }
		b.mig = func(_ uintptr, _, _ *uint32) int32 { return nvNotSupported }
		if active {
			// A pre-Volta MPS server remains visible in the core inventory;
			// absent optional MPS details must not erase its context or memory.
			b.compute = processQuery(processFixture(42, 1024))
		}
		d := b.collectDevice(1, testUUID, 0)
		if !d.AttributionComplete || !d.ComputeSupported || !d.GraphicsSupported || !strings.Contains(d.Error, "MPS inventory unsupported") {
			t.Fatal("Pascal optional query failure invalidated core context evidence")
		}
		if active {
			if len(d.Contexts) != 1 || d.Contexts[0].UsedMemoryBytes == nil || *d.Contexts[0].UsedMemoryBytes != 1024 {
				t.Fatal("visible MPS server context or memory lost")
			}
		} else if len(d.Contexts) != 0 {
			t.Fatal("complete empty inventories gained invented contexts")
		}
	}
	for _, code := range []int32{nvNotFound, nvGPULost, nvInvalidData} {
		b := fixtureBackend()
		b.mps = func(_ uintptr, _ *uint32, _ *processInfoV2) int32 { return code }
		if d := b.collectDevice(1, testUUID, 0); d.AttributionComplete || d.Error == "" {
			t.Fatal("non-unsupported MPS failure lost conservative unknown state")
		}
	}
}

func TestUtilizationActivityIncludesEveryEngine(t *testing.T) {
	for _, test := range []struct {
		name   string
		sample processUtilization
		active bool
	}{
		{"zero", processUtilization{}, false},
		{"sm", processUtilization{SM: 1}, true},
		{"memory", processUtilization{Memory: 1}, true},
		{"encoder", processUtilization{Encoder: 1}, true},
		{"decoder", processUtilization{Decoder: 1}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := fixtureBackend()
			test.sample.PID = 42
			test.sample.Timestamp = 100
			b.utilization = utilizationQuery(test.sample)
			out, code := b.readUtilization(1, 99)
			if code != nvSuccess || len(out) != 1 || out[0].Activity != test.active || out[0].SMPercent != float64(test.sample.SM) {
				t.Fatal("non-SM engine activity lost or SM metric changed")
			}
		})
	}
	for _, records := range [][]processUtilization{
		{{PID: 42, Timestamp: 100, Encoder: 1}, {PID: 42, Timestamp: 100}},
		{{PID: 42, Timestamp: 100}, {PID: 42, Timestamp: 100, Decoder: 1}},
	} {
		b := fixtureBackend()
		b.utilization = utilizationQuery(records...)
		out, code := b.readUtilization(1, 99)
		if code != nvSuccess || len(out) != 1 || !out[0].Activity || out[0].SMPercent != 0 {
			t.Fatal("duplicate samples lost non-SM activity evidence")
		}
	}
}
