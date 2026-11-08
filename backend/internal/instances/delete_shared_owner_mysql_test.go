package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/platform"
)

// Migrated disposable MySQL, loopback DELETE admission, production worker and
// pinned TLS Agent transport. CUDA ownership/memory are protocol fixtures, not
// hardware observations; deletion releases only the absent target's reservation.
func TestMySQLDeleteSharedOwnerHTTPWorkerTLS(t *testing.T) {
	for _, scenario := range []string{"fresh_delete", "verify_unknown_reconcile", "unknown_context", "reference_scan_failed", "target_still_present"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPlanConfigurationFixture(t)
			owners := make([]platform.Row, 0, 2)
			for i, uuid := range []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"} {
				body := f.createBody("1", "2.5")
				body["name"] = fmt.Sprintf("delete-shared-owner-%d", i)
				created := f.call(false, http.MethodPost, "/instances", body, 0, http.StatusAccepted, "")
				f.exec("UPDATE tasks SET status='succeeded',finished_at=NOW(6) WHERE id=?", created["task_id"])
				f.exec(`UPDATE instances SET incus_uuid=?,provisioning_state='ready',observed_state='stopped',observed_at=NOW(6),allowed_user_actions='["delete"]' WHERE id=?`, uuid, created["instance_id"])
				owners = append(owners, f.instance(platform.Uint(created["instance_id"])))
			}
			target := owners[0]
			// Same native name in a different project must not look like the target.
			f.exec("UPDATE instances SET project_name='cuda-project',incus_name=?,desired_state='running',observed_state='running' WHERE id=?", target["incus_name"], owners[1]["id"])
			other := f.instance(platform.Uint(owners[1]["id"]))
			f.exec("INSERT INTO node_gpus(id,node_id,uuid,model,memory_mib,max_allocations,status,last_seen_at) VALUES(1,1,'GPU-shared-delete','NVIDIA A100',81920,8,'available',NOW(6))")
			for i, owner := range []platform.Row{target, other} {
				f.exec("INSERT INTO gpu_allocations(id,instance_id,user_id,node_id,gpu_id,gpu_price_version_id,gpu_hourly_pt,status,access_mode,reserved_at,configured,observed) VALUES(?,?,2,1,1,1,2,'active','shared',NOW(6),true,true)", i+1, owner["id"])
				if err := billing.InstanceUsage(t.Context(), f.db, owner, "gpu_attached", []uint64{uint64(i + 1)}); err != nil {
					t.Fatal(err)
				}
				f.exec("UPDATE ip_addresses SET status='bound',bound_at=NOW(6) WHERE instance_id=?", owner["id"])
			}
			otherBefore := deletionOwnerResources(t, f, platform.Uint(other["id"]))
			targetBefore := deletionOwnerResources(t, f, platform.Uint(target["id"]))
			otherInstanceBefore := jsonText(f.instance(platform.Uint(other["id"])))
			var bucketBefore platform.Row
			if err := f.db.Table("credit_buckets").Where("account_id=1").Take(&bucketBefore).Error; err != nil {
				t.Fatal(err)
			}
			var absent, recovered atomic.Bool
			var deletes, metrics, waits atomic.Int64
			cachedStart := time.Now().Add(-time.Second)
			f.native = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/agent/v1/instances/metrics" {
					if request.Method != http.MethodGet || request.URL.Query().Get("project") != "*" || !absent.Load() {
						t.Errorf("metrics must follow native absence and enumerate every project: %s %s", request.Method, request.URL)
						http.Error(w, "invalid evidence request", 500)
						return
					}
					n := metrics.Add(1)
					frame := sharedDeletionFrame(time.Now())
					holder := object(rows(frame["instances"])[0])
					holder["uuid"], holder["name"], holder["project"] = other["incus_uuid"], other["incus_name"], other["project_name"]
					frame["instances"] = []any{holder}
					if scenario == "fresh_delete" && n == 1 {
						frame["collection_started_at"] = cachedStart.Format(time.RFC3339Nano)
					}
					if !recovered.Load() {
						switch scenario {
						case "verify_unknown_reconcile", "reference_scan_failed":
							frame["host_quality"] = platform.Row{"attribution_complete": false, "context_attribution_complete": false, "reasons": []string{"gpu_shared_control_references", "gpu_reference_scan_incomplete"}}
						case "unknown_context":
							object(frame["anonymous"])["unattributed_contexts"] = 1
							frame["host_quality"] = platform.Row{"attribution_complete": false, "context_attribution_complete": false, "reasons": []string{"gpu_shared_control_references", "unattributed_gpu_contexts"}}
						case "target_still_present":
							frame["instances"] = append(rows(frame["instances"]), platform.Row{"uuid": target["incus_uuid"], "name": target["incus_name"], "project": target["project_name"]})
						}
					}
					_ = json.NewEncoder(w).Encode(frame)
					return
				}
				if request.URL.Query().Get("project") != platform.Text(target["project_name"]) {
					t.Errorf("native deletion escaped frozen project: %s", request.URL)
					http.Error(w, "wrong project", 500)
					return
				}
				guest := platform.Row{"name": target["incus_name"], "config": platform.Row{"volatile.uuid": target["incus_uuid"], "user.101cloud.instance_id": platform.Text(target["id"])}}
				switch {
				case request.Method == http.MethodDelete && request.URL.Path == "/1.0/instances/"+platform.Text(target["incus_name"]):
					deletes.Add(1)
					absent.Store(true)
					_ = json.NewEncoder(w).Encode(platform.Row{"type": "async", "operation": "/1.0/operations/shared-delete"})
				case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/1.0/operations/shared-delete"):
					waits.Add(1)
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status_code": 200}})
				case request.Method == http.MethodGet && request.URL.Path == "/1.0/instances":
					list := []any{}
					if !absent.Load() {
						list = append(list, guest)
					}
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": list})
				case request.Method == http.MethodGet && request.URL.Path == "/1.0/instances/"+platform.Text(target["incus_name"])+"/state":
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status": "Stopped"}})
				case request.Method == http.MethodGet && request.URL.Path == "/1.0/instances/"+platform.Text(target["incus_name"]):
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": guest})
				default:
					t.Errorf("unexpected shared-delete native request: %s %s", request.Method, request.URL)
					http.Error(w, "unexpected operation", 500)
				}
			})
			h := newMemberCleanupHTTP(t, f)
			status, envelope := h.request(http.MethodDelete, fmt.Sprintf("/instances/%v", target["id"]), jsonText(platform.Row{"confirm_name": target["logical_name"], "delete_data": true}), fmt.Sprintf(`"rev-%d"`, platform.Uint(target["revision"])), "ordinary-shared-delete", h.csrf, h.server.URL)
			if status != http.StatusAccepted {
				t.Fatalf("ordinary DELETE admission failed: %d %+v", status, envelope)
			}
			taskID := platform.Uint(object(envelope["data"])["task_id"])
			stored := forceStopStored(t, f, taskID)
			if platform.Bool(object(stored["parameters_redacted"])["cleanup_stuck"]) {
				t.Fatal("ordinary deletion was routed through stuck cleanup")
			}
			a := platform.New(f.db, f.a.Config)
			// Bound negative evidence waits without changing production polling.
			a.RegisterJob("instance.delete", func(ctx context.Context, a *platform.App, job platform.Job) error {
				ctx, cancel := context.WithTimeout(ctx, 1800*time.Millisecond)
				defer cancel()
				return runDelete(ctx, a, job)
			})
			a.RegisterJob("instance.reconcile", runReconcile)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { a.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			task := awaitSharedDeletionTask(t, f, taskID)
			if deletes.Load() != 1 || metrics.Load() == 0 || waits.Load() != 1 {
				t.Fatalf("native/evidence dispatch mismatch: deletes=%d metrics=%d waits=%d", deletes.Load(), metrics.Load(), waits.Load())
			}
			if scenario != "fresh_delete" {
				if platform.Text(task["status"]) != "unknown" || platform.Text(task["error_code"]) != "gpu_reclamation_unverified" || platform.Text(task["phase"]) != "delete.verify_reclamation" {
					t.Fatalf("unsafe evidence did not retain actionable verify state: %+v", task)
				}
				if f.instance(platform.Uint(target["id"]))["deleted_at"] != nil || deletionOwnerResources(t, f, platform.Uint(target["id"])) != targetBefore {
					t.Fatal("unverified deletion released IP/GPU/usage/credit reservations")
				}
				var bucket platform.Row
				if err := f.db.Table("credit_buckets").Where("account_id=1").Take(&bucket).Error; err != nil || jsonText(bucket) != jsonText(bucketBefore) {
					t.Fatalf("unverified deletion changed the credit bucket: %+v %v", bucket, err)
				}
				if scenario == "verify_unknown_reconcile" {
					recovered.Store(true)
					out := f.call(true, http.MethodPost, fmt.Sprintf("/tasks/%d/reconcile", taskID), platform.Row{}, platform.Uint(task["revision"]), http.StatusAccepted, "")
					wrapper := awaitSharedDeletionTask(t, f, platform.Uint(out["task_id"]))
					task = forceStopStored(t, f, taskID)
					if platform.Text(wrapper["status"]) != "succeeded" || platform.Text(task["status"]) != "succeeded" || deletes.Load() != 1 {
						t.Fatalf("verify reconciliation repeated DELETE or failed: wrapper=%+v source=%+v deletes=%d", wrapper, task, deletes.Load())
					}
				}
			}
			if scenario == "fresh_delete" || scenario == "verify_unknown_reconcile" {
				if platform.Text(task["status"]) != "succeeded" || (scenario == "fresh_delete" && metrics.Load() < 2) {
					t.Fatalf("post-confirmation sample did not complete ordinary deletion: %+v metrics=%d", task, metrics.Load())
				}
				assertSharedDeletionReleased(t, f, platform.Uint(target["id"]))
				beforeLedger, beforeMetrics := f.count("credit_ledger"), metrics.Load()
				before := deletionOwnerResources(t, f, platform.Uint(target["id"]))
				job := platform.Job{ID: taskID, Type: "instance.delete", UserID: 2, NodeID: 1, InstanceID: platform.Uint(target["id"]), Parameters: json.RawMessage(`{}`)}
				if err := runDelete(t.Context(), a, job); err != nil || before != deletionOwnerResources(t, f, job.InstanceID) || beforeLedger != f.count("credit_ledger") || beforeMetrics != metrics.Load() || deletes.Load() != 1 {
					t.Fatalf("terminal retry repeated release, billing or native work: %v", err)
				}
			}
			if otherBefore != deletionOwnerResources(t, f, platform.Uint(other["id"])) || otherInstanceBefore != jsonText(f.instance(platform.Uint(other["id"]))) {
				t.Fatal("deleting target changed the active other-project owner's allocation, IP, usage or credit reservations")
			}
			var quota int64
			if err := f.db.Table("instances").Where("deleted_at IS NULL").Count(&quota).Error; err != nil {
				t.Fatal(err)
			}
			wantQuota := int64(2)
			if scenario == "fresh_delete" || scenario == "verify_unknown_reconcile" {
				wantQuota = 1
			}
			if quota != wantQuota {
				t.Fatalf("quota occupancy=%d want=%d", quota, wantQuota)
			}
		})
	}
}

