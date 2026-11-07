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

func fixtureSharedControlEngine(t *testing.T) (*Engine, *fixtureReader, []Instance, string) {
	t.Helper()
	now := time.Unix(1800000000, 0)
	root := t.TempDir()
	for _, item := range []struct {
		pid   int
		group string
	}{{101, "/instances/1"}, {201, "/instances/10"}} {
		fixtureProcess(t, root, item.pid, item.group+"/init.scope", uint64(item.pid))
		mount := "31 25 0:27 " + item.group + " /sys/fs/cgroup rw - cgroup2 cgroup rw\n"
		if err := os.WriteFile(filepath.Join(root, strconv.Itoa(item.pid), "mountinfo"), []byte(mount), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixtureProcess(t, root, 102, "/instances/1/system.slice/docker.service/docker/nested", 12)
	// Nested Docker PID namespaces differ; cgroup ancestry remains authoritative.
	for _, pid := range []int{101, 102} {
		base := filepath.Join(root, strconv.Itoa(pid), "ns")
		if err := os.MkdirAll(base, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(fmt.Sprintf("pid:[%d]", pid), filepath.Join(base, "pid")); err != nil {
			t.Fatal(err)
		}
	}
	memory := uint64(70)
	card := completeDevice("GPU-a", now, gpu.Process{PID: 102, UsedMemoryBytes: &memory})
	card.Utilization[0].SMPercent = 37
	reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{card, completeDevice("GPU-old", now)}}}
	engine := fixtureEngine(t, root, reader, &now)
	if err := os.MkdirAll(filepath.Join(root, "devices"), 0700); err != nil {
		t.Fatal(err)
	}
	for index, device := range []struct {
		path string
		leaf string
	}{{"/dev/null", "nvidia0"}, {"/dev/zero", "nvidiactl"}, {"/dev/random", "nvidia-uvm"}} {
		info, err := os.Stat(device.path)
		if err != nil {
			t.Fatal(err)
		}
		number, ok := deviceNumber(info)
		if !ok {
			t.Fatal("missing fixture device identity")
		}
		if index == 0 {
			engine.devices[number] = "GPU-a"
		} else {
			engine.controlDevices[number] = true
		}
		alias := filepath.Join(root, "devices", device.leaf)
		if err := os.Symlink(device.path, alias); err != nil {
			t.Fatal(err)
		}
		// Duplicate FDs must count once per process, for physical and shared nodes.
		for _, fd := range []int{index*2 + 4, index*2 + 5} {
			if err := os.Symlink(alias, filepath.Join(root, "102", "fd", strconv.Itoa(fd))); err != nil {
				t.Fatal(err)
			}
		}
	}
	inputs := []Instance{
		{Project: "p", Name: "one", UUID: "one", Type: "container", State: "Running", InitPID: 101, ConfiguredGPUs: []string{"GPU-a", "GPU-old"}},
		{Project: "p", Name: "ten", UUID: "ten", Type: "container", State: "Running", InitPID: 201},
	}
	return engine, reader, inputs, root
}

func TestQualityContextAttributionJSON(t *testing.T) {
	for _, mode := range []string{"complete", "shared", "metric", "unknown"} {
		q := quality()
		switch mode {
		case "shared":
			addSharedReferenceReason(&q)
		case "metric":
			addMetricReason(&q, "gpu_utilization_unknown")
		case "unknown":
			addReason(&q, "gpu_references_unresolved")
		}
		payload, err := json.Marshal(q)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatal(err)
		}
		flag, present := fields["context_attribution_complete"].(bool)
		if !present || flag != (mode != "unknown") {
			t.Fatalf("context flag missing or incorrectly serialized: %s", payload)
		}
		merged := quality()
		mergeQuality(&merged, q)
		if merged.ContextAttributionComplete != q.ContextAttributionComplete || merged.AttributionComplete != q.AttributionComplete {
			t.Fatalf("quality merge collapsed independent evidence: %+v", merged)
		}
	}
	var legacy Quality
	if err := json.Unmarshal([]byte(`{"complete":true,"attribution_complete":true,"reasons":[]}`), &legacy); err != nil || legacy.ContextAttributionComplete {
		t.Fatal("missing context field was invented")
	}
}

