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

func deletionFrame(started time.Time) platform.Row {
	return platform.Row{
		"collection_started_at": started.Format(time.RFC3339Nano), "collected_at": started.Add(time.Millisecond).Format(time.RFC3339Nano),
		"stale": false, "collection_complete": true,
		"host_quality": platform.Row{"attribution_complete": true},
		"quality":      platform.Row{"attribution_complete": false},
		"instances":    []any{platform.Row{"uuid": "other-uuid", "name": "test-pending", "project": "other-project", "state": "stopped", "quality": platform.Row{"attribution_complete": false}}},
	}
}

type deletionFrameCase struct {
	name   string
	reason string
	change func(platform.Row, time.Time)
}

func deletionUnsafeFrames() []deletionFrameCase {
	return []deletionFrameCase{
		{"predelete_cache", "collection_predates_deletion", func(f platform.Row, barrier time.Time) {
			f["collection_started_at"] = barrier.Add(-time.Second).Format(time.RFC3339Nano)
		}},
		{"at_barrier", "collection_predates_deletion", func(f platform.Row, barrier time.Time) { f["collection_started_at"] = barrier.Format(time.RFC3339Nano) }},
		{"missing_start", "invalid_collection_time", func(f platform.Row, _ time.Time) { delete(f, "collection_started_at") }},
		{"malformed_time", "invalid_collection_time", func(f platform.Row, _ time.Time) { f["collected_at"] = "not-a-time" }},
		{"reversed_time", "invalid_collection_time", func(f platform.Row, barrier time.Time) { f["collected_at"] = barrier.Format(time.RFC3339Nano) }},
		{"future_time", "invalid_collection_time", func(f platform.Row, barrier time.Time) {
			f["collected_at"] = barrier.Add(time.Hour).Format(time.RFC3339Nano)
		}},
		{"stale_flag", "stale_collection", func(f platform.Row, _ time.Time) { f["stale"] = true }},
		{"missing_stale", "stale_collection", func(f platform.Row, _ time.Time) { delete(f, "stale") }},
		{"incomplete_inventory", "incomplete_inventory", func(f platform.Row, _ time.Time) { f["collection_complete"] = false }},
		{"missing_inventory_complete", "incomplete_inventory", func(f platform.Row, _ time.Time) { delete(f, "collection_complete") }},
		{"missing_host_quality", "incomplete_host_attribution", func(f platform.Row, _ time.Time) { delete(f, "host_quality") }},
		{"incomplete_host_quality", "incomplete_host_attribution", func(f platform.Row, _ time.Time) { f["host_quality"] = platform.Row{"attribution_complete": false} }},
		{"coerced_host_quality", "incomplete_host_attribution", func(f platform.Row, _ time.Time) { f["host_quality"] = platform.Row{"attribution_complete": "true"} }},
		{"missing_instances", "missing_instance_inventory", func(f platform.Row, _ time.Time) { delete(f, "instances") }},
		{"malformed_instances", "missing_instance_inventory", func(f platform.Row, _ time.Time) { f["instances"] = platform.Row{} }},
		{"malformed_identity", "incomplete_instance_identity", func(f platform.Row, _ time.Time) { f["instances"] = []any{"not-an-instance"} }},
		{"numeric_identity", "incomplete_instance_identity", func(f platform.Row, _ time.Time) {
			f["instances"] = []any{platform.Row{"uuid": 123, "name": "other", "project": "default"}}
		}},
		{"target_uuid", "target_still_observed", func(f platform.Row, _ time.Time) {
			f["instances"] = []any{platform.Row{"uuid": "target-uuid", "name": "renamed", "project": "other"}}
		}},
		{"target_name_project", "target_still_observed", func(f platform.Row, _ time.Time) {
			f["instances"] = []any{platform.Row{"uuid": "replacement-uuid", "name": "test-pending", "project": "default"}}
		}},
	}
}

