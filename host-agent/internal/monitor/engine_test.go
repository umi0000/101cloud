package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"101cloud/hostagent/internal/monitor/gpu"
)

type fixtureReader struct {
	sample gpu.Sample
	err    error
	hook   func()
}

func (r *fixtureReader) Query(context.Context, uint64) (gpu.Sample, error) {
	if r.hook != nil {
		r.hook()
	}
	return r.sample, r.err
}

func fixtureProcess(t *testing.T, root string, pid int, group string, start uint64) {
	t.Helper()
	base := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(base, "fd"), 0700); err != nil {
		t.Fatal(err)
	}
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[19] = strconv.FormatUint(start, 10)
	stat := fmt.Sprintf("%d (secret command (with spaces)) %s\n", pid, strings.Join(fields, " "))
	if err := os.WriteFile(filepath.Join(base, "stat"), []byte(stat), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "cgroup"), []byte("0::"+group+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux host process attribution")
	}
}

func fixtureEngine(t *testing.T, root string, reader *fixtureReader, now *time.Time) *Engine {
	t.Helper()
	return NewEngine(Options{ProcRoot: root, Reader: reader, Now: func() time.Time { return *now }, Inventory: []GPUDevice{{UUID: "GPU-a", Rdevs: []uint64{123456}}, {UUID: "GPU-old", Rdevs: []uint64{123457}}}})
}

func completeDevice(uuid string, now time.Time, processes ...gpu.Process) gpu.Device {
	d := gpu.Device{UUID: uuid, Contexts: processes, ComputeSupported: true, GraphicsSupported: true, UtilizationSupported: true, AttributionComplete: true}
	for _, process := range processes {
		d.Utilization = append(d.Utilization, gpu.UtilizationSample{PID: process.PID, TimestampMicros: uint64(now.UnixMicro()), SMPercent: 0})
	}
	return d
}

func TestSharedGPUOwnershipAndOldGPU(t *testing.T) {
	requireLinux(t)
	now := time.Unix(1800000000, 0)
	root := t.TempDir()
	fixtureProcess(t, root, 101, "/instances/1", 11)
	fixtureProcess(t, root, 102, "/instances/1/docker/nested", 12)
	fixtureProcess(t, root, 201, "/instances/10", 21)
	fixtureProcess(t, root, 202, "/instances/10/child", 22)
	a, b := uint64(70), uint64(90)
	reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{
		completeDevice("GPU-a", now, gpu.Process{PID: 102, UsedMemoryBytes: &a}, gpu.Process{PID: 202, UsedMemoryBytes: &b}),
		completeDevice("GPU-old", now, gpu.Process{PID: 102, UsedMemoryBytes: &a}),
	}}}
	engine := fixtureEngine(t, root, reader, &now)
	inputs := []Instance{{Project: "alpha", Name: "same", UUID: "a", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}, {Project: "beta", Name: "same", UUID: "b", Type: "container", InitPID: 201, ConfiguredGPUs: []string{"GPU-a"}}}
	engine.Sample(context.Background(), inputs) // establish process identity, not utilization zero
	snapshot := engine.Sample(context.Background(), inputs)
	if snapshot.Anonymous.UnattributedContexts != 0 {
		t.Fatalf("unexpected anonymous contexts: %+v", snapshot.Anonymous)
	}
	if len(snapshot.Instances[0].GPUs) != 2 || len(snapshot.Instances[1].GPUs) != 1 {
		t.Fatalf("incorrect cgroup attribution: %+v", snapshot.Instances)
	}
	for i, memory := range []uint64{70, 90} {
		row := snapshot.Instances[i]
		if row.GPUState != "in_use" || !row.GPUs[0].Configured || !row.GPUs[0].Observed || row.GPUs[0].ContextCount == nil || *row.GPUs[0].ContextCount != 1 || row.GPUs[0].UsedMemoryBytes == nil || *row.GPUs[0].UsedMemoryBytes != memory {
			t.Fatalf("incorrect independent usage: %+v", row)
		}
		if row.GPUs[0].SMUtilizationPercent == nil || *row.GPUs[0].SMUtilizationPercent != 0 {
			t.Fatalf("zero utilization with context must remain in use: %+v", row)
		}
	}
	if snapshot.Instances[0].GPUs[1].UUID != "GPU-old" || snapshot.Instances[0].GPUs[1].Configured || !snapshot.Instances[0].GPUs[1].Observed {
		t.Fatal("old physical GPU disappeared after configuration change")
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret", "command", "pid", "PID", "/instances/", root, "starttime", "cgroup"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("privacy leak %q in %s", forbidden, payload)
		}
	}
	raw, err := json.Marshal(inputs[0])
	if err != nil || string(raw) != "{}" {
		t.Fatalf("raw input must not serialize: %s %v", raw, err)
	}
}

