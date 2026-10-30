package platform

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"gorm.io/gorm"
)

func jobTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("CLOUD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_DSN to a migrated test database")
	}
	db, e := OpenDB(Config{DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { pool.Close() })
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx
}
func TestMySQLQueuedTaskIdentity(t *testing.T) {
	db := jobTestDB(t)
	result, e := Enqueue(db, "regression.read", 0, 0, 0, false, Row{"purpose": "verify task identity"})
	if e != nil {
		t.Fatal(e)
	}
	id, e := IDString(Text(result["task_id"]))
	if e != nil {
		t.Fatal(e)
	}
	stored, e := Get(db, "tasks", id, false)
	if e != nil {
		t.Fatal(e)
	}
	if Text(stored["task_type"]) != "regression.read" || Text(stored["status"]) != "queued" {
		t.Fatalf("returned identity points to another task: %+v", stored)
	}
}
func TestMySQLRepeatedResultRetainsLease(t *testing.T) {
	db := jobTestDB(t)
	result, e := Enqueue(db, "regression.result", 0, 0, 0, false, Row{})
	if e != nil {
		t.Fatal(e)
	}
	id, _ := IDString(Text(result["task_id"]))
	a := New(db, Config{})
	now := a.Time()
	if e = db.Table("tasks").Where("id=?", id).Updates(Row{"status": "running", "lease_owner": "test", "lease_generation": 1, "lease_until": now.Add(time.Minute), "started_at": now}).Error; e != nil {
		t.Fatal(e)
	}
	job := Job{ID: id, LeaseGeneration: 1}
	for range 2 {
		if e = a.TaskResult(context.Background(), job, Row{"observed_state": "running"}); e != nil {
			t.Fatalf("same valid result lost lease: %v", e)
		}
	}
	if e = db.Table("tasks").Where("id=?", id).Update("lease_until", now.Add(-time.Second)).Error; e != nil {
		t.Fatal(e)
	}
	if e = a.TaskResult(context.Background(), job, Row{"observed_state": "stopped"}); e == nil {
		t.Fatal("expired lease changed task result")
	}
	row, e := Get(db, "tasks", id, false)
	if e != nil {
		t.Fatal(e)
	}
	var saved struct {
		State string `json:"observed_state"`
	}
	if e = json.Unmarshal([]byte(Text(row["result_redacted"])), &saved); e != nil || saved.State != "running" {
		t.Fatalf("expired writer overwrote result: %+v %v", saved, e)
	}
}