func TestDeletedGPUFrameRequiresPostDeletionHostEvidence(t *testing.T) {
	barrier := time.Now().Add(-time.Second)
	instance := platform.Row{"incus_uuid": "target-uuid", "incus_name": "test-pending", "project_name": "default"}
	for _, tc := range deletionUnsafeFrames() {
		t.Run(tc.name, func(t *testing.T) {
			frame := deletionFrame(barrier.Add(time.Millisecond))
			tc.change(frame, barrier)
			if reason := workerDeletedGPUFrame(frame, instance, barrier, time.Now()); reason != tc.reason {
				t.Fatalf("reason=%q want %q", reason, tc.reason)
			}
		})
	}
	for _, empty := range []bool{false, true} {
		frame := deletionFrame(barrier.Add(time.Millisecond))
		if empty {
			frame["instances"] = []any{}
		}
		if reason := workerDeletedGPUFrame(frame, instance, barrier, time.Now()); reason != "" {
			t.Fatalf("complete host evidence blocked by unrelated row quality: %s", reason)
		}
	}
	oldBarrier := time.Now().Add(-time.Minute)
	if reason := workerDeletedGPUFrame(deletionFrame(oldBarrier.Add(time.Millisecond)), instance, oldBarrier, time.Now()); reason != "stale_collection" {
		t.Fatalf("old collected_at accepted: %s", reason)
	}
}

