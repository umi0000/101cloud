package monitoring

import (
	"fmt"
	"os"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
)

func TestMySQLIdleContinuityUsesAgentRuntimeState(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_DSN to a migrated test database")
	}
	db, e := platform.OpenDB(platform.Config{DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	var fixture struct{ ID uint64 }
	if e = db.Table("instances").Select("id").Order("id").Limit(1).Find(&fixture).Error; e != nil {
		t.Fatal(e)
	}
	if fixture.ID == 0 {
		t.Skip("instance fixture required")
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	a := platform.New(tx, platform.Config{})
	at := a.Time().Add(-2 * time.Second)
	boot := []byte("0123456789abcdef")
	uuid := fmt.Sprintf("GPU-idle-runtime-%d", time.Now().UnixNano())
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
	inst.GPUs[0].Processes = &busy
	frame.Sequence = 3
	if e = metricObserveGPUIdle(tx, a, fixture.ID, boot, frame, inst, a.Time(), next); e != nil {
		t.Fatal(e)
	}
	if e = tx.Table("metric_gpu_idle_windows").Where("instance_id=? AND gpu_uuid=?", fixture.ID, uuid).Take(&saved).Error; e != nil {
		t.Fatal(e)
	}
	if saved["idle_since"] != nil {
		t.Fatal("a live GPU process did not reset idle continuity")
	}
}
