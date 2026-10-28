package node

import (
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/lxc/incus/v6/shared/api"
)

const (
	telemetryFirstUUID  = "GPU-11111111-1111-1111-1111-111111111111"
	telemetrySecondUUID = "GPU-22222222-2222-2222-2222-222222222222"
)

func telemetryResources() *api.Resources {
	return &api.Resources{GPU: api.ResourcesGPU{Cards: []api.ResourcesGPUCard{
		{PCIAddress: "0000:01:00.0", Product: "first product", Nvidia: &api.ResourcesGPUCardNvidia{UUID: telemetryFirstUUID, Model: "first inventory model"}},
		{PCIAddress: "0000:41:00.0", Product: "second product", Nvidia: &api.ResourcesGPUCardNvidia{UUID: telemetrySecondUUID}},
	}}}
}

func requireTelemetryMetric(t *testing.T, actual *float64, expected float64) {
	t.Helper()
	if actual == nil {
		t.Fatalf("metric is nil, want %v", expected)
	}
	if math.Abs(*actual-expected) > 1e-9 {
		t.Fatalf("metric is %v, want %v", *actual, expected)
	}
}

func requireUnknownTelemetry(t *testing.T, gpu GPUInfo) {
	t.Helper()
	if gpu.Error == "" || gpu.UtilizationPercent != nil || gpu.MemoryUsedMiB != nil || gpu.MemoryTotalMiB != nil || gpu.MemoryUtilizationPercent != nil || gpu.LoadPercent != nil {
		t.Fatalf("unknown telemetry must retain an error and no invented metrics: %#v", gpu)
	}
}

func TestGPUFormulaAndUUIDMerge(t *testing.T) {
	// Unequal capacities distinguish the per-card arithmetic average from a
	// memory-capacity-weighted aggregate. Input order intentionally differs.
	output := strings.ToLower(telemetrySecondUUID) + ", Second live model, 80, 900, 1000\n" +
		telemetryFirstUUID + ", \"First model, with comma\", 20, 1, 10\n"
	samples := parseGPUOutput(output)
	gpus, average := mergeGPUInfo(telemetryResources(), samples, nil)
	if len(gpus) != 2 {
		t.Fatalf("got %d GPUs, want both inventory GPUs", len(gpus))
	}
	if gpus[0].UUID != telemetryFirstUUID || gpus[0].PCIAddress != "0000:01:00.0" || gpus[0].Model != "First model, with comma" {
		t.Fatalf("first GPU was not matched by UUID: %#v", gpus[0])
	}
	if gpus[1].UUID != telemetrySecondUUID || gpus[1].PCIAddress != "0000:41:00.0" || gpus[1].Model != "Second live model" {
		t.Fatalf("second GPU was not matched by UUID: %#v", gpus[1])
	}
	if gpus[0].Error != "" || gpus[1].Error != "" {
		t.Fatalf("valid telemetry returned errors: %#v", gpus)
	}
	requireTelemetryMetric(t, gpus[0].UtilizationPercent, 20)
	requireTelemetryMetric(t, gpus[0].MemoryUsedMiB, 1)
	requireTelemetryMetric(t, gpus[0].MemoryTotalMiB, 10)
	requireTelemetryMetric(t, gpus[0].MemoryUtilizationPercent, 10)
	requireTelemetryMetric(t, gpus[0].LoadPercent, 15)
	requireTelemetryMetric(t, gpus[1].LoadPercent, 85)
	requireTelemetryMetric(t, average, 50)
}

func TestGPUInvalidMetricsPreserveOtherCards(t *testing.T) {
	for _, test := range []struct {
		name string
		row  string
	}{
		{"unavailable_utilization", "Model, N/A, 1, 10"},
		{"unavailable_memory", "Model, 50, [N/A], 10"},
		{"unavailable_total", "Model, 50, 1, N/A"},
		{"nan_utilization", "Model, NaN, 1, 10"},
		{"nan_memory", "Model, 50, NaN, 10"},
		{"infinite_total", "Model, 50, 1, +Inf"},
		{"infinite_used", "Model, 50, Inf, 10"},
		{"overflow", "Model, 50, 1, 1e999"},
		{"negative_utilization", "Model, -1, 1, 10"},
		{"percentage_over_100", "Model, 101, 1, 10"},
		{"negative_memory", "Model, 50, -1, 10"},
		{"negative_total", "Model, 50, 0, -1"},
		{"zero_total", "Model, 50, 0, 0"},
		{"used_exceeds_total", "Model, 50, 11, 10"},
		{"empty_metric", "Model, , 1, 10"},
		{"units_not_allowed", "Model, 50 %, 1, 10"},
		{"missing_column", "Model, 50, 1"},
		{"extra_column", "Model, 50, 1, 10, extra"},
		{"broken_csv", "\"unclosed model, 50, 1, 10"},
		{"missing_model", ", 50, 1, 10"},
		{"unavailable_model", "N/A, 50, 1, 10"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := telemetryFirstUUID + ", " + test.row + "\n" + telemetrySecondUUID + ", Good model, 40, 5, 10\n"
			gpus, average := mergeGPUInfo(telemetryResources(), parseGPUOutput(output), nil)
			requireUnknownTelemetry(t, gpus[0])
			requireTelemetryMetric(t, gpus[1].LoadPercent, 45)
			if gpus[0].Model == "" || average != nil {
				t.Fatalf("invalid GPU lost its inventory model or produced an average: %#v, %v", gpus[0], average)
			}
		})
	}
}