func TestSystemdSiblingScopesGPUOwnership(t *testing.T) {
	requireLinux(t)
	now := time.Unix(1800000000, 0)
	root := t.TempDir()
	for _, instance := range []struct {
		pid   int
		group string
	}{{101, "/instances/1"}, {201, "/instances/10"}} {
		fixtureProcess(t, root, instance.pid, instance.group+"/init.scope", uint64(instance.pid))
		fixtureProcess(t, root, instance.pid+1, instance.group+"/system.slice/workload.service", uint64(instance.pid+1))
		mount := "31 25 0:27 " + instance.group + " /sys/fs/cgroup rw - cgroup2 cgroup rw\n"
		if err := os.WriteFile(filepath.Join(root, strconv.Itoa(instance.pid), "mountinfo"), []byte(mount), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a, b := uint64(70), uint64(90)
	reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{
		completeDevice("GPU-a", now, gpu.Process{PID: 102, UsedMemoryBytes: &a}, gpu.Process{PID: 202, UsedMemoryBytes: &b}),
		completeDevice("GPU-old", now),
	}}}
	engine := fixtureEngine(t, root, reader, &now)
	inputs := []Instance{
		{Project: "p", Name: "one", UUID: "one", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}},
		{Project: "p", Name: "ten", UUID: "ten", Type: "container", InitPID: 201, ConfiguredGPUs: []string{"GPU-a"}},
	}
	engine.Sample(context.Background(), inputs)
	snapshot := engine.Sample(context.Background(), inputs)
	if snapshot.Anonymous.UnattributedContexts != 0 {
		t.Fatalf("sibling services lost ownership: %+v", snapshot.Anonymous)
	}
	for i, memory := range []uint64{a, b} {
		row := snapshot.Instances[i]
		if row.GPUState != "in_use" || len(row.GPUs) != 1 || row.GPUs[0].ContextCount == nil || *row.GPUs[0].ContextCount != 1 || row.GPUs[0].UsedMemoryBytes == nil || *row.GPUs[0].UsedMemoryBytes != memory {
			t.Fatalf("sibling scope crossed instance boundary: %+v", row)
		}
	}
}

func TestVerifiedGPUContextsSurviveUnknownControlReferences(t *testing.T) {
	requireLinux(t)
	now := time.Unix(1800000000, 0)
	root := t.TempDir()
	fixtureProcess(t, root, 101, "/instances/1", 11)
	fixtureProcess(t, root, 102, "/instances/1/workload", 12)
	if err := os.Symlink("/dev/null", filepath.Join(root, "102", "fd", "4")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	number, ok := deviceNumber(info)
	if !ok {
		t.Fatal("missing device identity")
	}
	memory := uint64(70)
	reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{
		completeDevice("GPU-a", now, gpu.Process{PID: 102, UsedMemoryBytes: &memory}),
	}}}
	engine := fixtureEngine(t, root, reader, &now)
	engine.controlDevices[number] = true
	inputs := []Instance{{Project: "p", Name: "one", UUID: "one", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}}
	engine.Sample(context.Background(), inputs)
	row := engine.Sample(context.Background(), inputs).Instances[0]
	card := row.GPUs[0]
	if row.GPUState != "in_use" || card.ContextCount == nil || *card.ContextCount != 1 || card.UsedMemoryBytes == nil || *card.UsedMemoryBytes != memory || card.SMUtilizationPercent == nil || *card.SMUtilizationPercent != 0 {
		t.Fatalf("unknown references erased verified usage: %+v", card)
	}
	if card.Quality.AttributionComplete || card.ReferencingProcessCount != nil {
		t.Fatalf("unknown control reference claimed complete attribution: %+v", card)
	}
	reader.sample.Devices[0].Contexts = nil
	reader.sample.Devices[0].Utilization = nil
	row = engine.Sample(context.Background(), inputs).Instances[0]
	if row.GPUState != "unknown" || row.GPUs[0].State != "unknown" || row.Quality.AttributionComplete {
		t.Fatalf("remaining control reference permitted idle release: %+v", row)
	}
}

