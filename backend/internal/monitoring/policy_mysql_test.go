package monitoring

import (
	"context"
	"os"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm/clause"
)

func TestMySQLPolicyPollCannotRewindIdleContinuity(t *testing.T) {
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
	var sample platform.Row
	if e = db.Table("instance_metric_samples s").Select("s.*").Where("EXISTS (SELECT 1 FROM instance_gpu_metric_samples g WHERE g.sample_id=s.id)").Order("s.id DESC").Take(&sample).Error; e != nil {
		t.Skip("instance GPU sample fixture required")
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	id := platform.Uint(sample["instance_id"])
	instance, e := platform.Get(tx, "instances", id, false)
	if e != nil {
		t.Fatal(e)
	}
	var accounts []platform.Row
	if e = tx.Table("credit_accounts").Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id=?", instance["user_id"]).Find(&accounts).Error; e != nil {
		t.Fatal(e)
	}
	if _, e = platform.Get(tx, "instances", id, true); e != nil {
		t.Fatal(e)
	}
	if e = tx.Table("instances").Where("id=?", id).Updates(platform.Row{"observed_state": "running", "gpu_policy": `{"idle":{"enabled":false,"timeout_seconds":60,"action":"stop_instance"},"rebalance":{"enabled":false}}`}).Error; e != nil {
		t.Fatal(e)
	}
	var gpu platform.Row
	if e = tx.Table("instance_gpu_metric_samples").Where("sample_id=?", sample["id"]).Take(&gpu).Error; e != nil {
		t.Fatal(e)
	}
	at := platform.Stamp(sample["sampled_at"])
	since := at.Add(-2 * time.Minute)
	sequence := platform.Uint(sample["sample_sequence"]) + 1
	window := platform.Row{"instance_id": id, "gpu_uuid": gpu["gpu_uuid"], "agent_boot_id": sample["agent_boot_id"], "runtime_started_at": sample["runtime_started_at"], "last_sequence": sequence, "last_sampled_at": at.Add(time.Second), "idle_since": since, "updated_at": at}
	if e = tx.Table("metric_gpu_idle_windows").Clauses(gpuIdleWindowConflict).Create(window).Error; e != nil {
		t.Fatal(e)
	}
	a := platform.New(tx, platform.Config{})
	if e = tx.Exec("CREATE TEMPORARY TABLE policy_versions (id BIGINT PRIMARY KEY,kind VARCHAR(32),config JSON NOT NULL)").Error; e != nil {
		t.Fatal(e)
	}
	if e = tx.Exec(`INSERT INTO policy_versions VALUES (1,'gpu-idle','{"enabled":true,"min_timeout_seconds":60,"max_timeout_seconds":86400}'),(2,'gpu-scheduling','{"evaluation_window_seconds":5,"metrics_max_age_seconds":3600}')`).Error; e != nil {
		t.Fatal(e)
	}
	if e = tx.Exec(`UPDATE instances SET policy_versions=JSON_SET(policy_versions,'$."gpu-idle"','1','$."gpu-scheduling"','2') WHERE id=?`, id).Error; e != nil {
		t.Fatal(e)
	}
	if e = evaluateGPUInstance(context.Background(), a, id); e != nil {
		t.Fatal(e)
	}
	var saved platform.Row
	if e = tx.Table("metric_gpu_idle_windows").Where("instance_id=? AND gpu_uuid=?", id, gpu["gpu_uuid"]).Take(&saved).Error; e != nil {
		t.Fatal(e)
	}
	if platform.Uint(saved["last_sequence"]) != sequence || !platform.Stamp(saved["idle_since"]).Equal(since) {
		t.Fatalf("policy polling rewound a newer immutable observation: %+v", saved)
	}
}
