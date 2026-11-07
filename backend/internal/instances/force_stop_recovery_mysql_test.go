package instances

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

func stopRecoverySource(t *testing.T, db *gorm.DB, id uint64) {
	t.Helper()
	if err := db.Table("tasks").Where("id=?", id).Updates(platform.Row{"status": "cancelled", "error_code": "task_force_stopped", "is_instance_mutation": true, "lease_generation": gorm.Expr("lease_generation+1"), "lease_owner": nil, "lease_until": nil, "result_redacted": `{"force_stop":{"orchestration_stopped":true,"external_state":"requires_reconciliation","requires_reconciliation":true}}`, "revision": gorm.Expr("revision+1")}).Error; err != nil {
		t.Fatal(err)
	}
}

func stoppedRecoveryWrapper(t *testing.T, db *gorm.DB, a *platform.App, source platform.Job) platform.Job {
	t.Helper()
	j := source
	j.ID, j.Type, j.LeaseGeneration = 44, "instance.reconcile", 1
	j.Parameters = json.RawMessage(`{"source_task_id":43,"read_only_force_stop_reconcile":true}`)
	row := platform.Row{"id": j.ID, "task_type": j.Type, "user_id": j.UserID, "node_id": j.NodeID, "instance_id": j.InstanceID, "status": "running", "lease_owner": "test", "lease_generation": 1, "lease_until": a.Time().Add(time.Minute), "parameters_redacted": string(j.Parameters), "result_redacted": "{}"}
	if err := db.Table("tasks").Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return j
}

func assertStoppedRecoveryResources(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, item := range []struct{ table, field, want string }{{"ip_addresses", "status", "reserved"}, {"gpu_allocations", "status", "reserved"}, {"credit_reservations", "status", "held"}, {"credit_buckets", "reserved_pt", "2.000000"}} {
		r, err := platform.Get(db, item.table, 1, false)
		if err != nil || platform.Text(r[item.field]) != item.want {
			t.Fatalf("stopped orchestration changed %s: %#v %v", item.table, r, err)
		}
	}
	var ledger int64
	if err := db.Table("credit_ledger").Count(&ledger).Error; err != nil || ledger != 0 {
		t.Fatalf("stop changed financial history: %d %v", ledger, err)
	}
}

func TestMySQLForceStopRejectsLateInstanceResourceWrites(t *testing.T) {
	db, a, j, r, mutations := creationCleanupDB(t)
	stopRecoverySource(t, db, j.ID)
	if err := workerConfirm(context.Background(), a, j, r, "running", "running", platform.Row{"provisioning_state": "ready"}); err == nil {
		t.Fatal("revoked worker confirmed instance and billing resources")
	}
	_ = workerUnknown(context.Background(), a, j, r, context.Canceled)
	if err := markFailedCreation(context.Background(), a, j, confirmedCreationFailure(context.Canceled), true); err != nil {
		t.Fatal(err)
	}
	current, err := platform.Get(db, "instances", j.InstanceID, false)
	if err != nil || platform.Text(current["provisioning_state"]) != "pending" || current["creation_failed_at"] != nil {
		t.Fatalf("late completion changed stopped instance: %#v %v", current, err)
	}
	assertStoppedRecoveryResources(t, db)
	if mutations.Load() != 0 {
		t.Fatal("late completion dispatched a mutation")
	}
}