func TestPIDReuseExitAndHostContextsAreUnknown(t *testing.T) {
	requireLinux(t)
	for _, mode := range []string{"reuse", "exit", "host", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(1800000000, 0)
			root := t.TempDir()
			fixtureProcess(t, root, 101, "/instances/1", 11)
			fixtureProcess(t, root, 102, "/instances/1/child", 12)
			reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{completeDevice("GPU-a", now, gpu.Process{PID: 102})}}}
			inputs := []Instance{{Project: "alpha", Name: "a", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}}
			switch mode {
			case "reuse":
				reader.hook = func() { fixtureProcess(t, root, 102, "/instances/1/child", 99) }
			case "exit":
				reader.hook = func() {
					if err := os.Remove(filepath.Join(root, "102", "stat")); err != nil {
						t.Fatal(err)
					}
				}
			case "host":
				fixtureProcess(t, root, 102, "/host", 12)
			case "ambiguous":
				fixtureProcess(t, root, 201, "/instances/1/child", 21)
				inputs = append(inputs, Instance{Project: "beta", Name: "b", Type: "container", InitPID: 201, ConfiguredGPUs: []string{"GPU-a"}})
			}
			snapshot := fixtureEngine(t, root, reader, &now).Sample(context.Background(), inputs)
			if snapshot.Anonymous.UnattributedContexts == 0 || snapshot.Instances[0].GPUState != "unknown" {
				t.Fatalf("%s must remain unknown: %+v", mode, snapshot)
			}
		})
	}
}

func TestUnknownEvidenceCannotBecomeIdle(t *testing.T) {
	requireLinux(t)
	for _, mode := range []string{"permission", "proc_permission", "fd_scan_error", "stale", "missing_cgroup", "provider_incomplete", "unresolved_configuration", "vm", "cancelled", "process_limit", "fd_limit"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(1800000000, 0)
			root := t.TempDir()
			fixtureProcess(t, root, 101, "/instances/1", 11)
			device := completeDevice("GPU-a", now)
			// A recent historical observation is not a current process inventory entry.
			device.Utilization = []gpu.UtilizationSample{{PID: 999, TimestampMicros: uint64(now.UnixMicro()), SMPercent: 0}}
			reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{device}}}
			input := Instance{Project: "p", Name: "a", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}
			engine := fixtureEngine(t, root, reader, &now)
			ctx := context.Background()
			switch mode {
			case "permission":
				reader.err = os.ErrPermission
			case "proc_permission":
				if os.Geteuid() == 0 {
					t.Skip("root bypasses fixture file permissions")
				}
				if err := os.Chmod(filepath.Join(root, "101", "cgroup"), 0000); err != nil {
					t.Fatal(err)
				}
			case "fd_scan_error":
				if err := os.Remove(filepath.Join(root, "101", "fd")); err != nil {
					t.Fatal(err)
				}
			case "stale":
				reader.sample.CollectedAt = now.Add(-time.Minute)
			case "missing_cgroup":
				if err := os.Remove(filepath.Join(root, "101", "cgroup")); err != nil {
					t.Fatal(err)
				}
			case "provider_incomplete":
				engine.SetCollectionComplete(false)
			case "unresolved_configuration":
				input.GPUConfigurationError = "gpu_configuration_unresolved"
				input.ConfiguredGPUs = nil
			case "vm":
				input.Type = "virtual-machine"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "process_limit":
				fixtureProcess(t, root, 102, "/instances/1/child", 12)
				engine.maxProcesses = 1
			case "fd_limit":
				engine.maxFDs = 0
			}
			row := engine.Sample(ctx, []Instance{input}).Instances[0]
			if row.GPUState != "unknown" {
				t.Fatalf("incomplete evidence became %s: %+v", row.GPUState, row)
			}
		})
	}
}

func TestIdleCandidateIsNotReleaseAuthorization(t *testing.T) {
	requireLinux(t)
	now := time.Unix(1800000000, 0)
	root := t.TempDir()
	fixtureProcess(t, root, 101, "/instances/1", 11)
	device := completeDevice("GPU-a", now)
	device.UtilizationSupported = false
	device.Error = "gpu_utilization_unsupported"
	reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{device}}}
	engine := fixtureEngine(t, root, reader, &now)
	row := engine.Sample(context.Background(), []Instance{{Project: "p", Name: "a", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}}).Instances[0]
	if row.GPUState != "idle_candidate" {
		t.Fatalf("complete point-in-time evidence: %+v", row)
	}
	if len(row.GPUs) != 1 {
		t.Fatalf("configured idle card missing from history: %+v", row)
	}
	card := row.GPUs[0]
	if !card.Configured || card.Observed || card.State != "idle_candidate" || card.ContextCount == nil || *card.ContextCount != 0 || card.ReferencingProcessCount == nil || *card.ReferencingProcessCount != 0 || card.UsedMemoryBytes == nil || *card.UsedMemoryBytes != 0 || card.SMUtilizationPercent != nil {
		t.Fatalf("idle evidence must be explicit without fabricating utilization: %+v", card)
	}
	if !row.Quality.AttributionComplete || row.Quality.Complete {
		t.Fatal("metric availability conflated with reference evidence")
	}
	reader.sample.Devices[0].UtilizationSupported = true
	reader.sample.Devices[0].Error = ""
	row = engine.Sample(context.Background(), []Instance{{Project: "p", Name: "a", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}}).Instances[0]
	if row.GPUState != "idle_candidate" {
		t.Fatal("complete empty context and FD inventories need no utilization sample")
	}
	engine.SetInventory(nil)
	if row = engine.Sample(context.Background(), []Instance{{Project: "p", Name: "a", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}}).Instances[0]; row.GPUState != "unknown" {
		t.Fatal("stale inventory survived refresh")
	}
}

