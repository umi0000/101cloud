package instances

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/platform"
)

// gpuStopFixture retains an independently billed old card while a rebind owns
// only its releasing source and unconfigured reserved target.
func gpuStopFixture(t *testing.T) (*planConfigurationFixture, uint64, uint64) {
	t.Helper()
	f := newPlanConfigurationFixture(t)
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	id := platform.Uint(created["instance_id"])
	f.exec("UPDATE tasks SET status='succeeded',finished_at=NOW(6) WHERE id=?", created["task_id"])
	f.exec("UPDATE instances SET provisioning_state='ready',observed_state='running',incus_uuid='gpu-stop-uuid',observed_at=NOW(6) WHERE id=?", id)
	for i := 1; i <= 3; i++ {
		f.exec("INSERT INTO node_gpus(id,node_id,uuid,model,memory_mib,max_allocations,status,last_seen_at) VALUES(?,1,?,'NVIDIA A100',81920,8,'available',NOW(6))", i, fmt.Sprintf("GPU-stop-%d", i))
		f.exec("INSERT INTO gpu_allocations(id,instance_id,user_id,node_id,gpu_id,gpu_price_version_id,gpu_hourly_pt,status,access_mode,reserved_at,configured,observed) VALUES(?,?,2,1,?,1,2,'reserved','shared',NOW(6),false,false)", i, id, i)
	}
	r := f.instance(id)
	if err := billing.InstanceUsage(t.Context(), f.db, r, "gpu_reserved", []uint64{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE gpu_allocations SET status='active',configured=true,observed=true WHERE id IN (1,3)")
	if err := billing.InstanceUsage(t.Context(), f.db, r, "gpu_attached", []uint64{1, 3}); err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE gpu_allocations SET status='releasing',release_requested_at=NOW(6),release_reason='rebind' WHERE id=1")
	queued, err := platform.Enqueue(f.db, "instance.gpu", 2, 1, id, true, platform.Row{"action": "rebind", "allocation_ids": []uint64{1}, "target_ids": []uint64{2}, "allow_live": true, "initiated_by_user_id": "2"})
	if err != nil {
		t.Fatal(err)
	}
	return f, id, platform.Uint(queued["task_id"])
}

func assertGPUStopResources(t *testing.T, f *planConfigurationFixture, safe bool) {
	t.Helper()
	for _, tc := range []struct {
		id           uint64
		safe, unsafe string
	}{{1, "active", "releasing"}, {2, "cancelled", "reserved"}, {3, "active", "active"}} {
		r, err := platform.Get(f.db, "gpu_allocations", tc.id, false)
		want := tc.unsafe
		if safe {
			want = tc.safe
		}
		if err != nil || platform.Text(r["status"]) != want {
			t.Fatalf("allocation %d: %+v err=%v want=%s", tc.id, r, err, want)
		}
		if tc.id != 2 && (!platform.Bool(r["configured"]) || !platform.Bool(r["observed"]) || r["released_at"] != nil) {
			t.Fatalf("old physical card changed: %+v", r)
		}
	}
	var sessions []platform.Row
	if err := f.db.Table("usage_sessions").Where("gpu_allocation_id IN (1,2,3)").Order("gpu_allocation_id").Find(&sessions).Error; err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 3 {
		t.Fatalf("GPU billing history lost: %+v", sessions)
	}
	for _, s := range sessions {
		ended := safe && platform.Uint(s["gpu_allocation_id"]) == 2
		if (s["ended_at"] != nil) != ended || (platform.Text(s["status"]) == "ended") != ended {
			t.Fatalf("incorrect usage rollback: %+v", s)
		}
	}
	if safe {
		var held int64
		if err := f.db.Table("credit_reservations r").Joins("JOIN usage_sessions s ON s.id=r.session_id").Where("s.gpu_allocation_id=2 AND r.status='held'").Count(&held).Error; err != nil {
			t.Fatal(err)
		}
		if held != 0 {
			t.Fatal("unconsumed target reservation retained")
		}
	}
}

func TestMySQLGPUForceStopPredispatchResources(t *testing.T) {
	for _, state := range []string{"queued", "running", "unknown"} {
		t.Run(state, func(t *testing.T) {
			f, id, taskID := gpuStopFixture(t)
			if state != "queued" {
				f.exec("UPDATE tasks SET status=?,lease_generation=1,lease_owner='gpu-preflight',lease_until=? WHERE id=?", state, f.a.Time().Add(time.Minute), taskID)
			}
			if state == "running" {
				if err := f.a.Step(t.Context(), gpuStopJob(t, f, id, taskID), "gpu.preflight", "", "running"); err != nil {
					t.Fatal(err)
				}
			}
			if state == "unknown" {
				f.exec("UPDATE tasks SET error_code='observation_incomplete',result_redacted=? WHERE id=?", `{"error":{"code":"observation_incomplete","message":"实时 GPU 归属信息不完整，暂不能安全调整分配。"}}`, taskID)
			}
			var beforeLedger []platform.Row
			if err := f.db.Table("credit_ledger").Order("id").Find(&beforeLedger).Error; err != nil {
				t.Fatal(err)
			}
			w := forceStopHTTP(f, true, taskID, 1, "gpu-safe-"+state)
			if w.Code != http.StatusOK {
				t.Fatalf("stop: %d %s", w.Code, w.Body.String())
			}
			assertGPUStopResources(t, f, true)
			for _, old := range beforeLedger {
				current, err := platform.Get(f.db, "credit_ledger", platform.Uint(old["id"]), false)
				if err != nil || jsonText(old) != jsonText(current) {
					t.Fatalf("financial history rewritten: before=%+v after=%+v err=%v", old, current, err)
				}
			}
			view := f.call(false, http.MethodGet, fmt.Sprintf("/instances/%d", id), platform.Row{}, 0, http.StatusOK, "")
			if view["active_task_id"] != nil || view["active_task_status"] != nil {
				t.Fatalf("cancelled task remains active: %+v", view)
			}
			f.call(false, http.MethodPost, fmt.Sprintf("/instances/%d/stop", id), platform.Row{}, platform.Uint(f.instance(id)["revision"]), http.StatusAccepted, "")
		})
	}
}

func TestMySQLGPUForceStopAmbiguousDispatchRetainsReservations(t *testing.T) {
	for _, scenario := range []string{"dispatched", "unfenced_missing_journal"} {
		t.Run(scenario, func(t *testing.T) {
			f, id, taskID := gpuStopFixture(t)
			f.exec("UPDATE tasks SET status='unknown',lease_generation=1 WHERE id=?", taskID)
			if scenario == "unfenced_missing_journal" {
				f.exec("UPDATE tasks SET lease_generation=0 WHERE id=?", taskID)
			} else {
				step := platform.Row{"task_id": taskID, "step_key": "gpu.configure", "attempt": 1, "status": "unknown", "parameters_redacted": "{}", "result_redacted": `{"native_dispatch_started":true}`}
				if err := f.db.Table("task_steps").Create(&step).Error; err != nil {
					t.Fatal(err)
				}
			}
			w := forceStopHTTP(f, true, taskID, 1, "gpu-ambiguous-"+scenario)
			if w.Code != http.StatusOK {
				t.Fatalf("stop: %d %s", w.Code, w.Body.String())
			}
			assertGPUStopResources(t, f, false)
			if err := platform.TaskMutationBarrier(f.db, id); err == nil {
				t.Fatal("ambiguous GPU cancellation unlocked instance")
			}
			view := f.call(false, http.MethodGet, fmt.Sprintf("/instances/%d", id), platform.Row{}, 0, http.StatusOK, "")
			if platform.Text(view["active_task_id"]) != platform.Text(taskID) || platform.Text(view["active_task_status"]) != "cancelled" {
				t.Fatalf("barrier DTO status lost: %+v", view)
			}
		})
	}
}

func TestMySQLGPUForceStopPreservesConfiguredTargets(t *testing.T) {
	for _, status := range []string{"active", "reserved"} {
		t.Run(status, func(t *testing.T) {
			f, _, taskID := gpuStopFixture(t)
			f.exec("UPDATE gpu_allocations SET status=?,configured=true WHERE id=2", status)
			before, err := platform.Get(f.db, "gpu_allocations", 2, false)
			if err != nil {
				t.Fatal(err)
			}
			w := forceStopHTTP(f, true, taskID, 1, "gpu-configured-"+status)
			if w.Code != http.StatusOK {
				t.Fatalf("stop: %d %s", w.Code, w.Body.String())
			}
			after, err := platform.Get(f.db, "gpu_allocations", 2, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"status", "configured", "observed", "released_at", "revision"} {
				if jsonText(before[key]) != jsonText(after[key]) {
					t.Fatalf("configured target %s changed: before=%+v after=%+v", key, before, after)
				}
			}
			var session platform.Row
			if err := f.db.Table("usage_sessions").Where("gpu_allocation_id=2").Take(&session).Error; err != nil {
				t.Fatal(err)
			}
			if session["ended_at"] != nil {
				t.Fatalf("configured target billing ended: %+v", session)
			}
		})
	}
}

func gpuStopJob(t *testing.T, f *planConfigurationFixture, id, taskID uint64) platform.Job {
	t.Helper()
	r := forceStopStored(t, f, taskID)
	return platform.Job{ID: taskID, Type: "instance.gpu", UserID: 2, NodeID: 1, InstanceID: id, LeaseGeneration: platform.Uint(r["lease_generation"]), Parameters: json.RawMessage(platform.Text(r["parameters_redacted"]))}
}
