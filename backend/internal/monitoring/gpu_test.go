package monitoring

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Execute the production selector against MySQL's actual JSON/boolean evaluator.
// A derived relation keeps the regression independent of business fixtures and
// changes no database schema or persistent data.
func TestGPUAutomaticCandidatesIncludeIdleStopOnly(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("CLOUD_TEST_MYSQL_DSN is required for the MySQL JSON selector regression")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows := `SELECT name FROM (
 SELECT 'idle-stop-only' AS name, 1 AS node_id, NULL AS deleted_at, 'running' AS observed_state,
  0 AS auto_release_gpu, 0 AS auto_rebind_gpu, CAST('{"idle":{"enabled":true,"action":"stop_instance"}}' AS JSON) AS gpu_policy
 UNION ALL SELECT 'detach-enabled',1,NULL,'running',1,0,CAST('{"idle":{"enabled":true,"action":"detach_gpu"}}' AS JSON)
 UNION ALL SELECT 'rebalance-enabled',1,NULL,'running',0,1,CAST('{"idle":{"enabled":false,"action":"detach_gpu"}}' AS JSON)
 UNION ALL SELECT 'disabled-stop',1,NULL,'running',0,0,CAST('{"idle":{"enabled":false,"action":"stop_instance"}}' AS JSON)
 UNION ALL SELECT 'stopped-stop',1,NULL,'stopped',0,0,CAST('{"idle":{"enabled":true,"action":"stop_instance"}}' AS JSON)
 UNION ALL SELECT 'deleted-stop',1,'2026-10-30 00:00:00','running',0,0,CAST('{"idle":{"enabled":true,"action":"stop_instance"}}' AS JSON)
 UNION ALL SELECT 'other-node-stop',2,NULL,'running',0,0,CAST('{"idle":{"enabled":true,"action":"stop_instance"}}' AS JSON)
 UNION ALL SELECT 'no-policy',1,NULL,'running',0,0,CAST('{}' AS JSON)
 UNION ALL SELECT 'string-not-boolean',1,NULL,'running',0,0,CAST('{"idle":{"enabled":"true","action":"stop_instance"}}' AS JSON)
) candidates WHERE ` + gpuAutomaticCandidateFilter + ` ORDER BY name`
	var got []string
	if err = db.WithContext(ctx).Raw(rows, 1).Scan(&got).Error; err != nil {
		t.Fatal(err)
	}
	want := []string{"detach-enabled", "idle-stop-only", "rebalance-enabled"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("automatic policy candidates = %v, want %v", got, want)
	}
}

// Exercise schema-less map upserts against real migrated tables. The transaction
// always rolls back; no schema or persistent fixture changes are made.
func TestGPUWindowMapUpserts(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("CLOUD_TEST_MYSQL_DSN is required for the MySQL upsert regression")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var fixture struct {
		ID     uint64
		NodeID uint64
		SchedulingPolicyID string
	}
	if err = db.WithContext(ctx).Table("instances").Select("id,node_id,JSON_UNQUOTE(JSON_EXTRACT(policy_versions,'$.\"gpu-scheduling\"')) AS scheduling_policy_id").Where("JSON_EXTRACT(policy_versions,'$.\"gpu-scheduling\"') IS NOT NULL").Order("id").Limit(1).Find(&fixture).Error; err != nil {
		t.Fatal(err)
	}
	if fixture.ID == 0 {
		t.Skip("a migrated database with an instance fixture is required")
	}
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	uuid := fmt.Sprintf("GPU-monitoring-upsert-%d", time.Now().UnixNano())
	first := time.Now().Truncate(time.Microsecond)
	second := first.Add(time.Second)
	high := map[string]any{"node_id": fixture.NodeID, "gpu_uuid": uuid, "policy_version_id": fixture.SchedulingPolicyID, "high_since": first, "last_sampled_at": first, "updated_at": first}
	if err = tx.Table("metric_gpu_high_windows").Clauses(gpuHighWindowConflict).Create(high).Error; err != nil {
		t.Fatal(err)
	}
	high["high_since"], high["last_sampled_at"], high["updated_at"] = nil, second, second
	if err = tx.Table("metric_gpu_high_windows").Clauses(gpuHighWindowConflict).Create(high).Error; err != nil {
		t.Fatal(err)
	}
	var highResult struct {
		Count    int64
		Cleared  int64
		Advanced int64
	}
	if err = tx.Table("metric_gpu_high_windows").Select("COUNT(*) AS count,MAX(high_since IS NULL) AS cleared,MAX(last_sampled_at=?) AS advanced", second).Where("node_id=? AND gpu_uuid=?", fixture.NodeID, uuid).Scan(&highResult).Error; err != nil {
		t.Fatal(err)
	}
	if highResult.Count != 1 || highResult.Cleared != 1 || highResult.Advanced != 1 {
		t.Fatalf("high-window upsert did not replace mutable values: %+v", highResult)
	}
	idle := map[string]any{"instance_id": fixture.ID, "gpu_uuid": uuid, "agent_boot_id": []byte("0123456789abcdef"), "runtime_started_at": first, "last_sequence": uint64(1), "last_sampled_at": first, "idle_since": first, "updated_at": first}
	if err = tx.Table("metric_gpu_idle_windows").Clauses(gpuIdleWindowConflict).Create(idle).Error; err != nil {
		t.Fatal(err)
	}
	idle["agent_boot_id"], idle["runtime_started_at"], idle["last_sequence"], idle["last_sampled_at"], idle["idle_since"], idle["updated_at"] = []byte("fedcba9876543210"), nil, uint64(2), second, nil, second
	if err = tx.Table("metric_gpu_idle_windows").Clauses(gpuIdleWindowConflict).Create(idle).Error; err != nil {
		t.Fatal(err)
	}
	var idleResult struct {
		Count    int64
		Cleared  int64
		Sequence uint64
		Boot     string
	}
	if err = tx.Table("metric_gpu_idle_windows").Select("COUNT(*) AS count,MAX(idle_since IS NULL AND runtime_started_at IS NULL) AS cleared,MAX(last_sequence) AS sequence,MAX(HEX(agent_boot_id)) AS boot").Where("instance_id=? AND gpu_uuid=?", fixture.ID, uuid).Scan(&idleResult).Error; err != nil {
		t.Fatal(err)
	}
	if idleResult.Count != 1 || idleResult.Cleared != 1 || idleResult.Sequence != 2 || idleResult.Boot != "66656463626139383736353433323130" {
		t.Fatalf("idle-window upsert did not replace mutable values: %+v", idleResult)
	}
}