func TestGPUFileDescriptorsAndGenericControl(t *testing.T) {
	requireLinux(t)
	for _, generic := range []bool{false, true} {
		t.Run(strconv.FormatBool(generic), func(t *testing.T) {
			now := time.Unix(1800000000, 0)
			root := t.TempDir()
			fixtureProcess(t, root, 101, "/instances/1", 11)
			fixtureProcess(t, root, 102, "/instances/1/docker", 12)
			if err := os.Symlink("/dev/null", filepath.Join(root, "102", "fd", "4")); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat("/dev/null")
			if err != nil {
				t.Fatal(err)
			}
			number, ok := deviceNumber(info)
			if !ok {
				t.Fatal("missing device identity")
			}
			reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{completeDevice("GPU-a", now), completeDevice("GPU-old", now)}}}
			engine := fixtureEngine(t, root, reader, &now)
			engine.SetInventory([]GPUDevice{{UUID: "GPU-a", Rdevs: []uint64{123456}}, {UUID: "GPU-old", Rdevs: []uint64{number}}})
			if generic {
				engine.controlDevices[number] = true
			}
			snapshot := engine.Sample(context.Background(), []Instance{{Project: "p", Name: "a", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}})
			row := snapshot.Instances[0]
			if generic {
				if row.GPUState != "unknown" || snapshot.Anonymous.UnresolvedGPUReferences == 0 {
					t.Fatalf("generic FD pretended to be unused: %+v", snapshot)
				}
			} else if row.GPUState != "in_use" || len(row.GPUs) != 2 || row.GPUs[1].UUID != "GPU-old" || row.GPUs[1].Configured || !row.GPUs[1].Observed || row.GPUs[1].ReferencingProcessCount == nil || *row.GPUs[1].ReferencingProcessCount != 1 {
				t.Fatalf("old GPU FD lost: %+v", row)
			}
		})
	}
}

func TestCPUCounterEpochAndImmutableSnapshot(t *testing.T) {
	now := time.Unix(1800000000, 0)
	reader := &fixtureReader{err: errors.New("unavailable")}
	engine := NewEngine(Options{Reader: reader, ProcRoot: t.TempDir(), Now: func() time.Time { return now }})
	usage, allocated, memory := uint64(100), uint64(2e9), uint64(1234)
	input := Instance{Project: "p", Name: "a", Type: "virtual-machine", StartedAt: now.Add(-time.Hour), CPUUsageNS: &usage, CPUAllocatedNS: &allocated, MemoryUsageBytes: &memory}
	first := engine.Sample(context.Background(), []Instance{input})
	if first.Instances[0].CPU.UsedCores != nil {
		t.Fatal("first sample fabricated CPU zero")
	}
	if first.Instances[0].CPU.DeltaNS != nil || first.Instances[0].CPU.WindowSeconds != nil {
		t.Fatal("first sample fabricated a CPU window")
	}
	if !first.Instances[0].StartedAt.Equal(input.StartedAt) {
		t.Fatal("runtime epoch missing from public DTO")
	}
	now = now.Add(time.Second)
	usage += 1e9
	second := engine.Sample(context.Background(), []Instance{input})
	cpu := second.Instances[0].CPU
	if cpu.UsedCores == nil || *cpu.UsedCores != 1 || cpu.AllocationPercent == nil || *cpu.AllocationPercent != 50 {
		t.Fatalf("wrong CPU delta: %+v", cpu)
	}
	if cpu.DeltaNS == nil || *cpu.DeltaNS != 1e9 || cpu.WindowSeconds == nil || *cpu.WindowSeconds != 1 {
		t.Fatalf("CPU window not explicit: %+v", cpu)
	}
	if *first.Instances[0].CPU.UsageNS != 100 {
		t.Fatal("previous snapshot changed with input counter")
	}
	memory = 9999
	if *first.Instances[0].Memory.UsageBytes != 1234 {
		t.Fatal("previous memory snapshot changed")
	}
	now = now.Add(time.Second)
	usage = 1
	if engine.Sample(context.Background(), []Instance{input}).Instances[0].CPU.UsedCores != nil {
		t.Fatal("negative delta fabricated zero")
	}
	now = now.Add(time.Second)
	usage = 10
	input.StartedAt = now
	if engine.Sample(context.Background(), []Instance{input}).Instances[0].CPU.UsedCores != nil {
		t.Fatal("restart reused old CPU baseline")
	}
	now = now.Add(time.Second)
	usage += 1e9
	engine.SetCollectionComplete(false)
	if engine.Sample(context.Background(), []Instance{input}).Instances[0].CPU.UsedCores != nil {
		t.Fatal("incomplete collection produced CPU rate")
	}
	now = now.Add(time.Second)
	usage += 1e9
	engine.SetCollectionComplete(true)
	if engine.Sample(context.Background(), []Instance{input}).Instances[0].CPU.UsedCores != nil {
		t.Fatal("CPU rate spanned unavailable collection window")
	}
}