func TestGPUDuplicateUUIDsAreUnknown(t *testing.T) {
	valid := telemetryFirstUUID + ", First live model, 20, 1, 10\n"
	for _, duplicate := range []string{
		strings.ToLower(telemetryFirstUUID) + ", Duplicate, 40, 2, 10\n",
		telemetryFirstUUID + ", Broken, N/A, 2, 10\n",
	} {
		output := valid + duplicate + telemetrySecondUUID + ", Second live model, 80, 9, 10\n"
		gpus, average := mergeGPUInfo(telemetryResources(), parseGPUOutput(output), nil)
		requireUnknownTelemetry(t, gpus[0])
		requireTelemetryMetric(t, gpus[1].LoadPercent, 85)
		if average != nil || !strings.Contains(gpus[0].Error, "duplicate") {
			t.Fatalf("duplicate UUID was not rejected: %#v, %v", gpus, average)
		}
	}

	resources := telemetryResources()
	resources.GPU.Cards[1].Nvidia.UUID = telemetryFirstUUID
	gpus, average := mergeGPUInfo(resources, parseGPUOutput(valid), nil)
	for _, gpu := range gpus {
		requireUnknownTelemetry(t, gpu)
	}
	if average != nil {
		t.Fatal("ambiguous inventory produced an average")
	}
}

func TestGPUMissingAndMixedVendorInventory(t *testing.T) {
	resources := telemetryResources()
	resources.GPU.Cards = append(resources.GPU.Cards,
		api.ResourcesGPUCard{PCIAddress: "0000:81:00.0", Vendor: "AMD", Product: "AMD Radeon PRO"},
		api.ResourcesGPUCard{PCIAddress: "0000:82:00.0", Product: "NVIDIA without UUID", Nvidia: &api.ResourcesGPUCardNvidia{}},
	)
	output := telemetryFirstUUID + ", First live model, 0, 0, 10\n" +
		"GPU-33333333-3333-3333-3333-333333333333, Not in Incus, 100, 10, 10\n" +
		"not-a-uuid, Invalid identity, 100, 10, 10\n"
	gpus, average := mergeGPUInfo(resources, parseGPUOutput(output), nil)
	if len(gpus) != 4 || average != nil {
		t.Fatalf("inventory cards were omitted or missing data produced an average: %#v, %v", gpus, average)
	}
	requireTelemetryMetric(t, gpus[0].LoadPercent, 0)
	for _, gpu := range gpus[1:] {
		requireUnknownTelemetry(t, gpu)
	}
	if gpus[1].Model != "second product" || gpus[2].Model != "AMD Radeon PRO" || gpus[2].UUID != "" || gpus[3].Model != "NVIDIA without UUID" {
		t.Fatalf("inventory model/identity not retained: %#v", gpus)
	}
	if _, found := parseGPUOutput(output)["not-a-uuid"]; found {
		t.Fatal("invalid UUID accepted as a telemetry identity")
	}
}

func TestGPUCollectionFailureAndModelFallback(t *testing.T) {
	resources := telemetryResources()
	gpus, average := mergeGPUInfo(resources, nil, errors.New("NVIDIA telemetry unavailable: executable not found"))
	if len(gpus) != 2 || average != nil {
		t.Fatalf("command failure lost inventory or invented an average: %#v, %v", gpus, average)
	}
	for _, gpu := range gpus {
		requireUnknownTelemetry(t, gpu)
	}
	if gpus[0].Model != "first inventory model" || gpus[1].Model != "second product" {
		t.Fatalf("Incus NVIDIA model/Product fallback not retained: %#v", gpus)
	}

	// A failed executable lookup exercises the production failure path without
	// requiring a host GPU, shell, or nvidia-smi installation.
	t.Setenv("PATH", t.TempDir())
	gpus, average = collectGPUs(context.Background(), resources)
	if len(gpus) != 2 || average != nil {
		t.Fatalf("collector failure did not retain inventory: %#v, %v", gpus, average)
	}
	for _, gpu := range gpus {
		requireUnknownTelemetry(t, gpu)
	}
}

func TestGPUEmptyInventoryAndBoundaryMetrics(t *testing.T) {
	for _, resources := range []*api.Resources{nil, {}} {
		gpus, average := collectGPUs(context.Background(), resources)
		if gpus == nil || len(gpus) != 0 || average != nil {
			t.Fatalf("empty inventory must return [] and null: %#v, %v", gpus, average)
		}
	}
	for _, row := range []string{"0, 0, 10", "100, 10, 10"} {
		resources := telemetryResources()
		resources.GPU.Cards = resources.GPU.Cards[:1]
		gpus, average := mergeGPUInfo(resources, parseGPUOutput(telemetryFirstUUID+", Model, "+row+"\r\n"), nil)
		expected := float64(0)
		if strings.HasPrefix(row, "100") {
			expected = 100
		}
		requireTelemetryMetric(t, gpus[0].LoadPercent, expected)
		requireTelemetryMetric(t, average, expected)
	}
}

func TestGPUOutputLimit(t *testing.T) {
	output := &gpuOutputBuffer{}
	if n, err := output.Write([]byte(strings.Repeat("x", maxGPUOutputBytes))); err != nil || n != maxGPUOutputBytes {
		t.Fatalf("exactly bounded output rejected: %d, %v", n, err)
	}
	if n, err := output.Write([]byte("more")); err == nil || n != 0 || output.buffer.Len() != maxGPUOutputBytes {
		t.Fatalf("output limit not enforced: n=%d err=%v size=%d", n, err, output.buffer.Len())
	}
	streamed := &gpuOutputBuffer{}
	source := io.LimitReader(strings.NewReader(strings.Repeat("x", maxGPUOutputBytes+1)), maxGPUOutputBytes+1)
	if n, err := io.Copy(streamed, source); err == nil || n != maxGPUOutputBytes || streamed.buffer.Len() != maxGPUOutputBytes {
		t.Fatalf("streamed output bypassed limit: n=%d err=%v size=%d", n, err, streamed.buffer.Len())
	}
}