func TestMySQLForceStoppedRecoveryObservesWithoutReplaying(t *testing.T) {
	for _, scenario := range []string{"no_dispatch", "terminal", "retired_terminal", "running", "missing_identity", "recovery_stopped"} {
		t.Run(scenario, func(t *testing.T) {
			var mutations atomic.Int64
			var db *gorm.DB
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations.Add(1)
					http.Error(w, "unexpected native mutation", 500)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.HasPrefix(r.URL.Path, "/1.0/operations/") {
					if scenario == "recovery_stopped" {
						stopRecoverySource(t, db, 44)
					}
					code := 200
					if scenario == "running" {
						code = 103
					}
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status_code": code, "may_cancel": true}})
					return
				}
				_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{}})
			})
			var a *platform.App
			var source platform.Job
			db, a, source, _, _ = creationCleanupDB(t, handler)
			stopRecoverySource(t, db, source.ID)
			j := stoppedRecoveryWrapper(t, db, a, source)
			if scenario == "retired_terminal" {
				if err := db.Table("instances").Where("id=?", source.InstanceID).Updates(platform.Row{"creation_failed_at": a.Time(), "desired_state": "deleted", "provisioning_state": "failed", "creation_cleanup_state": "unknown"}).Error; err != nil {
					t.Fatal(err)
				}
				if _, err := workerInstance(context.Background(), a, j); err == nil {
					t.Fatal("ordinary reconciliation gained retired instance access")
				}
				observation := context.WithValue(context.Background(), forceStopObservationKey{}, true)
				if err := db.WithContext(observation).Transaction(func(tx *gorm.DB) error { return workerLease(a, tx, j) }); err == nil {
					t.Fatal("read-only retired visibility granted resource mutation authority")
				}
			}
			if scenario != "no_dispatch" {
				op := "/1.0/operations/original?project=default"
				if scenario == "missing_identity" {
					op = ""
				}
				step := platform.Row{"task_id": source.ID, "step_key": "create.native", "attempt": 1, "status": "cancelled", "error_code": "task_force_stopped", "operation_id": op}
				if err := db.Table("task_steps").Create(&step).Error; err != nil {
					t.Fatal(err)
				}
			}
			// Prior unresolved read-only attempts must not make recovery impossible.
			prior := platform.Row{"id": 45, "task_type": "instance.reconcile", "user_id": source.UserID, "node_id": source.NodeID, "instance_id": source.InstanceID, "status": "unknown", "parameters_redacted": `{"source_task_id":43,"read_only_force_stop_reconcile":true}`}
			if err := db.Table("tasks").Create(&prior).Error; err != nil {
				t.Fatal(err)
			}
			err := runReconcile(context.Background(), a, j)
			settled := scenario == "no_dispatch" || scenario == "terminal" || scenario == "retired_terminal"
			if (err == nil) != settled {
				t.Fatalf("settled=%v err=%v", settled, err)
			}
			if scenario == "running" || scenario == "missing_identity" {
				if platform.PublicError(err, true)["code"] != "force_stop_reconciliation_pending" {
					t.Fatalf("unresolved native dispatch not exposed: %v", err)
				}
			}
			current, getErr := platform.Get(db, "tasks", source.ID, false)
			if getErr != nil || platform.Text(current["status"]) != "cancelled" || platform.Text(current["error_code"]) != "task_force_stopped" || platform.Bool(current["is_instance_mutation"]) == settled {
				t.Fatalf("cancelled history/barrier changed incorrectly: %#v %v", current, getErr)
			}
			if platform.Bool(object(object(current["result_redacted"])["force_stop"])["reconciliation_completed"]) != settled {
				t.Fatal("current reconciliation evidence missing or premature")
			}
			if barrierErr := platform.TaskMutationBarrier(db, source.InstanceID); (barrierErr == nil) != settled {
				t.Fatalf("read-only evidence did not control the actual mutation barrier: settled=%t error=%v", settled, barrierErr)
			}
			assertStoppedRecoveryResources(t, db)
			if mutations.Load() != 0 {
				t.Fatal("read-only recovery cancelled or replayed native work")
			}
		})
	}
}

func TestMySQLForceStoppedMissingJournalCannotAuthorizeCleanup(t *testing.T) {
	db, a, j, r, _ := creationCleanupDB(t)
	stopRecoverySource(t, db, j.ID)
	step := platform.Row{"task_id": j.ID, "step_key": "create.native", "attempt": 1, "status": "cancelled", "error_code": "task_force_stopped"}
	if err := db.Table("task_steps").Create(&step).Error; err != nil {
		t.Fatal(err)
	}
	j.ID = 44
	if err := stuckDeleteQuiescent(context.Background(), a, j, r); err == nil {
		t.Fatal("cancelled force-stop journal was treated as definite nondispatch")
	}
	if err := creationQuiescent(context.Background(), a, j); err == nil {
		t.Fatal("creation cleanup trusted an ambiguous force-stop journal")
	}
	assertStoppedRecoveryResources(t, db)
}

func TestMySQLExplicitCleanupTakesStoppedBarrierWithoutRewritingJournal(t *testing.T) {
	db, a, source, r, _ := creationCleanupDB(t)
	stopRecoverySource(t, db, source.ID)
	step := platform.Row{"task_id": source.ID, "step_key": "create.native", "attempt": 1, "status": "cancelled", "error_code": "task_force_stopped", "operation_id": "/1.0/operations/original?project=default"}
	if err := db.Table("task_steps").Create(&step).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := admitStuckDelete(a, db, r); err != nil {
		t.Fatal(err)
	}
	current, err := platform.Get(db, "tasks", source.ID, false)
	if err != nil || platform.Text(current["status"]) != "cancelled" || platform.Text(current["error_code"]) != "task_force_stopped" || platform.Bool(current["is_instance_mutation"]) {
		t.Fatalf("cleanup rewrote force-stop history or retained old slot: %#v %v", current, err)
	}
	journal, err := workerStep(context.Background(), a, source, "create.native")
	if err != nil || platform.Text(journal["operation_id"]) != "/1.0/operations/original?project=default" || platform.Text(journal["error_code"]) != "task_force_stopped" {
		t.Fatalf("cleanup lost native evidence: %#v %v", journal, err)
	}
	var cleanup int64
	if err := db.Table("tasks").Where("task_type='instance.delete' AND status='queued' AND is_instance_mutation=1").Count(&cleanup).Error; err != nil || cleanup != 1 {
		t.Fatalf("cleanup did not take mutation ownership: %d %v", cleanup, err)
	}
	assertStoppedRecoveryResources(t, db)
}