func TestUnsupportedUtilizationRetainsMemoryEvidence(t *testing.T) {
	owners := map[uint32]int{7: 0}
	memory := uint64(5)
	now := time.Unix(1800000000, 0)
	device := completeDevice("GPU-a", now, gpu.Process{PID: 7, UsedMemoryBytes: &memory})
	device.UtilizationSupported = false
	metric := aggregateGPU(device, 0, owners, nil, 0, now, time.Second, now)
	if metric.SMUtilizationPercent != nil || metric.UsedMemoryBytes == nil || *metric.UsedMemoryBytes != 5 || metric.State != "in_use" {
		t.Fatalf("unsupported utilization lost process evidence: %+v", metric)
	}
	device.UtilizationSupported = true
	device.Utilization = nil
	metric = aggregateGPU(device, 0, owners, nil, 0, now, time.Second, now)
	if metric.SMUtilizationPercent != nil {
		t.Fatal("missing utilization sample fabricated zero")
	}
}

func TestUtilizationRequiresPriorProcessIdentityAndNewSample(t *testing.T) {
	requireLinux(t)
	now := time.Unix(1800000000, 0)
	root := t.TempDir()
	fixtureProcess(t, root, 101, "/instances/1", 11)
	fixtureProcess(t, root, 102, "/instances/1/child", 12)
	memory := uint64(7)
	device := completeDevice("GPU-a", now, gpu.Process{PID: 102, UsedMemoryBytes: &memory})
	device.Utilization[0].SMPercent = 37
	device.Utilization[0].TimestampMicros = uint64(now.Add(-time.Second).UnixMicro())
	reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{device}}}
	engine := fixtureEngine(t, root, reader, &now)
	inputs := []Instance{{Project: "p", Name: "a", UUID: "a", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}}
	collect := func() GPUMetrics {
		t.Helper()
		reader.sample.CollectedAt = now
		row := engine.Sample(context.Background(), inputs).Instances[0]
		if row.GPUState != "in_use" || len(row.GPUs) != 1 {
			t.Fatalf("process evidence lost: %+v", row)
		}
		return row.GPUs[0]
	}
	if card := collect(); card.SMUtilizationPercent != nil {
		t.Fatal("first observation trusted a cached PID sample")
	}
	now = now.Add(time.Second)
	reader.sample.Devices[0].Utilization[0].TimestampMicros = uint64(now.UnixMicro())
	if card := collect(); card.SMUtilizationPercent == nil || *card.SMUtilizationPercent != 37 {
		t.Fatalf("stable identity and new sample not admitted: %+v", card)
	}
	now = now.Add(time.Second)
	fixtureProcess(t, root, 102, "/instances/1/child", 99)
	if card := collect(); card.SMUtilizationPercent != nil {
		t.Fatal("PID reuse inherited historical utilization")
	}
	now = now.Add(time.Second)
	if card := collect(); card.SMUtilizationPercent != nil {
		t.Fatal("stable reused PID admitted a sample before its identity baseline")
	}
	now = now.Add(time.Second)
	reader.sample.Devices[0].Utilization[0].TimestampMicros = uint64(now.UnixMicro())
	if card := collect(); card.SMUtilizationPercent == nil || *card.SMUtilizationPercent != 37 {
		t.Fatalf("new identity never recovered fresh utilization: %+v", card)
	}
}