func TestGPUFDIdentityUsesRdevNotContainerName(t *testing.T) {
	requireLinux(t)
	engine, _, inputs, _ := fixtureSharedControlEngine(t)
	info, err := os.Stat("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	number, ok := deviceNumber(info)
	if !ok {
		t.Fatal("missing fixture device identity")
	}
	engine.devices[number] = "GPU-old" // The container alias remains nvidia0.
	proc := engine.scanProcesses(context.Background(), inputs)
	if proc.references[0]["GPU-old"] != 1 || proc.references[0]["GPU-a"] != 0 || proc.sharedReferences[0] != 1 || proc.unresolved != 0 {
		t.Fatalf("container filename overrode trusted rdev mapping: %+v", proc)
	}
}

func TestVerifiedGPUContextsSurviveSharedControlReferences(t *testing.T) {
	requireLinux(t)
	for _, otherGPU := range []bool{false, true} {
		t.Run(strconv.FormatBool(otherGPU), func(t *testing.T) {
			engine, reader, inputs, root := fixtureSharedControlEngine(t)
			if otherGPU {
				inputs[1].ConfiguredGPUs = []string{"GPU-old"}
			}
			proc := engine.scanProcesses(context.Background(), inputs)
			if !proc.complete || !proc.referencesComplete || proc.unresolved != 0 || proc.sharedReferences[0] != 1 || len(proc.sharedReferences) != 1 || proc.references[0]["GPU-a"] != 1 {
				t.Fatalf("known shared references lost owner or FD deduplication: %+v", proc)
			}
			engine.Sample(context.Background(), inputs)
			snapshot := engine.Sample(context.Background(), inputs)
			row, other := snapshot.Instances[0], snapshot.Instances[1]
			card := row.GPUs[0]
			if row.GPUState != "in_use" || card.UUID != "GPU-a" || card.ContextCount == nil || *card.ContextCount != 1 || card.UsedMemoryBytes == nil || *card.UsedMemoryBytes != 70 || card.SMUtilizationPercent == nil || *card.SMUtilizationPercent != 37 {
				t.Fatalf("shared references erased verified usage: %+v", card)
			}
			for _, q := range []Quality{snapshot.Quality, snapshot.HostQuality, row.Quality, row.GPUs[0].Quality, row.GPUs[1].Quality} {
				reasons := "," + strings.Join(q.Reasons, ",") + ","
				if q.Complete || q.AttributionComplete || !q.ContextAttributionComplete || !strings.Contains(reasons, ",gpu_shared_control_references,") || strings.Contains(reasons, ",gpu_references_unresolved,") || strings.Contains(reasons, ",gpu_reference_scan_incomplete,") {
					t.Fatalf("owned shared references misclassified: %+v", q)
				}
			}
			if card.ReferencingProcessCount != nil || row.GPUs[1].ReferencingProcessCount != nil || row.GPUs[1].State != "unknown" {
				t.Fatal("physical FD incorrectly restricted shared reference scope to one card")
			}
			if snapshot.Anonymous != (AnonymousCounts{}) || !other.Quality.AttributionComplete || !other.Quality.ContextAttributionComplete || strings.Contains(strings.Join(other.Quality.Reasons, ","), "gpu_shared_control_references") {
				t.Fatalf("shared owner poisoned unrelated instance: %+v", snapshot)
			}
			if otherGPU {
				if other.GPUState != "idle_candidate" || len(other.GPUs) != 1 || !other.GPUs[0].Quality.AttributionComplete || !other.GPUs[0].Quality.ContextAttributionComplete || other.GPUs[0].ContextCount == nil || *other.GPUs[0].ContextCount != 0 {
					t.Fatalf("unrelated card evidence was downgraded: %+v", other)
				}
			} else if other.GPUState != "not_attached" || len(other.GPUs) != 0 {
				t.Fatalf("unrelated no-GPU instance was downgraded: %+v", other)
			}
			report, err := json.Marshal(map[string]any{
				"owner_context_complete": row.Quality.ContextAttributionComplete,
				"owner_full_complete":    row.Quality.AttributionComplete,
				"owner_process_count":    card.ContextCount,
				"other_context_complete": other.Quality.ContextAttributionComplete,
				"other_full_complete":    other.Quality.AttributionComplete,
				"anonymous_references":   snapshot.Anonymous.UnresolvedGPUReferences,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Log(string(report))
			for _, fd := range []string{"4", "5"} {
				if err := os.Remove(filepath.Join(root, "102", "fd", fd)); err != nil {
					t.Fatal(err)
				}
			}
			reader.sample.Devices[0].Contexts = nil
			reader.sample.Devices[0].Utilization = nil
			row = engine.Sample(context.Background(), inputs).Instances[0]
			if row.GPUState != "unknown" || row.Quality.AttributionComplete || !row.Quality.ContextAttributionComplete {
				t.Fatalf("shared-only holder permitted idle release: %+v", row)
			}
			for _, card := range row.GPUs {
				if card.State != "unknown" || card.ReferencingProcessCount != nil || card.Quality.AttributionComplete {
					t.Fatalf("shared-only holder permitted per-card release: %+v", card)
				}
			}
			inputs[0].ConfiguredGPUs = nil
			row = engine.Sample(context.Background(), inputs).Instances[0]
			if row.GPUState != "unknown" || len(row.GPUs) != 0 || row.Quality.AttributionComplete || !row.Quality.ContextAttributionComplete {
				t.Fatalf("unconfigured shared-only holder became not_attached: %+v", row)
			}
		})
	}
}

func TestSharedControlFailuresStayGlobalUnknown(t *testing.T) {
	requireLinux(t)
	for _, mode := range []string{"unknown_rdev", "ambiguous_rdev", "fd_read", "fd_permission", "process_limit", "fd_limit", "pid_reuse", "cgroup_move", "init_reuse", "ambiguous_cgroup"} {
		t.Run(mode, func(t *testing.T) {
			engine, reader, inputs, root := fixtureSharedControlEngine(t)
			reader.sample.Devices[0].Contexts = nil
			reader.sample.Devices[0].Utilization = nil
			switch mode {
			case "unknown_rdev", "ambiguous_rdev":
				info, err := os.Stat("/dev/random")
				if err != nil {
					t.Fatal(err)
				}
				number, ok := deviceNumber(info)
				if !ok {
					t.Fatal("missing fixture device identity")
				}
				delete(engine.controlDevices, number)
				if mode == "ambiguous_rdev" {
					engine.devices[number] = ""
				}
			case "fd_read":
				if err := os.Remove(filepath.Join(root, "devices", "nvidia-uvm")); err != nil {
					t.Fatal(err)
				}
			case "fd_permission":
				if os.Geteuid() == 0 {
					t.Skip("root bypasses fixture permissions")
				}
				if err := os.Chmod(filepath.Join(root, "102", "fd"), 0000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "102", "fd"), 0700) })
			case "process_limit":
				engine.maxProcesses = 1
			case "fd_limit":
				engine.maxFDs = 1
			case "pid_reuse":
				reader.hook = func() { fixtureProcess(t, root, 102, "/instances/1/system.slice/docker.service/docker/nested", 99) }
			case "cgroup_move":
				reader.hook = func() { fixtureProcess(t, root, 102, "/instances/10/moved", 12) }
			case "init_reuse":
				reader.hook = func() { fixtureProcess(t, root, 101, "/instances/1/init.scope", 99) }
			case "ambiguous_cgroup":
				fixtureProcess(t, root, 301, "/instances/1/system.slice", 31)
				inputs = append(inputs, Instance{Project: "p", Name: "ambiguous", UUID: "ambiguous", Type: "container", State: "Running", InitPID: 301})
			}
			snapshot := engine.Sample(context.Background(), inputs)
			for _, q := range []Quality{snapshot.Quality, snapshot.HostQuality, snapshot.Instances[0].Quality, snapshot.Instances[1].Quality} {
				if q.AttributionComplete || q.ContextAttributionComplete {
					t.Fatalf("unsafe evidence admitted context-only exception: %+v", snapshot)
				}
			}
			if snapshot.Instances[0].GPUState == "idle_candidate" || snapshot.Instances[1].GPUState != "unknown" {
				t.Fatalf("unknown evidence became idle/unattached: %+v", snapshot)
			}
			if (mode == "unknown_rdev" || mode == "ambiguous_rdev") && snapshot.Anonymous.UnresolvedGPUReferences == 0 {
				t.Fatal("unmapped references disappeared from anonymous accounting")
			}
		})
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

func TestGPUUtilizationNoSamplesClassification(t *testing.T) {
	requireLinux(t)
	for _, test := range []struct {
		name         string
		noSamples    bool
		active       bool
		failure      string
		otherFailure bool
	}{
		{name: "idle_no_samples", noSamples: true},
		{name: "idle_without_observation_status"},
		{name: "unsupported", failure: "process utilization unsupported"},
		{name: "active_missing_sample", noSamples: true, active: true},
		{name: "empty_utilization_with_other_failure", noSamples: true, failure: "MPS inventory unsupported"},
		{name: "other_device_failure", noSamples: true, otherFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(1800000000, 0)
			root := t.TempDir()
			fixtureProcess(t, root, 101, "/instances/1", 11)
			device := completeDevice("GPU-a", now)
			device.UtilizationSupported = false
			device.UtilizationNoSamples = test.noSamples
			device.Error = test.failure
			wantCount, wantMemory, wantState := 0, uint64(0), "idle_candidate"
			if test.active {
				fixtureProcess(t, root, 102, "/instances/1/child", 12)
				wantCount, wantMemory, wantState = 1, 5, "in_use"
				device.Contexts = []gpu.Process{{PID: 102, UsedMemoryBytes: &wantMemory}}
			}
			reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{device}}}
			if test.otherFailure {
				other := completeDevice("GPU-old", now)
				other.UtilizationSupported = false
				other.Error = "process utilization unsupported"
				reader.sample.Devices = append(reader.sample.Devices, other)
			}
			snapshot := fixtureEngine(t, root, reader, &now).Sample(context.Background(), []Instance{{Project: "p", Name: "a", Type: "container", State: "Running", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}})
			row := snapshot.Instances[0]
			card := row.GPUs[0]
			if row.GPUState != wantState || card.State != wantState || card.ContextCount == nil || *card.ContextCount != wantCount || card.ReferencingProcessCount == nil || *card.ReferencingProcessCount != 0 || card.UsedMemoryBytes == nil || *card.UsedMemoryBytes != wantMemory || card.SMUtilizationPercent != nil {
				t.Fatalf("empty utilization changed context evidence or fabricated utilization: %+v", row)
			}
			for index, q := range []Quality{snapshot.Quality, snapshot.HostQuality, row.Quality, card.Quality} {
				reasons := "," + strings.Join(q.Reasons, ",") + ","
				hostSummary := index < 2
				wantNoSamples := test.noSamples && !test.active && test.failure == ""
				wantOtherFailure := hostSummary && test.otherFailure
				if q.Complete || !q.AttributionComplete || strings.Contains(reasons, ",gpu_utilization_no_samples,") != wantNoSamples || strings.Contains(reasons, ",gpu_utilization_unknown,") != (!wantNoSamples || wantOtherFailure) || strings.Contains(reasons, ",gpu_metric_unavailable,") != (test.failure != "" || wantOtherFailure) {
					t.Fatalf("empty utilization quality misclassified or leaked another device failure: %+v", q)
				}
			}
		})
	}
}

