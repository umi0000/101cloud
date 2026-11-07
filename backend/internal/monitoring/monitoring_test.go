package monitoring

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestMetricCPUIntervalAcceptsAgentNanosecondWindow(t *testing.T) {
	// Captured from a real schema-v1 Agent response. CPU observations retain
	// nanoseconds even though the frame and MySQL timestamps use microseconds.
	at, err := time.Parse(time.RFC3339Nano, "2026-10-30T02:56:11.498994560Z")
	if err != nil {
		t.Fatal(err)
	}
	window := decimal.RequireFromString("5.002381337")
	location := time.FixedZone("UTC+8", 8*3600)
	start, end, stored, err := metricCPUInterval(metricCPU{CollectedAt: at, Window: &window}, location)
	if err != nil {
		t.Fatal(err)
	}
	wantStart := at.Add(-5002381337 * time.Nanosecond).Truncate(time.Microsecond)
	if !start.Equal(wantStart) || !end.Equal(at.Truncate(time.Microsecond)) || start.Location() != location || end.Location() != location {
		t.Fatalf("CPU endpoints not quantized in database location: %v %v", start, end)
	}
	if stored.StringFixed(6) != "5.002381" {
		t.Fatalf("unexpected stored CPU window: %s", stored)
	}
}

func TestMetricCPUIntervalDerivesDurationFromQuantizedEndpoints(t *testing.T) {
	at := time.Date(2026, 10, 30, 2, 56, 11, 498994100, time.UTC)
	window := decimal.RequireFromString("5.002381337")
	start, end, stored, err := metricCPUInterval(metricCPU{CollectedAt: at, Window: &window}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if stored.StringFixed(6) != "5.002382" || end.Sub(start) != 5002382*time.Microsecond {
		t.Fatalf("duration was rounded independently of endpoints: %v %v %s", start, end, stored)
	}
}

func TestMetricCPUIntervalLeavesUnavailableWindowsNull(t *testing.T) {
	at := time.Date(2026, 10, 30, 2, 56, 11, 498994560, time.UTC)
	positive := decimal.NewFromInt(5)
	zero := decimal.Zero
	negative := decimal.NewFromInt(-1)
	subMicrosecond := decimal.RequireFromString("0.000000001")
	for name, cpu := range map[string]metricCPU{
		"first_sample":       {CollectedAt: at},
		"missing_timestamp":  {Window: &positive},
		"zero_window":        {CollectedAt: at, Window: &zero},
		"negative_window":    {CollectedAt: at, Window: &negative},
		"unrepresentable_ns": {CollectedAt: at, Window: &subMicrosecond},
	} {
		t.Run(name, func(t *testing.T) {
			start, end, window, err := metricCPUInterval(cpu, time.UTC)
			if err != nil || !start.IsZero() || !end.IsZero() || !window.IsZero() {
				t.Fatalf("unavailable CPU interval became a measurement: %v %v %s %v", start, end, window, err)
			}
		})
	}
}

func TestMetricCPUIntervalPreservesCollectionBound(t *testing.T) {
	at := time.Date(2026, 10, 30, 2, 56, 11, 0, time.UTC)
	for _, input := range []string{"86400.000000001", "999999999999999999999999999999999"} {
		window := decimal.RequireFromString(input)
		if _, _, _, err := metricCPUInterval(metricCPU{CollectedAt: at, Window: &window}, time.UTC); err == nil {
			t.Fatalf("out-of-bound CPU window accepted: %s", input)
		}
	}
	window := decimal.NewFromInt(86400)
	start, end, stored, err := metricCPUInterval(metricCPU{CollectedAt: at, Window: &window}, time.UTC)
	if err != nil || end.Sub(start) != 24*time.Hour || !stored.Equal(window) {
		t.Fatalf("valid boundary CPU window rejected: %v %v %s %v", start, end, stored, err)
	}
}

func TestMetricReasonsRetainsExpectedGPUObservationStates(t *testing.T) {
	q := metricQuality{Reasons: []string{
		"gpu_utilization_no_samples", "instance_not_running", "gpu_utilization_no_samples",
		"gpu_metric_unavailable", "gpu_utilization_unknown", "instance_attribution_unknown",
		"/private/not-an-allowed-reason",
	}}
	want := `["gpu_utilization_no_samples","instance_not_running","gpu_metric_unavailable","gpu_utilization_unknown","instance_attribution_unknown"]`
	if got := metricReasons(q); got != want {
		t.Fatalf("expected observation states or real failure reasons were dropped: %s", got)
	}
}

func TestSharedControlReferencesRemainPartialAndPublic(t *testing.T) {
	q := metricQuality{Reasons: []string{"gpu_shared_control_references", "gpu_utilization_no_samples", "gpu_shared_control_references", "/private/process/path"}}
	if got := metricReference(q); got != "partial" {
		t.Fatalf("shared references reported as %q", got)
	}
	if got := metricReasons(q); got != `["gpu_shared_control_references","gpu_utilization_no_samples"]` {
		t.Fatalf("shared-control reason lost or private reason leaked: %s", got)
	}
	// A contradictory incoming flag must not make reference scope complete.
	q.AttributionComplete = true
	if got := metricReference(q); got != "partial" {
		t.Fatalf("shared-reference reason was overridden by full flag: %q", got)
	}
}