func awaitSharedDeletionTask(t *testing.T, f *planConfigurationFixture, id uint64) platform.Row {
	t.Helper()
	deadline := time.After(10 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		task := forceStopStored(t, f, id)
		switch platform.Text(task["status"]) {
		case "succeeded", "failed", "unknown", "cancelled":
			return task
		}
		select {
		case <-deadline:
			t.Fatalf("shared-delete worker did not settle: %+v", task)
		case <-ticker.C:
		}
	}
}

func deletionOwnerResources(t *testing.T, f *planConfigurationFixture, id uint64) string {
	t.Helper()
	out := platform.Row{}
	for _, table := range []string{"gpu_allocations", "ip_addresses", "usage_sessions"} {
		var entries []platform.Row
		if err := f.db.Table(table).Where("instance_id=?", id).Order("id").Find(&entries).Error; err != nil {
			t.Fatal(err)
		}
		out[table] = entries
	}
	var reservations []platform.Row
	if err := f.db.Table("credit_reservations").Where("session_id IN (SELECT id FROM usage_sessions WHERE instance_id=?)", id).Order("id").Find(&reservations).Error; err != nil {
		t.Fatal(err)
	}
	out["credit_reservations"] = reservations
	return jsonText(out)
}

func assertSharedDeletionReleased(t *testing.T, f *planConfigurationFixture, id uint64) {
	t.Helper()
	var allocation platform.Row
	if err := f.db.Table("gpu_allocations").Where("instance_id=?", id).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	if platform.Text(allocation["status"]) != "released" || !platform.Bool(allocation["release_verified"]) || platform.Bool(allocation["configured"]) || platform.Bool(allocation["observed"]) || allocation["active_slot"] != nil || allocation["released_at"] == nil || allocation["last_verified_at"] == nil || f.instance(id)["deleted_at"] == nil {
		t.Fatalf("target allocation/tombstone not atomically released: %+v", allocation)
	}
	for _, query := range []struct{ table, where string }{
		{"ip_addresses", "instance_id=?"},
		{"usage_sessions", "instance_id=? AND status='active'"},
		{"credit_reservations", "session_id IN (SELECT id FROM usage_sessions WHERE instance_id=?) AND status='held'"},
	} {
		var count int64
		if err := f.db.Table(query.table).Where(query.where, id).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("target reservations remain in %s: %d %v", query.table, count, err)
		}
	}
}