func TestMissingInstanceRootDistinguishesStoppedFromUnknown(t *testing.T) {
	requireLinux(t)
	for _, state := range []string{"Stopped", "stopped", "STOPPED", "Running", "running", "Frozen", ""} {
		t.Run(state, func(t *testing.T) {
			now := time.Unix(1800000000, 0)
			device := completeDevice("GPU-a", now)
			device.UtilizationSupported = false
			device.UtilizationNoSamples = true
			reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{device}}}
			snapshot := fixtureEngine(t, t.TempDir(), reader, &now).Sample(context.Background(), []Instance{{Project: "p", Name: "a", Type: "container", State: state, ConfiguredGPUs: []string{"GPU-a"}}})
			row := snapshot.Instances[0]
			card := row.GPUs[0]
			if row.GPUState != "unknown" || card.State != "unknown" || row.Quality.AttributionComplete || card.Quality.AttributionComplete || card.ContextCount != nil || card.ReferencingProcessCount != nil || card.UsedMemoryBytes != nil || card.SMUtilizationPercent != nil {
				t.Fatalf("missing root gained attribution, numbers, or idle authorization: %+v", row)
			}
			for _, q := range []Quality{snapshot.Quality, row.Quality, card.Quality} {
				reasons := "," + strings.Join(q.Reasons, ",") + ","
				stopped := strings.EqualFold(state, "stopped")
				if strings.Contains(reasons, ",instance_not_running,") != stopped || strings.Contains(reasons, ",instance_attribution_unknown,") == stopped {
					t.Fatalf("missing instance root state %q misclassified: %+v", state, q)
				}
			}
			if !snapshot.HostQuality.AttributionComplete || strings.Contains(strings.Join(snapshot.HostQuality.Reasons, ","), "instance_") {
				t.Fatalf("missing instance root polluted host evidence: %+v", snapshot.HostQuality)
			}
		})
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
				if row.GPUState != "unknown" || snapshot.Anonymous.UnresolvedGPUReferences != 0 || row.Quality.AttributionComplete || !row.Quality.ContextAttributionComplete {
					t.Fatalf("known shared FD pretended to be idle or anonymous: %+v", snapshot)
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
	device.UtilizationNoSamples = true
	metric = aggregateGPU(device, 0, owners, nil, 0, now, time.Second, now)
	if metric.SMUtilizationPercent != nil || !strings.Contains(strings.Join(metric.Quality.Reasons, ","), "gpu_utilization_unknown") || strings.Contains(strings.Join(metric.Quality.Reasons, ","), "gpu_utilization_no_samples") {
		t.Fatal("active context with missing sample became normal empty utilization or fabricated zero")
	}
	metric = aggregateGPU(device, 1, owners, nil, 0, now, time.Second, now)
	if metric.SMUtilizationPercent != nil || !strings.Contains(strings.Join(metric.Quality.Reasons, ","), "gpu_utilization_no_samples") || strings.Contains(strings.Join(metric.Quality.Reasons, ","), "gpu_utilization_unknown") {
		t.Fatal("verified empty owner inherited another owner's missing sample")
	}
}