func deletionDB(t *testing.T, handler http.Handler, intent, operation string) (*gorm.DB, *platform.App, platform.Job) {
	t.Helper()
	db, a, job, _, _ := creationCleanupDB(t, handler)
	job.Type = "instance.delete"
	job.Parameters = json.RawMessage(`{}`)
	updates := []struct {
		table string
		where string
		value platform.Row
	}{
		{"instances", "id=15", platform.Row{"incus_uuid": "target-uuid", "observed_state": "stopped", "desired_state": "deleted", "provisioning_state": "deleting"}},
		{"tasks", "id=43", platform.Row{"task_type": "instance.delete", "parameters_redacted": "{}", "lease_until": a.Time().Add(5 * time.Minute)}},
		{"usage_sessions", "instance_id=15", platform.Row{"metering_state": "held"}},
		{"gpu_allocations", "id=1", platform.Row{"status": "active", "configured": true, "observed": true}},
		{"ip_addresses", "id=1", platform.Row{"status": "bound", "bound_at": a.Time()}},
	}
	for _, update := range updates {
		if err := db.Table(update.table).Where(update.where).Updates(update.value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if intent != "" {
		if err := db.Table("task_steps").Create(&platform.Row{"task_id": job.ID, "step_key": "delete.native", "attempt": 1, "status": intent, "operation_id": operation, "parameters_redacted": "{}"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db, a, job
}

func assertDeletionReservations(t *testing.T, db *gorm.DB, released bool) {
	t.Helper()
	instance, err := platform.Get(db, "instances", 15, false)
	if err != nil {
		t.Fatal(err)
	}
	ip, err := platform.Get(db, "ip_addresses", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	gpu, err := platform.Get(db, "gpu_allocations", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	bucket, err := platform.Get(db, "credit_buckets", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	var activeUsage, heldCredit, ledger, quotaInstances, charges int64
	for _, check := range []struct {
		table string
		where string
		count *int64
	}{
		{"usage_sessions", "status='active'", &activeUsage},
		{"credit_reservations", "status='held'", &heldCredit},
		{"credit_ledger", "1=1", &ledger},
		{"instances", "deleted_at IS NULL", &quotaInstances},
		{"usage_charges", "1=1", &charges},
	} {
		if err := db.Table(check.table).Where(check.where).Count(check.count).Error; err != nil {
			t.Fatal(err)
		}
	}
	if released {
		if platform.Text(instance["observed_state"]) != "deleted" || platform.Text(instance["provisioning_state"]) != "deleted" || instance["deleted_at"] == nil || platform.Text(ip["status"]) != "available" || ip["instance_id"] != nil || platform.Text(gpu["status"]) != "released" || !platform.Bool(gpu["release_verified"]) || gpu["active_slot"] != nil || platform.Text(bucket["reserved_pt"]) != "0.000000" || activeUsage != 0 || heldCredit != 0 || ledger != 2 || quotaInstances != 0 || charges != 0 {
			t.Fatalf("release incomplete: instance=%+v ip=%+v gpu=%+v bucket=%+v usage=%d credit=%d ledger=%d quota=%d charges=%d", instance, ip, gpu, bucket, activeUsage, heldCredit, ledger, quotaInstances, charges)
		}
	} else if instance["deleted_at"] != nil || platform.Text(ip["status"]) != "bound" || platform.Uint(ip["instance_id"]) != 15 || platform.Text(gpu["status"]) != "active" || platform.Bool(gpu["release_verified"]) || platform.Uint(gpu["active_slot"]) != 1 || platform.Text(bucket["reserved_pt"]) != "2.000000" || activeUsage != 2 || heldCredit != 2 || ledger != 0 || quotaInstances != 1 || charges != 0 {
		t.Fatalf("unsafe partial release: instance=%+v ip=%+v gpu=%+v bucket=%+v usage=%d credit=%d ledger=%d quota=%d charges=%d", instance, ip, gpu, bucket, activeUsage, heldCredit, ledger, quotaInstances, charges)
	}
}

func TestMySQLDeleteAwaitsPostConfirmationSampleAndNeverReplays(t *testing.T) {
	for _, name := range []string{"fresh_delete", "expired_operation_reconcile", "durable_intent_without_operation"} {
		recovery := name != "fresh_delete"
		t.Run(name, func(t *testing.T) {
			var absent atomic.Bool
			absent.Store(recovery)
			var deletes, metrics, waits atomic.Int64
			cached := deletionFrame(time.Now().Add(-time.Second))
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/agent/v1/instances/metrics" && r.URL.Query().Get("project") != "*" {
					http.Error(w, "reclamation requires the complete host inventory", http.StatusBadRequest)
					return
				}
				switch {
				case r.Method == http.MethodDelete:
					deletes.Add(1)
					absent.Store(true)
					_ = json.NewEncoder(w).Encode(platform.Row{"type": "async", "operation": "/1.0/operations/delete-op"})
				case strings.HasPrefix(r.URL.Path, "/1.0/operations/"):
					waits.Add(1)
					if recovery {
						http.Error(w, "operation expired", http.StatusNotFound)
						return
					}
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status_code": 200}})
				case r.URL.Path == "/1.0/instances":
					list := []any{}
					if !absent.Load() {
						list = append(list, platform.Row{"name": "test-pending", "config": platform.Row{"volatile.uuid": "target-uuid"}})
					}
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": list})
				case r.URL.Path == "/agent/v1/instances/metrics":
					if metrics.Add(1) == 1 {
						_ = json.NewEncoder(w).Encode(cached)
					} else {
						_ = json.NewEncoder(w).Encode(deletionFrame(time.Now()))
					}
				case strings.HasSuffix(r.URL.Path, "/state"):
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status": "Stopped"}})
				default:
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"name": "test-pending", "config": platform.Row{"volatile.uuid": "target-uuid"}}})
				}
			})
			intent, operation := "", ""
			if recovery {
				intent = "running"
				if name == "expired_operation_reconcile" {
					intent, operation = "waiting", "/1.0/operations/expired?project=default"
				}
			}
			db, a, job := deletionDB(t, handler, intent, operation)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if recovery {
				if err := db.Table("tasks").Where("id=43").Updates(platform.Row{"status": "unknown", "lease_owner": nil}).Error; err != nil {
					t.Fatal(err)
				}
				reconcile := platform.Row{"id": 44, "task_type": "instance.reconcile", "user_id": 2, "node_id": 1, "instance_id": 15, "status": "running", "parameters_redacted": `{"source_task_id":"43"}`, "lease_generation": 1, "lease_until": a.Time().Add(time.Minute), "result_redacted": "{}"}
				if err := db.Table("tasks").Create(&reconcile).Error; err != nil {
					t.Fatal(err)
				}
				job.ID, job.Type, job.Parameters = 44, "instance.reconcile", json.RawMessage(`{"source_task_id":"43"}`)
				if err := runReconcile(ctx, a, job); err != nil {
					t.Fatal(err)
				}
				source, err := platform.Get(db, "tasks", 43, false)
				if err != nil || platform.Text(source["status"]) != "succeeded" {
					t.Fatalf("source not reconciled: %+v %v", source, err)
				}
			} else if err := runDelete(ctx, a, job); err != nil {
				t.Fatal(err)
			}
			assertDeletionReservations(t, db, true)
			for _, stage := range []string{"delete.verify_reclamation", "delete.finalize"} {
				step, err := workerStep(ctx, a, job, stage)
				if err != nil || platform.Text(step["status"]) != "succeeded" {
					t.Fatalf("completed stage left unfinished: %s %+v %v", stage, step, err)
				}
			}
			wantWaits := int64(1)
			if name == "durable_intent_without_operation" {
				wantWaits = 0
			}
			if metrics.Load() < 2 || waits.Load() != wantWaits || (!recovery && deletes.Load() != 1) || (recovery && deletes.Load() != 0) {
				t.Fatalf("wrong dispatch/evidence counts: deletes=%d metrics=%d waits=%d", deletes.Load(), metrics.Load(), waits.Load())
			}
			beforeMetrics, beforeWaits, beforeDeletes := metrics.Load(), waits.Load(), deletes.Load()
			if recovery {
				if err := runReconcile(ctx, a, job); err == nil {
					t.Fatal("duplicate reconciliation unexpectedly reclaimed a completed source")
				}
			}
			// An independently retried delete/reconcile handler sees terminal deletion
			// and must not dispatch, settle usage or release any reservation twice.
			job.Type = "instance.delete"
			if err := runDelete(ctx, a, job); err != nil {
				t.Fatal(err)
			}
			assertDeletionReservations(t, db, true)
			if metrics.Load() != beforeMetrics || waits.Load() != beforeWaits || deletes.Load() != beforeDeletes {
				t.Fatal("terminal deletion repeated native work or evidence collection")
			}
		})
	}
}

func TestMySQLDeleteUnverifiedEvidenceRetainsEveryReservation(t *testing.T) {
	cases := append(deletionUnsafeFrames(), deletionFrameCase{"stale_timestamp_predating_confirmation", "collection_predates_deletion", func(f platform.Row, barrier time.Time) {
		f["collection_started_at"] = barrier.Add(time.Millisecond).Format(time.RFC3339Nano)
		f["collected_at"] = barrier.Add(2 * time.Millisecond).Format(time.RFC3339Nano)
	}})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mutations, metrics atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations.Add(1)
					http.Error(w, "unexpected mutation", 500)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/agent/v1/instances/metrics" {
					metrics.Add(1)
					frame := deletionFrame(time.Now())
					// The barrier used for invalid-frame edits is safely before the
					// actual deletion confirmation, not an invented future timestamp.
					tc.change(frame, time.Now().Add(-time.Minute))
					_ = json.NewEncoder(w).Encode(frame)
					return
				}
				_ = json.NewEncoder(w).Encode(platform.Row{"metadata": []any{}})
			})
			db, a, job := deletionDB(t, handler, "succeeded", "")
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			err := runDelete(ctx, a, job)
			public := platform.PublicError(err, true)
			if err == nil || public["code"] != "gpu_reclamation_unverified" || metrics.Load() == 0 || mutations.Load() != 0 {
				t.Fatalf("unsafe outcome/cause: err=%v public=%+v metrics=%d mutations=%d", err, public, metrics.Load(), mutations.Load())
			}
			if _, known := err.(*platform.HTTPError); known {
				t.Fatal("unverified release became a replayable known failure")
			}
			details := object(public["details"])
			if details["stage"] != "delete.verify_reclamation" || details["native_deletion_confirmed"] != true || details["resource_reservations_released"] != false || platform.Text(details["action"]) == "" {
				t.Fatalf("safe actionable cause missing: %+v", public)
			}
			stored, err := platform.Get(db, "tasks", job.ID, false)
			if err != nil || platform.Text(stored["phase"]) != "delete.verify_reclamation" {
				t.Fatalf("task still claims native phase: %+v %v", stored, err)
			}
			assertDeletionReservations(t, db, false)
		})
	}
}

