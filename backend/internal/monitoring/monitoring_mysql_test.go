package monitoring

import (
	"crypto/rand"
	"os"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
)

func TestMySQLIngestFrameAcceptsAgentCPUPrecision(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_DSN to a migrated test database")
	}
	db, err := platform.OpenDB(platform.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var fixture struct {
		ID          uint64
		NodeID      uint64
		IncusUUID   string
		ProjectName string
	}
	if err = db.Table("instances").Select("id,node_id,incus_uuid,project_name").Where("deleted_at IS NULL AND CHAR_LENGTH(incus_uuid)=36").Take(&fixture).Error; err != nil {
		t.Skip("a runtime-linked instance fixture is required")
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	a := platform.New(tx, platform.Config{})
	boot := make([]byte, 16)
	if _, err = rand.Read(boot); err != nil {
		t.Fatal(err)
	}
	at := a.Time().Add(-time.Minute)
	start := at.Add(-5 * time.Second)
	window := decimal.RequireFromString("5.002381337")
	percent := decimal.RequireFromString("0.00047477388067826144")
	usage, delta, allocated := uint64(766224635000), uint64(95000), uint64(4000000000)
	instance := metricInstance{
		Project: fixture.ProjectName, UUID: fixture.IncusUUID, State: "running",
		Started: at.Add(-time.Hour), GPUState: "unknown",
		CPU:     metricCPU{CollectedAt: at.Add(-time.Second).Add(100 * time.Nanosecond), Window: &window, Percent: &percent, Usage: &usage, Delta: &delta, Allocated: &allocated},
		Quality: metricQuality{Reasons: []string{"gpu_metric_unavailable", "/private/not-an-allowed-reason"}},
	}
	frame := metricFrame{Schema: 1, Boot: bootString(boot), Sequence: 1, At: at, Start: start, End: at, Instances: []metricInstance{instance}}
	earliest := start
	if err = ingestFrame(tx, a, fixture.NodeID, boot, frame, at, start, at, &earliest); err != nil {
		t.Fatal(err)
	}
	var saved platform.Row
	query := tx.Table("instance_metric_samples").Where("instance_id=? AND agent_boot_id=? AND sample_sequence=?", fixture.ID, boot, frame.Sequence)
	if err = query.Take(&saved).Error; err != nil {
		t.Fatal(err)
	}
	cpuStart, cpuEnd, cpuWindow, err := metricCPUInterval(instance.CPU, a.Location)
	if err != nil {
		t.Fatal(err)
	}
	if !platform.Stamp(saved["cpu_sampled_at"]).Equal(cpuEnd) || !earliest.Equal(cpuStart) || platform.Text(saved["cpu_window_seconds"]) != cpuWindow.StringFixed(6) {
		t.Fatalf("independent CPU interval or retention boundary was lost: %+v", saved)
	}
	if platform.Uint(saved["cpu_usage_total_ns"]) != usage || platform.Uint(saved["cpu_usage_delta_ns"]) != delta || platform.Text(saved["allocated_cpu_seconds"]) != "20.009528000" {
		t.Fatalf("CPU counters or quantized capacity integral changed: %+v", saved)
	}
	if platform.Text(saved["agent_boot_id"]) != string(boot) || platform.Uint(saved["sample_sequence"]) != 1 {
		t.Fatal("sample lost its boot/sequence identity")
	}
	for _, key := range []string{"memory_used_bytes", "memory_limit_bytes", "memory_peak_bytes"} {
		if saved[key] != nil {
			t.Fatalf("unknown %s became a non-NULL measurement", key)
		}
	}
	if platform.Text(saved["quality_reasons"]) != `["gpu_metric_unavailable"]` {
		t.Fatalf("non-allowlisted reason was stored: %s", platform.Text(saved["quality_reasons"]))
	}
	// Replay remains idempotent, even for a batch containing nanosecond windows.
	if err = ingestFrame(tx, a, fixture.NodeID, boot, frame, at, start, at, &earliest); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err = query.Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("sample replay was not idempotent: count=%d error=%v", count, err)
	}
	// A subsequent counter-reset/first-sample observation must not inherit CPU
	// values from the preceding frame or turn missing data into zeroes.
	frame.Sequence++
	frame.Instances[0].CPU = metricCPU{CollectedAt: instance.CPU.CollectedAt.Add(time.Second)}
	if err = ingestFrame(tx, a, fixture.NodeID, boot, frame, at, start, at, &earliest); err != nil {
		t.Fatal(err)
	}
	var unavailable platform.Row
	if err = tx.Table("instance_metric_samples").Where("instance_id=? AND agent_boot_id=? AND sample_sequence=?", fixture.ID, boot, frame.Sequence).Take(&unavailable).Error; err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cpu_sampled_at", "cpu_window_seconds", "cpu_usage_total_ns", "cpu_usage_delta_ns", "allocated_vcpus", "allocated_cpu_seconds", "cpu_utilization_pct"} {
		if unavailable[key] != nil {
			t.Fatalf("unavailable %s became a non-NULL measurement", key)
		}
	}
}