func TestEmptyOwnerUtilizationRequiresWorkingSamplingCapability(t *testing.T) {
	now := time.Unix(1800000000, 0)
	memory := uint64(678 << 20)
	for _, mode := range []string{"other_owner_sample", "other_owner_missing_sample", "unsupported", "device_error", "unknown_capability", "empty_without_status", "explicit_no_samples"} {
		t.Run(mode, func(t *testing.T) {
			device := completeDevice("GPU-a", now, gpu.Process{PID: 7, UsedMemoryBytes: &memory})
			device.Utilization[0].SMPercent = 29
			wantNoSamples := false
			switch mode {
			case "other_owner_sample":
				wantNoSamples = true
			case "other_owner_missing_sample":
				device.Utilization = nil
				wantNoSamples = true
			case "unsupported":
				device.UtilizationSupported = false
			case "device_error":
				device.Error = "process utilization device lost"
			case "unknown_capability":
				device.UtilizationSupported = false
				device.Utilization = nil
			case "empty_without_status":
				device.Contexts = nil
				device.Utilization = nil
			case "explicit_no_samples":
				device.Contexts = nil
				device.Utilization = nil
				device.UtilizationSupported = false
				device.UtilizationNoSamples = true
				wantNoSamples = true
			}
			card := aggregateGPU(device, 1, map[uint32]int{7: 0}, nil, 0, now, time.Second, now)
			reasons := "," + strings.Join(card.Quality.Reasons, ",") + ","
			if card.SMUtilizationPercent != nil || card.ContextCount == nil || *card.ContextCount != 0 || card.UsedMemoryBytes == nil || *card.UsedMemoryBytes != 0 || card.ReferencingProcessCount == nil || *card.ReferencingProcessCount != 0 || card.Observed || !card.Quality.AttributionComplete || card.Quality.Complete {
				t.Fatalf("empty owner lost zero evidence or fabricated SM: %+v", card)
			}
			if strings.Contains(reasons, ",gpu_utilization_no_samples,") != wantNoSamples || strings.Contains(reasons, ",gpu_utilization_unknown,") == wantNoSamples || strings.Contains(reasons, ",gpu_metric_unavailable,") != (mode == "device_error") {
				t.Fatalf("sampling failure became normal empty owner: %+v", card)
			}
		})
	}
}