func TestPerCardQualityCannotClaimExactPartialAttribution(t *testing.T) {
	requireLinux(t)
	for _, mode := range []string{"scan", "owner", "missing_configured"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(1800000000, 0)
			root := t.TempDir()
			fixtureProcess(t, root, 101, "/instances/1", 11)
			fixtureProcess(t, root, 102, "/instances/1/child", 12)
			memory := uint64(7)
			device := completeDevice("GPU-a", now, gpu.Process{PID: 102, UsedMemoryBytes: &memory})
			input := Instance{Project: "p", Name: "a", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}
			switch mode {
			case "scan":
				fixtureProcess(t, root, 103, "/host", 13)
				if err := os.Remove(filepath.Join(root, "103", "cgroup")); err != nil {
					t.Fatal(err)
				}
			case "owner":
				device.Contexts = append(device.Contexts, gpu.Process{PID: 999})
			case "missing_configured":
				input.ConfiguredGPUs = []string{"GPU-missing"}
				device.Contexts = nil
			}
			reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{device}}}
			row := fixtureEngine(t, root, reader, &now).Sample(context.Background(), []Instance{input}).Instances[0]
			if len(row.GPUs) != 1 {
				t.Fatalf("configured/observed union is incomplete: %+v", row)
			}
			card := row.GPUs[0]
			if card.Quality.AttributionComplete || card.ContextCount != nil || card.ReferencingProcessCount != nil || card.UsedMemoryBytes != nil || card.SMUtilizationPercent != nil {
				t.Fatalf("partial evidence claimed exact numbers: %+v", card)
			}
			if mode == "missing_configured" {
				if !card.Configured || card.Observed || card.State != "unknown" {
					t.Fatalf("missing configured GPU not explicit: %+v", card)
				}
			} else if !card.Observed || card.State != "in_use" || row.GPUState != "in_use" {
				t.Fatalf("partial scan lost positive usage evidence: %+v", row)
			}
		})
	}
}

func TestCPUAllocationChangeDoesNotRewritePreviousWindowCapacity(t *testing.T) {
	now := time.Unix(1800000000, 0)
	reader := &fixtureReader{err: errors.New("unavailable")}
	engine := NewEngine(Options{Reader: reader, ProcRoot: t.TempDir(), Now: func() time.Time { return now }})
	usage, allocation := uint64(0), uint64(2e9)
	input := Instance{Project: "p", Name: "a", Type: "virtual-machine", StartedAt: now.Add(-time.Hour), CPUUsageNS: &usage, CPUAllocatedNS: &allocation}
	engine.Sample(context.Background(), []Instance{input})
	now = now.Add(time.Second)
	usage += 1e9
	allocation = 1e9
	cpu := engine.Sample(context.Background(), []Instance{input}).Instances[0].CPU
	if cpu.UsedCores == nil || *cpu.UsedCores != 1 || cpu.AllocationPercent != nil || cpu.DeltaNS == nil || *cpu.DeltaNS != 1e9 || cpu.WindowSeconds == nil || *cpu.WindowSeconds != 1 {
		t.Fatalf("capacity transition fabricated an allocation ratio: %+v", cpu)
	}
	now = now.Add(time.Second)
	usage += 1e9
	cpu = engine.Sample(context.Background(), []Instance{input}).Instances[0].CPU
	if cpu.AllocationPercent == nil || *cpu.AllocationPercent != 100 {
		t.Fatalf("stable new capacity did not recover percentage: %+v", cpu)
	}
}

func TestCPUObservationWindowIgnoresVariableGPUQueryDelay(t *testing.T) {
	observed := time.Date(2026, 10, 29, 12, 0, 0, 0, time.FixedZone("fixture", 8*60*60))
	now := observed
	delay := time.Second
	reader := &fixtureReader{err: errors.New("unavailable"), hook: func() { now = now.Add(delay) }}
	engine := NewEngine(Options{Reader: reader, ProcRoot: t.TempDir(), Now: func() time.Time { return now }})
	usage, allocation := uint64(0), uint64(1e9)
	input := Instance{Project: "p", Name: "a", Type: "virtual-machine", StartedAt: observed.Add(-time.Hour), CPUUsageNS: &usage, CPUAllocatedNS: &allocation, CPUCollectedAt: observed}
	first := engine.Sample(context.Background(), []Instance{input}).Instances[0].CPU
	if !first.CollectedAt.Equal(observed) || first.CollectedAt.Location() != time.UTC || first.WindowSeconds != nil {
		t.Fatalf("CPU observation timestamp not preserved in UTC: %+v", first)
	}
	now = observed.Add(5 * time.Second)
	input.CPUCollectedAt = now
	delay = 4 * time.Second
	usage += 5e9
	second := engine.Sample(context.Background(), []Instance{input}).Instances[0].CPU
	if !second.CollectedAt.Equal(input.CPUCollectedAt) || second.CollectedAt.Location() != time.UTC || second.WindowSeconds == nil || *second.WindowSeconds != 5 || second.DeltaNS == nil || *second.DeltaNS != 5e9 || second.UsedCores == nil || *second.UsedCores != 1 || second.AllocationPercent == nil || *second.AllocationPercent != 100 {
		t.Fatalf("GPU query delay contaminated CPU observation window: %+v", second)
	}
}

