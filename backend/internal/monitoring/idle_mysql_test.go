package monitoring

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	driver "github.com/go-sql-driver/mysql"
	"github.com/shopspring/decimal"
)

func TestMySQLIdleContinuityUsesAgentRuntimeState(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated idle safety regressions")
	}
	cfg, e := driver.ParseDSN(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.DBName = ""
	owner, e := sql.Open("mysql", cfg.FormatDSN())
	if e != nil {
		t.Fatal(e)
	}
	defer owner.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var nonce [8]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		t.Fatal(e)
	}
	database := "ci_idle_safety_" + hex.EncodeToString(nonce[:])
	if _, e = owner.ExecContext(ctx, "CREATE DATABASE `"+database+"`"); e != nil {
		t.Fatal(e)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, e := owner.ExecContext(cleanup, "DROP DATABASE `"+database+"`"); e != nil {
			t.Error(e)
		}
	}()
	cfg.DBName = database
	db, e := platform.OpenDB(platform.Config{DSN: cfg.FormatDSN()})
	if e != nil {
		t.Fatal(e)
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	// Only the two tables touched by production idle ingestion are needed. Keep
	// the real composite key, binary boot ID, timestamp precision and FK; do not
	// depend on any instance or metric sample in the shared integration database.
	for _, statement := range []string{
		`CREATE TABLE instances(id BIGINT UNSIGNED PRIMARY KEY,observed_at DATETIME(6) NULL,idle_since DATETIME(6) NULL) ENGINE=InnoDB`,
		`CREATE TABLE metric_gpu_idle_windows(instance_id BIGINT UNSIGNED NOT NULL,gpu_uuid VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,agent_boot_id BINARY(16) NOT NULL,runtime_started_at DATETIME(6) NULL,last_sequence BIGINT UNSIGNED NOT NULL,last_sampled_at DATETIME(6) NOT NULL,idle_since DATETIME(6) NULL,updated_at DATETIME(6) NOT NULL,PRIMARY KEY(instance_id,gpu_uuid),CONSTRAINT fk_metric_gpu_idle_instance FOREIGN KEY(instance_id) REFERENCES instances(id)) ENGINE=InnoDB`,
		`INSERT INTO instances(id) VALUES(1)`,
	} {
		if e = db.WithContext(ctx).Exec(statement).Error; e != nil {
			t.Fatal(e)
		}
	}
	fixture := struct{ ID uint64 }{ID: 1}
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	a := platform.New(tx, platform.Config{})
	at := a.Time().Add(-time.Minute)
	boot := []byte("0123456789abcdef")
	uuid := "GPU-idle-runtime-safety"
	zero := uint64(0)
	count := uint32(0)
	quality := metricQuality{AttributionComplete: true}
	inst := metricInstance{State: "Running", Started: at.Add(-time.Minute), Quality: quality, GPUs: []metricGPU{{UUID: uuid, State: "idle_candidate", Processes: &count, References: &count, Memory: &zero, Quality: quality}}}
	frame := metricFrame{Complete: true, Sequence: 1}
	if e = metricObserveGPUIdle(tx, a, fixture.ID, boot, frame, inst, at, at); e != nil {
		t.Fatal(e)
	}
	frame.Sequence = 2
	next := at.Add(time.Second)
	if e = metricObserveGPUIdle(tx, a, fixture.ID, boot, frame, inst, next, at); e != nil {
		t.Fatal(e)
	}
	var saved platform.Row
	if e = tx.Table("metric_gpu_idle_windows").Where("instance_id=? AND gpu_uuid=?", fixture.ID, uuid).Take(&saved).Error; e != nil {
		t.Fatal(e)
	}
	if !platform.Stamp(saved["idle_since"]).Equal(at) {
		t.Fatalf("capitalized Running reset the continuous idle window: %+v", saved)
	}
	busy := uint32(1)
	used := uint64(1)
	zeroSM := decimal.Zero
	busySM := decimal.RequireFromString("0.01")
	for index, tc := range []struct {
		name   string
		mutate func(*metricFrame, *metricInstance)
	}{
		{"live_process", func(_ *metricFrame, i *metricInstance) { i.GPUs[0].Processes = &busy }},
		{"device_reference", func(_ *metricFrame, i *metricInstance) { i.GPUs[0].References = &busy }},
		{"one_byte_memory", func(_ *metricFrame, i *metricInstance) { i.GPUs[0].Memory = &used }},
		{"unknown_processes", func(_ *metricFrame, i *metricInstance) { i.GPUs[0].Processes = nil }},
		{"unknown_references", func(_ *metricFrame, i *metricInstance) { i.GPUs[0].References = nil }},
		{"unknown_memory", func(_ *metricFrame, i *metricInstance) { i.GPUs[0].Memory = nil }},
		{"nonzero_sm", func(_ *metricFrame, i *metricInstance) { i.GPUs[0].SM = &busySM }},
		{"incomplete_collection", func(f *metricFrame, _ *metricInstance) { f.Complete = false }},
		{"incomplete_instance_attribution", func(_ *metricFrame, i *metricInstance) { i.Quality.AttributionComplete = false }},
		{"incomplete_gpu_attribution", func(_ *metricFrame, i *metricInstance) { i.GPUs[0].Quality.AttributionComplete = false }},
		{"partial_reference_scan", func(_ *metricFrame, i *metricInstance) {
			i.GPUs[0].Quality.Reasons = []string{"gpu_reference_scan_incomplete"}
		}},
		{"unknown_gpu_state", func(_ *metricFrame, i *metricInstance) { i.GPUs[0].State = "unknown" }},
		{"stopped_runtime", func(_ *metricFrame, i *metricInstance) { i.State = "Stopped" }},
		{"unknown_runtime_generation", func(_ *metricFrame, i *metricInstance) { i.Started = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := at.Add(time.Duration(index*3+2) * time.Second)
			inst = metricInstance{State: "Running", Started: at.Add(-time.Minute), Quality: quality, GPUs: []metricGPU{{UUID: uuid, State: "idle_candidate", Processes: &count, References: &count, Memory: &zero, SM: &zeroSM, Quality: quality}}}
			frame = metricFrame{Complete: true, Sequence: uint64(index*3 + 3)}
			if e = metricObserveGPUIdle(tx, a, fixture.ID, boot, frame, inst, observed, next); e != nil {
				t.Fatal(e)
			}
			frame.Sequence++
			tc.mutate(&frame, &inst)
			if e = metricObserveGPUIdle(tx, a, fixture.ID, boot, frame, inst, observed.Add(time.Second), observed); e != nil {
				t.Fatal(e)
			}
			if e = tx.Table("metric_gpu_idle_windows").Where("instance_id=? AND gpu_uuid=?", fixture.ID, uuid).Take(&saved).Error; e != nil {
				t.Fatal(e)
			}
			if saved["idle_since"] != nil {
				t.Fatalf("unsafe evidence did not reset idle continuity: %+v", saved)
			}
		})
	}
}