func TestPerOwnerMetricQualityDoesNotLeakAcrossCardsOrInstances(t *testing.T) {
	requireLinux(t)
	for _, mode := range []string{"idle_no_samples", "unsupported", "stale", "identity_unconfirmed", "same_instance_other_card", "unknown_context", "context_inventory", "reference_scan"} {
		t.Run(mode, func(t *testing.T) {
			engine, reader, inputs, root := fixtureSharedControlEngine(t)
			now := reader.sample.CollectedAt
			memory := uint64(678 << 20)
			reader.sample.Devices[0].Contexts[0].UsedMemoryBytes = &memory
			reader.sample.Devices[0].Utilization[0].SMPercent = 0 // Baseline must not invent anonymous activity.
			reader.sample.Devices[1].UtilizationNoSamples = true
			reader.sample.Devices[1].UtilizationSupported = false
			inputs[0].ConfiguredGPUs = []string{"GPU-a"}
			inputs[1].ConfiguredGPUs = []string{"GPU-a"}
			fixtureProcess(t, root, 301, "/instances/other", 31)
			inputs = append(inputs, Instance{Project: "p", Name: "other", UUID: "other", Type: "container", State: "Running", InitPID: 301, ConfiguredGPUs: []string{"GPU-old"}})
			if mode == "stale" {
				fixtureProcess(t, root, 302, "/instances/other/child", 32)
				reader.sample.Devices[1] = completeDevice("GPU-old", now, gpu.Process{PID: 302, UsedMemoryBytes: &memory})
			}
			engine.Sample(context.Background(), inputs)
			reader.sample.Devices[0].Utilization[0].SMPercent = 29
			wantReason := "gpu_utilization_no_samples"
			switch mode {
			case "unsupported":
				reader.sample.Devices[1].UtilizationNoSamples = false
				reader.sample.Devices[1].Error = "process utilization unsupported"
				wantReason = "gpu_metric_unavailable"
			case "stale":
				reader.sample.Devices[1].Utilization[0].TimestampMicros = uint64(now.Add(-time.Minute).UnixMicro())
				wantReason = "gpu_utilization_stale"
			case "identity_unconfirmed":
				fixtureProcess(t, root, 302, "/instances/other/child", 32)
				reader.sample.Devices[1] = completeDevice("GPU-old", now, gpu.Process{PID: 302, UsedMemoryBytes: &memory})
				wantReason = "gpu_utilization_identity_unconfirmed"
			case "same_instance_other_card":
				inputs[0].ConfiguredGPUs = []string{"GPU-a", "GPU-old"}
			case "unknown_context":
				reader.sample.Devices[1].Contexts = []gpu.Process{{PID: 999}}
				wantReason = "unattributed_gpu_contexts"
			case "context_inventory":
				reader.sample.Devices[1].AttributionComplete = false
				wantReason = "gpu_attribution_incomplete"
			case "reference_scan":
				if err := os.Remove(filepath.Join(root, "301", "fd")); err != nil {
					t.Fatal(err)
				}
				wantReason = "gpu_reference_scan_incomplete"
			}
			snapshot := engine.Sample(context.Background(), inputs)
			a, b, c := snapshot.Instances[0], snapshot.Instances[1], snapshot.Instances[2]
			structural := mode == "unknown_context" || mode == "context_inventory" || mode == "reference_scan"
			if structural {
				for _, row := range snapshot.Instances {
					if row.Quality.AttributionComplete || !strings.Contains(strings.Join(row.Quality.Reasons, ","), wantReason) {
						t.Fatalf("structural host uncertainty stopped failing closed: %+v", snapshot)
					}
					for _, card := range row.GPUs {
						if card.Quality.AttributionComplete || card.ReferencingProcessCount != nil {
							t.Fatalf("structural uncertainty permitted per-card release: %+v", card)
						}
					}
				}
				if snapshot.HostQuality.AttributionComplete {
					t.Fatalf("structural uncertainty disappeared from host quality: %+v", snapshot.HostQuality)
				}
				return
			}
			active, idle := a.GPUs[0], b.GPUs[0]
			if active.UUID != "GPU-a" || active.SMUtilizationPercent == nil || *active.SMUtilizationPercent != 29 || active.ContextCount == nil || *active.ContextCount != 1 || active.UsedMemoryBytes == nil || *active.UsedMemoryBytes != memory || active.ReferencingProcessCount != nil || active.State != "in_use" || active.Quality.AttributionComplete || !active.Quality.ContextAttributionComplete {
				t.Fatalf("shared controls or another card erased active owner's sample: %+v", a)
			}
			if a.Quality.AttributionComplete || !a.Quality.ContextAttributionComplete || !strings.Contains(strings.Join(active.Quality.Reasons, ","), "gpu_shared_control_references") {
				t.Fatalf("shared owner became release-authorized: %+v", a)
			}
			for _, reason := range []string{"gpu_utilization_no_samples", "gpu_utilization_unknown", "gpu_metric_unavailable", "gpu_utilization_stale", "gpu_utilization_identity_unconfirmed"} {
				if strings.Contains(strings.Join(active.Quality.Reasons, ","), reason) || (mode != "same_instance_other_card" && strings.Contains(strings.Join(a.Quality.Reasons, ","), reason)) {
					t.Fatalf("other card's metric warning leaked into active card/owner: %+v", a)
				}
			}
			if b.GPUState != "idle_candidate" || idle.State != "idle_candidate" || idle.Observed || idle.ContextCount == nil || *idle.ContextCount != 0 || idle.ReferencingProcessCount == nil || *idle.ReferencingProcessCount != 0 || idle.UsedMemoryBytes == nil || *idle.UsedMemoryBytes != 0 || idle.SMUtilizationPercent != nil {
				t.Fatalf("other owner's activity changed verified empty owner: %+v", b)
			}
			for _, q := range []Quality{b.Quality, idle.Quality} {
				if !q.AttributionComplete || !q.ContextAttributionComplete || q.Complete || len(q.Reasons) != 1 || q.Reasons[0] != "gpu_utilization_no_samples" {
					t.Fatalf("empty owner inherited active/shared/other-card warnings: %+v", b)
				}
			}
			if !c.Quality.AttributionComplete || !strings.Contains(strings.Join(c.GPUs[0].Quality.Reasons, ","), wantReason) || !strings.Contains(strings.Join(c.Quality.Reasons, ","), wantReason) || !strings.Contains(strings.Join(snapshot.Quality.Reasons, ","), wantReason) {
				t.Fatalf("local metric warning vanished instead of being scoped: %+v", snapshot)
			}
			if !snapshot.HostQuality.ContextAttributionComplete || snapshot.HostQuality.AttributionComplete || !strings.Contains(strings.Join(snapshot.HostQuality.Reasons, ","), "gpu_shared_control_references") {
				t.Fatalf("host summary lost shared-control safety: %+v", snapshot.HostQuality)
			}
			if mode != "identity_unconfirmed" && !strings.Contains(strings.Join(snapshot.HostQuality.Reasons, ","), wantReason) {
				t.Fatalf("device warning vanished from conservative host summary: %+v", snapshot.HostQuality)
			}
			if mode == "same_instance_other_card" && (len(a.GPUs) != 2 || !strings.Contains(strings.Join(a.Quality.Reasons, ","), "gpu_utilization_no_samples") || !strings.Contains(strings.Join(a.GPUs[1].Quality.Reasons, ","), "gpu_utilization_no_samples")) {
				t.Fatalf("instance did not aggregate its own other card warning: %+v", a)
			}
			if snapshot.Anonymous != (AnonymousCounts{}) {
				t.Fatalf("known independent owners became anonymous: %+v", snapshot.Anonymous)
			}
		})
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

func TestDelayedUtilizationRetainsContinuousProcessIdentity(t *testing.T) {
	requireLinux(t)
	firstObserved := time.Unix(1800000000, 0)
	now := firstObserved
	root := t.TempDir()
	fixtureProcess(t, root, 101, "/instances/1", 11)
	fixtureProcess(t, root, 102, "/instances/1/child", 12)
	memory := uint64(7)
	device := completeDevice("GPU-a", now, gpu.Process{PID: 102, UsedMemoryBytes: &memory})
	device.Utilization[0].SMPercent = 37
	reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{device}}}
	engine := fixtureEngine(t, root, reader, &now)
	inputs := []Instance{{Project: "p", Name: "a", UUID: "a", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}}
	collect := func() Snapshot {
		t.Helper()
		reader.sample.CollectedAt = now
		return engine.Sample(context.Background(), inputs)
	}
	if card := collect().Instances[0].GPUs[0]; card.SMUtilizationPercent != nil {
		t.Fatal("first observation admitted utilization before identity confirmation")
	}
	// NVML can deliver a sample older than the preceding collection, but newer
	// than the first observation of this uninterrupted identity.
	reader.sample.Devices[0].Utilization[0].TimestampMicros = uint64(firstObserved.Add(500 * time.Millisecond).UnixMicro())
	for round := 1; round <= 3; round++ {
		now = firstObserved.Add(time.Duration(round) * 2 * time.Second)
		snapshot := collect()
		card := snapshot.Instances[0].GPUs[0]
		if card.SMUtilizationPercent == nil || *card.SMUtilizationPercent != 37 || snapshot.Anonymous.UnattributedActivitySamples != 0 {
			t.Fatalf("round %d rejected delayed utilization for continuous identity: %+v, %+v", round, card, snapshot.Anonymous)
		}
	}
	reader.sample.Devices[0].Utilization[0].TimestampMicros = uint64(firstObserved.Add(-time.Microsecond).UnixMicro())
	snapshot := collect()
	card := snapshot.Instances[0].GPUs[0]
	if card.SMUtilizationPercent != nil || snapshot.Anonymous.UnattributedActivitySamples != 1 || !strings.Contains(strings.Join(card.Quality.Reasons, ","), "gpu_utilization_identity_unconfirmed") {
		t.Fatalf("sample before first identity observation was admitted: %+v, %+v", card, snapshot.Anonymous)
	}
	// Preserving identity must not extend the utilization freshness window.
	now = firstObserved.Add(engine.maxAge + time.Second)
	reader.sample.Devices[0].Utilization[0].TimestampMicros = uint64(firstObserved.Add(500 * time.Millisecond).UnixMicro())
	snapshot = collect()
	card = snapshot.Instances[0].GPUs[0]
	if card.SMUtilizationPercent != nil || !strings.Contains(strings.Join(snapshot.Quality.Reasons, ","), "gpu_utilization_stale") {
		t.Fatalf("continuous identity admitted an expired sample: %+v", snapshot)
	}
	reader.sample.Devices[0].Utilization = nil
	if card := collect().Instances[0].GPUs[0]; card.SMUtilizationPercent != nil {
		t.Fatalf("missing NVML sample reused earlier utilization: %+v", card)
	}
}