func TestRecentUnattributedActivityPreventsIdleAfterProcessExit(t *testing.T) {
	requireLinux(t)
	for _, mode := range []string{"short_lived", "previously_seen_exit", "video_only", "zero_activity"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(1800000000, 0)
			root := t.TempDir()
			fixtureProcess(t, root, 101, "/instances/1", 11)
			device := completeDevice("GPU-a", now)
			reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{device}}}
			engine := fixtureEngine(t, root, reader, &now)
			input := []Instance{{Project: "p", Name: "a", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}}
			if mode == "previously_seen_exit" {
				fixtureProcess(t, root, 102, "/instances/1/short", 12)
				reader.sample.Devices[0] = completeDevice("GPU-a", now, gpu.Process{PID: 102})
				engine.Sample(context.Background(), input)
				if err := os.RemoveAll(filepath.Join(root, "102")); err != nil {
					t.Fatal(err)
				}
				now = now.Add(time.Second)
			}
			// The process no longer has any enumerable context, but its activity
			// remains in NVML's recent ring. The video case has zero SM activity.
			reader.sample.CollectedAt = now
			reader.sample.Devices[0].Contexts = nil
			sample := gpu.UtilizationSample{PID: 102, TimestampMicros: uint64(now.UnixMicro()), SMPercent: 50}
			if mode == "video_only" {
				sample.SMPercent = 0
				sample.Activity = true
			}
			if mode == "zero_activity" {
				sample.SMPercent = 0
			}
			reader.sample.Devices[0].Utilization = []gpu.UtilizationSample{sample}
			snapshot := engine.Sample(context.Background(), input)
			row := snapshot.Instances[0]
			if mode == "zero_activity" {
				if snapshot.Anonymous.UnattributedActivitySamples != 0 || row.GPUState != "idle_candidate" {
					t.Fatalf("zero activity invented an anonymous reference: %+v", snapshot)
				}
				return
			}
			if snapshot.Anonymous.UnattributedActivitySamples != 1 || snapshot.Quality.AttributionComplete || row.GPUState != "unknown" || len(row.GPUs) != 1 {
				t.Fatalf("unowned recent activity became idle evidence: %+v", snapshot)
			}
			card := row.GPUs[0]
			if card.Observed || card.State != "unknown" || card.Quality.AttributionComplete || card.ContextCount != nil || card.ReferencingProcessCount != nil || card.UsedMemoryBytes != nil || card.SMUtilizationPercent != nil {
				t.Fatalf("exited process was fabricated into exact attribution: %+v", card)
			}
		})
	}
}

func TestFreshActivityNeedsStableIdentityEvenWithCurrentContext(t *testing.T) {
	requireLinux(t)
	now := time.Unix(1800000000, 0)
	root := t.TempDir()
	fixtureProcess(t, root, 101, "/instances/1", 11)
	fixtureProcess(t, root, 102, "/instances/1/child", 12)
	device := completeDevice("GPU-a", now, gpu.Process{PID: 102})
	device.Utilization[0].Activity = true // non-SM activity is still activity
	reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{device}}}
	engine := fixtureEngine(t, root, reader, &now)
	input := []Instance{{Project: "p", Name: "a", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}}
	first := engine.Sample(context.Background(), input)
	if first.Anonymous.UnattributedActivitySamples != 1 || first.Instances[0].GPUState != "in_use" {
		t.Fatalf("new identity did not preserve separate context and activity evidence: %+v", first)
	}
	now = now.Add(time.Second)
	reader.sample.CollectedAt = now
	reader.sample.Devices[0].Utilization[0].TimestampMicros = uint64(now.UnixMicro())
	second := engine.Sample(context.Background(), input)
	if second.Anonymous.UnattributedActivitySamples != 0 || !second.Quality.AttributionComplete || second.Instances[0].GPUState != "in_use" {
		t.Fatalf("stable activity was not attributed: %+v", second)
	}
}

