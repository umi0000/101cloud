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

func sharedDeletionFrame(started time.Time) platform.Row {
	frame := deletionFrame(started)
	q := platform.Row{"attribution_complete": false, "context_attribution_complete": true, "reasons": []string{"gpu_shared_control_references", "gpu_utilization_no_samples"}}
	frame["host_quality"] = q
	frame["anonymous"] = platform.Row{"unattributed_contexts": 0, "unresolved_gpu_references": 0, "unattributed_activity_samples": 0}
	object(rows(frame["instances"])[0])["quality"] = platform.Row{"attribution_complete": false, "context_attribution_complete": false, "reasons": []string{"instance_not_running"}}
	frame["instances"] = append([]any{platform.Row{
		"uuid": "active-owner-uuid", "name": "test-pending", "project": "cuda-project", "state": "Running", "quality": q,
		"gpus": []any{platform.Row{"uuid": "GPU-shared-delete", "configured": true, "observed": true, "process_count": 1, "used_memory_bytes": 805306368, "referencing_process_count": nil, "quality": q}},
	}}, rows(frame["instances"])...)
	return frame
}

func sharedDeletionUnsafeFrames() []deletionFrameCase {
	host := func(f platform.Row) platform.Row { return object(f["host_quality"]) }
	holder := func(f platform.Row) platform.Row { return object(rows(f["instances"])[0]) }
	cases := []deletionFrameCase{
		{"shared_missing_host_full", "incomplete_host_attribution", func(f platform.Row, _ time.Time) { delete(host(f), "attribution_complete") }},
		{"shared_missing_host_context", "incomplete_host_attribution", func(f platform.Row, _ time.Time) { delete(host(f), "context_attribution_complete") }},
		{"shared_string_host_context", "incomplete_host_attribution", func(f platform.Row, _ time.Time) { host(f)["context_attribution_complete"] = "true" }},
		{"shared_false_host_context", "incomplete_host_attribution", func(f platform.Row, _ time.Time) { host(f)["context_attribution_complete"] = false }},
		{"shared_missing_host_reasons", "incomplete_host_attribution", func(f platform.Row, _ time.Time) { delete(host(f), "reasons") }},
		{"shared_unknown_host_reason", "incomplete_host_attribution", func(f platform.Row, _ time.Time) {
			host(f)["reasons"] = []string{"gpu_shared_control_references", "future_reason"}
		}},
		{"shared_unresolved_reference", "incomplete_host_attribution", func(f platform.Row, _ time.Time) {
			host(f)["reasons"] = []string{"gpu_shared_control_references", "gpu_references_unresolved"}
		}},
		{"shared_failed_reference_scan", "incomplete_host_attribution", func(f platform.Row, _ time.Time) {
			host(f)["reasons"] = []string{"gpu_shared_control_references", "gpu_reference_scan_incomplete"}
		}},
		{"shared_metric_only", "incomplete_host_attribution", func(f platform.Row, _ time.Time) { host(f)["reasons"] = []string{"gpu_utilization_no_samples"} }},
		{"shared_string_reasons", "incomplete_host_attribution", func(f platform.Row, _ time.Time) { host(f)["reasons"] = `["gpu_shared_control_references"]` }},
		{"shared_nonstring_reason", "incomplete_host_attribution", func(f platform.Row, _ time.Time) { host(f)["reasons"] = []any{"gpu_shared_control_references", true} }},
		{"shared_missing_anonymous", "unverified_anonymous_gpu_evidence", func(f platform.Row, _ time.Time) { delete(f, "anonymous") }},
		{"shared_no_holders", "missing_shared_gpu_holder", func(f platform.Row, _ time.Time) { f["instances"] = []any{} }},
		{"shared_only_stopped_unknown", "missing_shared_gpu_holder", func(f platform.Row, _ time.Time) { f["instances"] = rows(f["instances"])[1:] }},
		{"shared_holder_missing_quality", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) { delete(holder(f), "quality") }},
		{"shared_holder_context_missing", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) {
			holder(f)["quality"] = platform.Row{"attribution_complete": false, "reasons": []string{"gpu_shared_control_references"}}
		}},
		{"shared_holder_context_string", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) {
			holder(f)["quality"] = platform.Row{"attribution_complete": false, "context_attribution_complete": "true", "reasons": []string{"gpu_shared_control_references"}}
		}},
		{"shared_holder_unknown_references", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) {
			holder(f)["quality"] = platform.Row{"attribution_complete": false, "context_attribution_complete": true, "reasons": []string{"gpu_shared_control_references", "gpu_reference_scan_incomplete"}}
		}},
		{"shared_holder_no_shared_reason", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) {
			holder(f)["quality"] = platform.Row{"attribution_complete": false, "context_attribution_complete": true, "reasons": []string{"gpu_utilization_no_samples"}}
		}},
		{"shared_holder_inconsistent_full", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) {
			holder(f)["quality"] = platform.Row{"attribution_complete": true, "context_attribution_complete": true, "reasons": []string{"gpu_shared_control_references"}}
		}},
		{"shared_holder_missing_identity", "incomplete_instance_identity", func(f platform.Row, _ time.Time) { delete(holder(f), "uuid") }},
		{"shared_target_survives", "target_still_observed", func(f platform.Row, _ time.Time) { holder(f)["uuid"] = "target-uuid" }},
		{"shared_target_name_project_survives", "target_still_observed", func(f platform.Row, _ time.Time) { holder(f)["project"] = "default" }},
		{"shared_predelete_frame", "collection_predates_deletion", func(f platform.Row, barrier time.Time) { f["collection_started_at"] = barrier.Format(time.RFC3339Nano) }},
		{"shared_stale_frame", "stale_collection", func(f platform.Row, _ time.Time) { f["stale"] = true }},
		{"shared_holder_stopped", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) { holder(f)["state"] = "Stopped" }},
		{"shared_holder_unknown_state", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) { holder(f)["state"] = "unknown" }},
		{"shared_holder_missing_state", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) { delete(holder(f), "state") }},
		{"shared_duplicate_uuid", "ambiguous_instance_inventory", func(f platform.Row, _ time.Time) { object(rows(f["instances"])[1])["uuid"] = holder(f)["uuid"] }},
		{"shared_duplicate_name_project", "ambiguous_instance_inventory", func(f platform.Row, _ time.Time) {
			row := object(rows(f["instances"])[1])
			row["name"], row["project"] = holder(f)["name"], holder(f)["project"]
		}},
		{"shared_stopped_missing_quality", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) { delete(object(rows(f["instances"])[1]), "quality") }},
		{"shared_stopped_missing_reasons", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) {
			delete(object(object(rows(f["instances"])[1])["quality"]), "reasons")
		}},
		{"shared_stopped_malformed_reasons", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) {
			object(object(rows(f["instances"])[1])["quality"])["reasons"] = "instance_not_running"
		}},
		{"shared_stopped_unknown_references", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) {
			object(object(rows(f["instances"])[1])["quality"])["reasons"] = []string{"instance_not_running", "gpu_references_unresolved"}
		}},
		{"shared_stopped_failed_process_scan", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) {
			object(object(rows(f["instances"])[1])["quality"])["reasons"] = []string{"instance_not_running", "process_scan_incomplete"}
		}},
		{"shared_stopped_unknown_reason", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) {
			object(object(rows(f["instances"])[1])["quality"])["reasons"] = []string{"instance_not_running", "future_reason"}
		}},
		{"shared_running_unknown_references", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) {
			other := object(rows(f["instances"])[1])
			other["state"] = "Running"
			other["quality"] = platform.Row{"attribution_complete": true, "reasons": []string{"gpu_references_unresolved"}}
		}},
		{"shared_running_missing_reasons", "unverified_shared_gpu_holder", func(f platform.Row, _ time.Time) {
			other := object(rows(f["instances"])[1])
			other["state"] = "Running"
			other["quality"] = platform.Row{"attribution_complete": true}
		}},
	}
	for _, counter := range []string{"unattributed_contexts", "unresolved_gpu_references", "unattributed_activity_samples"} {
		cases = append(cases, deletionFrameCase{"shared_missing_" + counter, "unverified_anonymous_gpu_evidence", func(f platform.Row, _ time.Time) { delete(object(f["anonymous"]), counter) }})
		for _, invalid := range []any{nil, "0", false, -1, 1, 0.5, json.Number("1e-999")} {
			cases = append(cases, deletionFrameCase{"shared_" + counter + "_" + jsonText(invalid), "unverified_anonymous_gpu_evidence", func(f platform.Row, _ time.Time) { object(f["anonymous"])[counter] = invalid }})
		}
	}
	// The common unsafe-evidence tests start from the legacy healthy frame;
	// replace it with the shared-owner protocol fixture before each mutation.
	for i, tc := range cases {
		change := tc.change
		cases[i].change = func(f platform.Row, barrier time.Time) {
			shared := sharedDeletionFrame(barrier.Add(time.Millisecond))
			shared["collection_started_at"], shared["collected_at"] = f["collection_started_at"], f["collected_at"]
			for key, value := range shared {
				f[key] = value
			}
			change(f, barrier)
		}
	}
	return cases
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