func TestDelayedUtilizationResetsAfterIdentityChangeOrDisappearance(t *testing.T) {
	requireLinux(t)
	for _, mode := range []string{"pid_reuse", "instance_identity", "instance_epoch", "context_disappeared"} {
		t.Run(mode, func(t *testing.T) {
			firstObserved := time.Unix(1800000000, 0)
			now := firstObserved
			root := t.TempDir()
			fixtureProcess(t, root, 101, "/instances/1", 11)
			fixtureProcess(t, root, 102, "/instances/1/child", 12)
			memory := uint64(7)
			process := gpu.Process{PID: 102, UsedMemoryBytes: &memory}
			device := completeDevice("GPU-a", now, process)
			device.Utilization[0].SMPercent = 37
			reader := &fixtureReader{sample: gpu.Sample{CollectedAt: now, Devices: []gpu.Device{device}}}
			engine := fixtureEngine(t, root, reader, &now)
			inputs := []Instance{{Project: "p", Name: "a", UUID: "a", Type: "container", InitPID: 101, ConfiguredGPUs: []string{"GPU-a"}}}
			collect := func() GPUMetrics {
				t.Helper()
				reader.sample.CollectedAt = now
				return engine.Sample(context.Background(), inputs).Instances[0].GPUs[0]
			}
			collect()
			reader.sample.Devices[0].Utilization[0].TimestampMicros = uint64(firstObserved.Add(500 * time.Millisecond).UnixMicro())
			for round := 1; round <= 2; round++ {
				now = firstObserved.Add(time.Duration(round) * time.Second)
				if card := collect(); card.SMUtilizationPercent == nil || *card.SMUtilizationPercent != 37 {
					t.Fatalf("continuous identity rejected delayed utilization: %+v", card)
				}
			}
			now = now.Add(time.Second)
			switch mode {
			case "pid_reuse":
				fixtureProcess(t, root, 102, "/instances/1/child", 99)
			case "instance_identity":
				inputs[0].UUID = "b"
			case "instance_epoch":
				fixtureProcess(t, root, 101, "/instances/1", 99)
			case "context_disappeared":
				reader.sample.Devices[0].Contexts = nil
				collect()
				reader.sample.Devices[0].Contexts = []gpu.Process{process}
				now = now.Add(time.Second)
			}
			identityObserved := now
			for round := 0; round <= 2; round++ {
				card := collect()
				if card.SMUtilizationPercent != nil || !strings.Contains(strings.Join(card.Quality.Reasons, ","), "gpu_utilization_identity_unconfirmed") {
					t.Fatalf("round %d inherited utilization from an interrupted identity: %+v", round, card)
				}
				now = now.Add(time.Second)
			}
			reader.sample.Devices[0].Utilization[0].TimestampMicros = uint64(identityObserved.Add(500 * time.Millisecond).UnixMicro())
			if card := collect(); card.SMUtilizationPercent == nil || *card.SMUtilizationPercent != 37 {
				t.Fatalf("new continuous identity rejected its own delayed sample: %+v", card)
			}
		})
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
	for _, mode := range []string{"complete", "optional_utilization_missing", "utilization_no_samples", "utilization_unsupported", "control_fd", "fd_scan", "anonymous_context", "anonymous_activity", "process_scan", "process_limit", "fd_limit", "instance_inventory", "device_inventory", "gpu_inventory", "mig_context_inventory", "cancelled", "query_failed", "stale"} {
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
			case "utilization_no_samples":
				reader.sample.Devices[0].Utilization = nil
				reader.sample.Devices[0].UtilizationSupported = false
				reader.sample.Devices[0].UtilizationNoSamples = true
				reason = "gpu_utilization_no_samples"
			case "utilization_unsupported":
				reader.sample.Devices[0].Utilization = nil
				reader.sample.Devices[0].UtilizationSupported = false
				reader.sample.Devices[0].Error = "process utilization unsupported"
				reason = "gpu_metric_unavailable"
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
				reason = "gpu_shared_control_references"
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
			wantHostAttribution := mode == "complete" || mode == "optional_utilization_missing" || mode == "utilization_no_samples" || mode == "utilization_unsupported"
			if snapshot.HostQuality.AttributionComplete != wantHostAttribution || snapshot.HostQuality.Complete != (mode == "complete") {
				t.Fatalf("host evidence was weakened or polluted by row quality: %s", payload)
			}
			if reason != "" && !strings.Contains(strings.Join(snapshot.HostQuality.Reasons, ","), reason) {
				t.Fatalf("host uncertainty reason %q missing: %s", reason, payload)
			}
			for _, hostReason := range snapshot.HostQuality.Reasons {
				if hostReason == "instance_attribution_unknown" || hostReason == "instance_not_running" {
					t.Fatalf("stopped row reason escaped into host evidence: %s", payload)
				}
			}
			stoppedReasons := "," + strings.Join(snapshot.Instances[1].Quality.Reasons, ",") + ","
			if !strings.Contains(stoppedReasons, ",instance_not_running,") || strings.Contains(stoppedReasons, ",instance_attribution_unknown,") || strings.Contains(strings.Join(snapshot.Instances[0].Quality.Reasons, ","), "instance_not_running") {
				t.Fatalf("stopped reason was misclassified or leaked to running instance: %s", payload)
			}
		})
	}
}