func TestSnapshotHostQualitySeparatesStoppedRowsFromHostUncertainty(t *testing.T) {
	requireLinux(t)
	for _, mode := range []string{"complete", "optional_utilization_missing", "control_fd", "fd_scan", "anonymous_context", "anonymous_activity", "process_scan", "process_limit", "fd_limit", "instance_inventory", "device_inventory", "gpu_inventory", "mig_context_inventory", "cancelled", "query_failed", "stale"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(1800000000, 0)
			root := t.TempDir()
			fixtureProcess(t, root, 101, "/instances/live", 11)
			device := completeDevice("GPU-a", now)
			device.Utilization = []gpu.UtilizationSample{{PID: 999, TimestampMicros: uint64(now.UnixMicro())}}
			reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{device}}}
			engine := fixtureEngine(t, root, reader, &now)
			inputs := []Instance{
				{Project: "live-project", Name: "live", UUID: "live-id", Type: "container", State: "Running", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}},
				{Project: "other-project", Name: "unrelated-stopped", UUID: "stopped-id", Type: "container", State: "Stopped", ConfiguredGPUs: []string{"GPU-a"}},
			}
			ctx := context.Background()
			reason := ""
			switch mode {
			case "optional_utilization_missing":
				reader.sample.Devices[0].Utilization = nil
				reason = "gpu_utilization_unknown"
			case "control_fd":
				if err := os.Symlink("/dev/null", filepath.Join(root, "101", "fd", "4")); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat("/dev/null")
				if err != nil {
					t.Fatal(err)
				}
				number, ok := deviceNumber(info)
				if !ok {
					t.Fatal("missing control device identity")
				}
				engine.controlDevices[number] = true
				reason = "gpu_references_unresolved"
			case "fd_scan":
				if err := os.Remove(filepath.Join(root, "101", "fd")); err != nil {
					t.Fatal(err)
				}
				reason = "gpu_reference_scan_incomplete"
			case "anonymous_context":
				reader.sample.Devices[0].Contexts = []gpu.Process{{PID: 999}}
				reason = "unattributed_gpu_contexts"
			case "anonymous_activity":
				reader.sample.Devices[0].Utilization[0].Activity = true
				reason = "unattributed_gpu_activity"
			case "process_scan":
				fixtureProcess(t, root, 301, "/host", 31)
				if err := os.Remove(filepath.Join(root, "301", "cgroup")); err != nil {
					t.Fatal(err)
				}
				reason = "process_scan_incomplete"
			case "process_limit":
				engine.maxProcesses = 1
				reason = "process_scan_incomplete"
			case "fd_limit":
				engine.maxFDs = 0
				reason = "gpu_reference_scan_incomplete"
			case "instance_inventory":
				engine.SetCollectionComplete(false)
				reason = "instance_inventory_incomplete"
			case "device_inventory":
				engine.SetInventory(nil)
				reason = "device_inventory_incomplete"
			case "gpu_inventory":
				reader.sample.Devices = nil
				reason = "gpu_inventory_unavailable"
			case "mig_context_inventory":
				reader.sample.Devices[0].AttributionComplete = false
				reason = "gpu_attribution_incomplete"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				reason = "collection_cancelled"
			case "query_failed":
				reader.err = errors.New("unavailable")
				reason = "gpu_collection_failed"
			case "stale":
				reader.sample.CollectedAt = now.Add(-time.Minute)
				reason = "gpu_sample_stale"
			}
			payload, err := json.Marshal(engine.Sample(ctx, inputs))
			if err != nil {
				t.Fatal(err)
			}
			var snapshot Snapshot
			if err := json.Unmarshal(payload, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.Quality.AttributionComplete || snapshot.Instances[1].Quality.AttributionComplete || snapshot.Instances[1].GPUState != "unknown" {
				t.Fatalf("stopped instance uncertainty was erased: %s", payload)
			}
			wantHostAttribution := mode == "complete" || mode == "optional_utilization_missing"
			if snapshot.HostQuality.AttributionComplete != wantHostAttribution || snapshot.HostQuality.Complete != (mode == "complete") {
				t.Fatalf("host evidence was weakened or polluted by row quality: %s", payload)
			}
			if reason != "" && !strings.Contains(strings.Join(snapshot.HostQuality.Reasons, ","), reason) {
				t.Fatalf("host uncertainty reason %q missing: %s", reason, payload)
			}
			for _, hostReason := range snapshot.HostQuality.Reasons {
				if hostReason == "instance_attribution_unknown" {
					t.Fatalf("stopped row reason escaped into host evidence: %s", payload)
				}
			}
		})
	}
}