func TestMySQLDeleteAbsentWithoutIntentNeverReleases(t *testing.T) {
	var mutations, metrics atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations.Add(1)
		}
		if r.URL.Path == "/agent/v1/instances/metrics" {
			metrics.Add(1)
		}
		_ = json.NewEncoder(w).Encode(platform.Row{"metadata": []any{}})
	})
	db, a, job := deletionDB(t, handler, "", "")
	if err := runDelete(context.Background(), a, job); err == nil {
		t.Fatal("absence without durable intent authorized release")
	}
	assertDeletionReservations(t, db, false)
	if mutations.Load() != 0 || metrics.Load() != 0 {
		t.Fatal("missing intent triggered native work or reclamation wait")
	}
}

func TestMySQLDeleteWithoutActiveGPUSkipsMetrics(t *testing.T) {
	var metrics atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/v1/instances/metrics" {
			metrics.Add(1)
			http.Error(w, "metrics unavailable", 503)
			return
		}
		_ = json.NewEncoder(w).Encode(platform.Row{"metadata": []any{}})
	})
	db, a, job := deletionDB(t, handler, "succeeded", "")
	if err := db.Table("gpu_allocations").Where("id=1").Update("status", "released").Error; err != nil {
		t.Fatal(err)
	}
	if err := runDelete(context.Background(), a, job); err != nil {
		t.Fatal(err)
	}
	instance, err := platform.Get(db, "instances", job.InstanceID, false)
	if err != nil || platform.Text(instance["observed_state"]) != "deleted" || metrics.Load() != 0 {
		t.Fatalf("GPU-free deletion waited on metrics: instance=%+v metrics=%d err=%v", instance, metrics.Load(), err)
	}
}
