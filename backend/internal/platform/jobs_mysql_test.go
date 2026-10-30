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

func TestMySQLTaskAndStepSafeFailurePersistence(t *testing.T) {
	for _, status := range []string{"failed", "unknown"} {
		t.Run(status, func(t *testing.T) {
			db := jobTestDB(t)
			queued, err := Enqueue(db, "regression.safe-error", 0, 0, 0, false, Row{})
			if err != nil {
				t.Fatal(err)
			}
			id, _ := IDString(Text(queued["task_id"]))
			a := New(db, Config{})
			job := Job{ID: id, LeaseGeneration: 1}
			if err = db.Table("tasks").Where("id=?", id).Updates(Row{"status": "running", "lease_owner": "owner", "lease_generation": 1, "lease_until": a.Time().Add(time.Minute), "started_at": a.Time(), "result_redacted": `{"observed_state":"pending"}`}).Error; err != nil {
				t.Fatal(err)
			}
			if err = a.Step(context.Background(), job, "create.preflight", "", "running"); err != nil {
				t.Fatal(err)
			}
			cause := EDetails(409, "storage_pool_capacity_exceeded", "存储池空间不足，请减少磁盘申请。", Row{"available": "16106127360", "requested": "21474836480"})
			var finished any = a.Time()
			if status == "unknown" {
				cause = Unknown(cause)
				finished = nil
			}
			if err = a.finishTask(context.Background(), job, "wrong-owner", status, finished, "storage_pool_capacity_exceeded", cause); err == nil {
				t.Fatal("stale owner completed task")
			}
			if err = a.finishTask(context.Background(), job, "owner", status, finished, "storage_pool_capacity_exceeded", cause); err != nil {
				t.Fatal(err)
			}
			stored, err := Get(db, "tasks", id, false)
			if err != nil {
				t.Fatal(err)
			}
			if Text(stored["status"]) != status || Text(stored["error_code"]) != "storage_pool_capacity_exceeded" || StoredTaskError(stored["result_redacted"], status, Text(stored["error_code"])) == nil {
				t.Fatalf("missing safe cause: %+v", stored)
			}
			var result map[string]any
			if err = json.Unmarshal([]byte(Text(stored["result_redacted"])), &result); err != nil || result["observed_state"] != "pending" {
				t.Fatalf("existing task result overwritten: %v %+v", err, result)
			}
			var step Row
			if err = db.Table("task_steps").Where("task_id=?", id).Take(&step).Error; err != nil {
				t.Fatal(err)
			}
			if Text(step["status"]) != status || StoredTaskError(step["result_redacted"], status, Text(step["error_code"])) == nil {
				t.Fatalf("step cause absent: %+v", step)
			}
		})
	}
}

func TestMySQLTimeoutExecutionRemainsUnknown(t *testing.T) {
	db := jobTestDB(t)
	queued, err := Enqueue(db, "regression.timeout", 0, 0, 0, false, Row{})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := IDString(Text(queued["task_id"]))
	a := New(db, Config{})
	a.RegisterJob("regression.timeout", func(context.Context, *App, Job) error { return Unknown(context.DeadlineExceeded) })
	if err = db.Table("tasks").Where("id=?", id).Updates(Row{"status": "running", "lease_owner": "test", "lease_generation": 1, "lease_until": a.Time().Add(time.Minute), "started_at": a.Time()}).Error; err != nil {
		t.Fatal(err)
	}
	a.execute(context.Background(), Job{ID: id, Type: "regression.timeout", LeaseGeneration: 1}, "test")
	stored, err := Get(db, "tasks", id, false)
	if err != nil {
		t.Fatal(err)
	}
	if Text(stored["status"]) != "unknown" || stored["finished_at"] != nil || Text(stored["error_code"]) != "external_result_unknown" {
		t.Fatalf("timeout became replayable failure: %+v", stored)
	}
	public := StoredTaskError(stored["result_redacted"], "unknown", "external_result_unknown")
	if public == nil || Text(public["message"]) == context.DeadlineExceeded.Error() {
		t.Fatalf("timeout safe fallback absent: %+v", public)
	}
}