func TestDeletedGPUFrameAllowsOnlyAttributedSurvivingSharedOwners(t *testing.T) {
	barrier := time.Now().Add(-time.Second)
	target := platform.Row{"incus_uuid": "target-uuid", "incus_name": "test-pending", "project_name": "default"}
	for _, tc := range sharedDeletionUnsafeFrames() {
		t.Run(tc.name, func(t *testing.T) {
			frame := deletionFrame(barrier.Add(time.Millisecond))
			tc.change(frame, barrier)
			if reason := workerDeletedGPUFrame(frame, target, barrier, time.Now()); reason != tc.reason {
				t.Fatalf("reason=%q want=%q", reason, tc.reason)
			}
		})
	}
	for _, metric := range []string{"gpu_metric_unavailable", "gpu_utilization_unknown", "gpu_utilization_no_samples", "gpu_utilization_stale", "gpu_utilization_identity_unconfirmed"} {
		t.Run(metric, func(t *testing.T) {
			frame := sharedDeletionFrame(barrier.Add(time.Millisecond))
			object(frame["host_quality"])["reasons"] = []string{"gpu_shared_control_references", metric}
			// Exercise the production Agent decoder's json.Number representation.
			var decoded platform.Row
			decoder := json.NewDecoder(strings.NewReader(jsonText(frame)))
			decoder.UseNumber()
			if err := decoder.Decode(&decoded); err != nil {
				t.Fatal(err)
			}
			if reason := workerDeletedGPUFrame(decoded, target, barrier, time.Now()); reason != "" {
				t.Fatalf("known surviving owner rejected: %s", reason)
			}
		})
	}
	for _, state := range []string{"Running", "running", "Frozen", "frozen"} {
		frame := sharedDeletionFrame(barrier.Add(time.Millisecond))
		object(rows(frame["instances"])[0])["state"] = state
		if reason := workerDeletedGPUFrame(frame, target, barrier, time.Now()); reason != "" {
			t.Fatalf("known %s holder rejected: %s", state, reason)
		}
	}
	for _, value := range []any{json.Number("0"), json.Number("0.0"), json.Number("0e2"), float64(0), 0} {
		if !workerDeletedNumericZero(value) {
			t.Fatalf("numeric zero rejected: %T %v", value, value)
		}
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
		if platform.Text(instance["observed_state"]) != "deleted" || platform.Text(instance["provisioning_state"]) != "deleted" || instance["deleted_at"] == nil || platform.Text(ip["status"]) != "available" || ip["instance_id"] != nil || platform.Text(gpu["status"]) != "released" || platform.Bool(gpu["configured"]) || platform.Bool(gpu["observed"]) || !platform.Bool(gpu["release_verified"]) || gpu["released_at"] == nil || gpu["last_verified_at"] == nil || gpu["active_slot"] != nil || platform.Text(bucket["reserved_pt"]) != "0.000000" || activeUsage != 0 || heldCredit != 0 || ledger != 2 || quotaInstances != 0 || charges != 0 {
			t.Fatalf("release incomplete: instance=%+v ip=%+v gpu=%+v bucket=%+v usage=%d credit=%d ledger=%d quota=%d charges=%d", instance, ip, gpu, bucket, activeUsage, heldCredit, ledger, quotaInstances, charges)
		}
	} else if instance["deleted_at"] != nil || platform.Text(ip["status"]) != "bound" || platform.Uint(ip["instance_id"]) != 15 || platform.Text(gpu["status"]) != "active" || !platform.Bool(gpu["configured"]) || !platform.Bool(gpu["observed"]) || platform.Bool(gpu["release_verified"]) || platform.Uint(gpu["active_slot"]) != 1 || platform.Text(bucket["reserved_pt"]) != "2.000000" || activeUsage != 2 || heldCredit != 2 || ledger != 0 || quotaInstances != 1 || charges != 0 {
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
	cases = append(cases, sharedDeletionUnsafeFrames()...)
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
