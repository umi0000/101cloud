package instances

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

func stuckDeleteBody(r platform.Row) platform.Row {
	return platform.Row{"mode": "force", "confirm_data_loss_risk": true, "confirm_name": r["logical_name"], "delete_data": true, "cleanup_stuck": true}
}

func stuckDeleteFixture(t *testing.T) (*planConfigurationFixture, platform.Row, platform.Row) {
	t.Helper()
	f := newPlanConfigurationFixture(t)
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	r := f.instance(platform.Uint(created["instance_id"]))
	old, err := platform.Get(f.db, "tasks", platform.Uint(created["task_id"]), false)
	if err != nil {
		t.Fatal(err)
	}
	return f, r, old
}

func memberStuckDeleteFixture(t *testing.T) (*planConfigurationFixture, platform.Row, platform.Row) {
	t.Helper()
	f, r, old := stuckDeleteFixture(t)
	f.exec("UPDATE instances SET allowed_user_actions=JSON_ARRAY('delete') WHERE id=?", r["id"])
	return f, f.instance(platform.Uint(r["id"])), old
}

func stuckDeleteClaim(t *testing.T, f *planConfigurationFixture, id uint64) platform.Job {
	t.Helper()
	f.exec("UPDATE tasks SET status='running',lease_owner='cleanup-test',lease_until=?,lease_generation=lease_generation+1,attempt_count=attempt_count+1 WHERE id=?", f.a.Time().Add(5*time.Minute), id)
	r, err := platform.Get(f.db, "tasks", id, false)
	if err != nil {
		t.Fatal(err)
	}
	return platform.Job{ID: id, Type: platform.Text(r["task_type"]), UserID: platform.Uint(r["user_id"]), NodeID: platform.Uint(r["node_id"]), InstanceID: platform.Uint(r["instance_id"]), Parameters: json.RawMessage(platform.Text(r["parameters_redacted"])), LeaseGeneration: platform.Uint(r["lease_generation"])}
}

func stuckDeleteAccept(f *planConfigurationFixture, r platform.Row) uint64 {
	f.t.Helper()
	out := f.call(false, http.MethodDelete, fmt.Sprintf("/instances/%d", platform.Uint(r["id"])), stuckDeleteBody(r), platform.Uint(r["revision"]), http.StatusAccepted, "")
	return platform.Uint(out["task_id"])
}

func TestMySQLStuckDeleteAdmissionAndTakeover(t *testing.T) {
	for _, admin := range []bool{false, true} {
		for _, status := range []string{"queued", "running", "waiting", "unknown", "cancelled"} {
			t.Run(fmt.Sprintf("admin=%v/%s", admin, status), func(t *testing.T) {
				f, r, old := stuckDeleteFixture(t)
				if !admin {
					f.exec("UPDATE instances SET allowed_user_actions=JSON_ARRAY('delete') WHERE id=?", r["id"])
				}
				f.exec("UPDATE tasks SET status=?,finished_at=IF(?='cancelled',NOW(6),NULL),lease_owner='old-worker',lease_until=NOW(6)+INTERVAL 10 MINUTE,lease_generation=4 WHERE id=?", status, status, old["id"])
				if status == "cancelled" {
					f.exec("UPDATE tasks SET lease_owner=NULL,lease_until=NULL,error_code='task_force_stopped',result_redacted=JSON_OBJECT('force_stop',JSON_OBJECT('requires_reconciliation',true,'resources_retained',true)) WHERE id=?", old["id"])
				}
				f.exec("UPDATE instances SET provisioning_state='unknown',observed_at=NOW(6)-INTERVAL 1 DAY WHERE id=?", r["id"])
				f.exec("INSERT INTO task_steps(task_id,step_key,attempt,status,parameters_redacted,operation_id,node_id,project_name) VALUES(?,'create.native',4,'waiting',JSON_OBJECT(),'/1.0/operations/pending-create?project=default',1,'default')", old["id"])
				wrapped, err := platform.Enqueue(f.db, "instance.reconcile", 2, 1, platform.Uint(r["id"]), false, platform.Row{"source_task_id": old["id"]})
				if err != nil {
					t.Fatal(err)
				}
				path := fmt.Sprintf("/instances/%d", platform.Uint(r["id"]))
				if admin {
					path += "/delete"
				}
				out := f.call(admin, httpMethodDelete(admin), path, stuckDeleteBody(r), platform.Uint(r["revision"]), http.StatusAccepted, "")
				id := platform.Uint(out["task_id"])
				if id == 0 || id == platform.Uint(old["id"]) {
					t.Fatal("cleanup did not create its own persistent task")
				}
				for _, previous := range []uint64{platform.Uint(old["id"]), platform.Uint(wrapped["task_id"])} {
					task, err := platform.Get(f.db, "tasks", previous, false)
					retainedStop := status == "cancelled" && previous == platform.Uint(old["id"])
					code := "superseded_by_cleanup"
					if retainedStop {
						code = "task_force_stopped"
					}
					if err != nil || platform.Text(task["status"]) != "cancelled" || platform.Text(task["error_code"]) != code || task["lease_owner"] != nil || task["lease_until"] != nil || (!retainedStop && previous == platform.Uint(old["id"]) && platform.Uint(task["lease_generation"]) <= 4) || (retainedStop && platform.Bool(task["is_instance_mutation"])) {
						t.Fatalf("old task not durably revoked: %+v %v", task, err)
					}
				}
				var step platform.Row
				if err := f.db.Table("task_steps").Where("task_id=?", old["id"]).Take(&step).Error; err != nil || platform.Text(step["operation_id"]) == "" || (status != "cancelled" && platform.Text(step["status"]) != "cancelled") {
					t.Fatalf("takeover lost native journal: %+v %v", step, err)
				}
				current := f.instance(platform.Uint(r["id"]))
				if platform.Text(current["desired_state"]) != "deleted" || current["deleted_at"] != nil {
					t.Fatal("admission prematurely released the managed instance")
				}
				replay := f.call(admin, httpMethodDelete(admin), path, stuckDeleteBody(current), platform.Uint(current["revision"]), http.StatusAccepted, "")
				if platform.Uint(replay["task_id"]) != id {
					t.Fatal("active cleanup replay created another task")
				}
			})
		}
	}
}

func httpMethodDelete(admin bool) string {
	if admin {
		return http.MethodPost
	}
	return http.MethodDelete
}

func TestMySQLStuckDeleteKeepsMemberAndNormalAdmissionGates(t *testing.T) {
	f, r, _ := stuckDeleteFixture(t)
	path := fmt.Sprintf("/instances/%d", platform.Uint(r["id"]))
	f.exec("UPDATE instances SET allowed_user_actions=JSON_ARRAY() WHERE id=?", r["id"])
	f.call(false, http.MethodDelete, path, stuckDeleteBody(r), platform.Uint(r["revision"]), http.StatusForbidden, "action_not_allowed")
	f.call(true, http.MethodDelete, path, platform.Row{"confirm_name": r["logical_name"], "delete_data": true}, platform.Uint(r["revision"]), http.StatusConflict, "instance_not_ready")
	for _, field := range []string{"mode", "confirm_data_loss_risk", "confirm_name", "delete_data"} {
		body := stuckDeleteBody(r)
		delete(body, field)
		f.call(true, http.MethodDelete, path, body, platform.Uint(r["revision"]), http.StatusUnprocessableEntity, "invalid_request")
	}
	if f.count("tasks") != 1 || platform.Text(f.instance(platform.Uint(r["id"]))["desired_state"]) != "running" {
		t.Fatal("rejected cleanup mutated durable state")
	}
	// Delete authorization does not authorize standalone force actions.
	f.exec("UPDATE tasks SET status='succeeded',finished_at=NOW(6) WHERE instance_id=?", r["id"])
	f.exec("UPDATE instances SET allowed_user_actions=JSON_ARRAY('delete','stop','restart'),provisioning_state='ready',observed_state='running',observed_at=NOW(6) WHERE id=?", r["id"])
	body := stuckDeleteBody(r)
	delete(body, "cleanup_stuck")
	f.call(false, http.MethodDelete, path, body, platform.Uint(r["revision"]), http.StatusForbidden, "force_not_allowed")
	for _, action := range []string{"stop", "restart"} {
		f.call(false, http.MethodPost, path+"/"+action, platform.Row{"mode": "force", "confirm_data_loss_risk": true}, platform.Uint(r["revision"]), http.StatusForbidden, "force_not_allowed")
	}
	body["cleanup_stuck"] = true
	body["mode"] = "graceful"
	f.call(false, http.MethodDelete, path, body, platform.Uint(r["revision"]), http.StatusUnprocessableEntity, "invalid_request")
}

func stuckNativeAbsent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/1.0/operations":
		_ = json.NewEncoder(w).Encode(platform.Row{"type": "sync", "metadata": platform.Row{"running": []any{}}})
	case r.Method == http.MethodGet && r.URL.Path == "/1.0/instances":
		_ = json.NewEncoder(w).Encode(platform.Row{"type": "sync", "metadata": []any{}})
	default:
		http.Error(w, "unexpected mutation", http.StatusInternalServerError)
	}
}

func TestMySQLStuckDeleteInitiallyAbsentTombstoneLedgerAndStaleWorkers(t *testing.T) {
	f, r, old := memberStuckDeleteFixture(t)
	var mutations atomic.Int64
	f.native = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			mutations.Add(1)
		}
		stuckNativeAbsent(w, request)
	})
	f.exec("UPDATE tasks SET status='unknown',lease_generation=3 WHERE id=?", old["id"])
	beforeSessions, beforeLedger := f.count("usage_sessions"), f.count("credit_ledger")
	job := stuckDeleteClaim(t, f, stuckDeleteAccept(f, r))
	if err := runDelete(t.Context(), f.a, job); err != nil {
		t.Fatal(err)
	}
	deleted := f.instance(platform.Uint(r["id"]))
	if deleted["deleted_at"] == nil || platform.Text(deleted["observed_state"]) != "deleted" || f.count("instances") != 1 || f.count("usage_sessions") != beforeSessions || f.count("credit_ledger") < beforeLedger {
		t.Fatalf("deletion lost tombstone or financial history: %+v", deleted)
	}
	var active, bound, held int64
	f.db.Table("usage_sessions").Where("instance_id=? AND status='active'", r["id"]).Count(&active)
	f.db.Table("ip_addresses").Where("instance_id=?", r["id"]).Count(&bound)
	f.db.Table("credit_reservations").Where("status='held'").Count(&held)
	if active != 0 || bound != 0 || held != 0 || mutations.Load() != 0 {
		t.Fatalf("absent cleanup did not release once without native mutations: active=%d ip=%d held=%d mutations=%d", active, bound, held, mutations.Load())
	}
	ledger := f.count("credit_ledger")
	if err := runDelete(t.Context(), f.a, job); err != nil || f.count("credit_ledger") != ledger {
		t.Fatalf("cleanup replay changed financial history: %v", err)
	}
	stale := platform.Job{ID: platform.Uint(old["id"]), Type: "instance.create", UserID: 2, NodeID: 1, InstanceID: platform.Uint(r["id"]), Parameters: json.RawMessage(`{}`), LeaseGeneration: 3}
	_ = runCreate(t.Context(), f.a, stale)
	_ = runLifecycle(t.Context(), f.a, platform.Job{ID: stale.ID, Type: "instance.start", UserID: 2, NodeID: 1, InstanceID: stale.InstanceID, Parameters: stale.Parameters, LeaseGeneration: 3})
	if err := workerConfirm(t.Context(), f.a, stale, r, "running", "running", platform.Row{"provisioning_state": "ready"}); err == nil {
		t.Fatal("stale worker regained SQL authority")
	}
	if f.instance(stale.InstanceID)["deleted_at"] == nil || mutations.Load() != 0 {
		t.Fatal("stale worker resurrected deleted instance")
	}
	f.call(true, http.MethodGet, "/instances", nil, 0, http.StatusOK, "")
}

func TestMySQLStuckDeleteOfflineAndIdentityConflictsRetainReservations(t *testing.T) {
	for _, scenario := range []string{"offline", "forbidden", "wrong_uuid", "missing_marker", "wrong_marker"} {
		t.Run(scenario, func(t *testing.T) {
			f, r, _ := memberStuckDeleteFixture(t)
			if scenario != "missing_marker" {
				f.exec("UPDATE instances SET incus_uuid='managed-uuid' WHERE id=?", r["id"])
				r = f.instance(platform.Uint(r["id"]))
			}
			var mutations atomic.Int64
			f.native = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet {
					mutations.Add(1)
				}
				if scenario == "offline" || scenario == "forbidden" {
					status := http.StatusServiceUnavailable
					if scenario == "forbidden" {
						status = http.StatusForbidden
					}
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(platform.Row{"type": "error", "error_code": status})
					return
				}
				cfg := platform.Row{"volatile.uuid": "managed-uuid"}
				if scenario == "wrong_uuid" {
					cfg["volatile.uuid"] = "other-uuid"
				}
				if scenario == "wrong_marker" {
					cfg["user.101cloud.instance_id"] = "999999"
				}
				guest := platform.Row{"name": r["incus_name"], "config": cfg}
				metadata := any(guest)
				if request.URL.Path == "/1.0/instances" {
					metadata = []any{guest}
				} else if request.URL.Path == "/1.0/operations" {
					metadata = platform.Row{"running": []any{}}
				}
				_ = json.NewEncoder(w).Encode(platform.Row{"type": "sync", "metadata": metadata})
			})
			job := stuckDeleteClaim(t, f, stuckDeleteAccept(f, r))
			if err := runDelete(t.Context(), f.a, job); err == nil {
				t.Fatal("uncertain/conflicting native state released instance")
			}
			var ips, sessions int64
			f.db.Table("ip_addresses").Where("instance_id=?", r["id"]).Count(&ips)
			f.db.Table("usage_sessions").Where("instance_id=? AND status='active'", r["id"]).Count(&sessions)
			if f.instance(job.InstanceID)["deleted_at"] != nil || ips == 0 || sessions == 0 || mutations.Load() != 0 {
				t.Fatal("unsafe cleanup changed reservations or native guest")
			}
		})
	}
}

func TestMySQLStuckDeleteLostSubmissionFailsActionablyAndKnownOperationRecovers(t *testing.T) {
	f, r, old := memberStuckDeleteFixture(t)
	f.exec("UPDATE tasks SET status='unknown',lease_generation=2 WHERE id=?", old["id"])
	f.exec("INSERT INTO task_steps(task_id,step_key,attempt,status,parameters_redacted,result_redacted) VALUES(?,'create.native',2,'unknown',JSON_OBJECT(),JSON_OBJECT('native_dispatch_fenced',true,'native_dispatch_started',true))", old["id"])
	var cancels atomic.Int64
	var terminal atomic.Bool
	f.native = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/1.0/operations/recovered") {
			code := 103
			if request.Method == http.MethodDelete {
				cancels.Add(1)
				terminal.Store(true)
			}
			if terminal.Load() {
				code = 401
			}
			_ = json.NewEncoder(w).Encode(platform.Row{"type": "sync", "metadata": platform.Row{"status_code": code, "may_cancel": true}})
			return
		}
		stuckNativeAbsent(w, request)
	})
	job := stuckDeleteClaim(t, f, stuckDeleteAccept(f, r))
	err := runDelete(t.Context(), f.a, job)
	var he *platform.HTTPError
	if !errors.As(err, &he) || he.Code != "cleanup_native_submission_unknown" || f.instance(job.InstanceID)["deleted_at"] != nil {
		t.Fatalf("lost submission did not preserve actionable uncertainty: %v", err)
	}
	// Simulate restoration from authoritative native operation logs, then recover
	// under a fresh reconciliation lease without reviving the original task.
	f.exec("UPDATE task_steps SET operation_id='/1.0/operations/recovered?project=default',node_id=1,project_name='default' WHERE task_id=?", old["id"])
	f.exec("UPDATE tasks SET status='unknown',lease_owner=NULL,lease_until=NULL WHERE id=?", job.ID)
	recovery, err := platform.Enqueue(f.db, "instance.reconcile", job.UserID, job.NodeID, job.InstanceID, false, platform.Row{"source_task_id": job.ID})
	if err != nil {
		t.Fatal(err)
	}
	wrapper := stuckDeleteClaim(t, f, platform.Uint(recovery["task_id"]))
	if err := runReconcile(t.Context(), f.a, wrapper); err != nil || cancels.Load() != 1 || !terminal.Load() || f.instance(job.InstanceID)["deleted_at"] == nil {
		t.Fatalf("known operation recovery failed: cancels=%d err=%v", cancels.Load(), err)
	}
}

func TestMySQLStuckDeleteDispatchFenceSerializesInflightCreate(t *testing.T) {
	f, r, old := memberStuckDeleteFixture(t)
	oldJob := stuckDeleteClaim(t, f, platform.Uint(old["id"]))
	if err := f.a.Step(t.Context(), oldJob, "create.native", "", "running"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var creates atomic.Int64
	f.native = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path == "/1.0/instances" {
			creates.Add(1)
			close(entered)
			<-release
			_ = json.NewEncoder(w).Encode(platform.Row{"type": "async", "operation": "/1.0/operations/inflight"})
			return
		}
		if strings.HasPrefix(request.URL.Path, "/1.0/operations/inflight") {
			_ = json.NewEncoder(w).Encode(platform.Row{"type": "sync", "metadata": platform.Row{"status_code": 200}})
			return
		}
		stuckNativeAbsent(w, request)
	})
	dispatched := make(chan error, 1)
	go func() {
		_, err := f.a.Agent(platform.WithTaskDispatch(t.Context(), oldJob, "create.native"), 1, http.MethodPost, "/1.0/instances?project=default", platform.Row{"name": r["incus_name"]})
		dispatched <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("native dispatch did not reach isolated fixture")
	}
	accepted := make(chan platform.Row, 1)
	failed := make(chan error, 1)
	go func() {
		err := f.db.Transaction(func(db *gorm.DB) error {
			if err := lockOwnerAccount(db, 2); err != nil {
				return err
			}
			current, err := platform.Get(db, "instances", oldJob.InstanceID, true)
			if err != nil {
				return err
			}
			out, err := admitStuckDelete(f.a, db, current)
			if err == nil {
				accepted <- out
			}
			return err
		})
		failed <- err
	}()
	var out platform.Row
	select {
	case out = <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("takeover could not revoke orchestration while native response was pending")
	}
	if err := <-failed; err != nil {
		t.Fatal(err)
	}
	job := stuckDeleteClaim(t, f, platform.Uint(out["task_id"]))
	if err := runDelete(t.Context(), f.a, job); err == nil || platform.PublicError(err, true)["code"] != "cleanup_native_submission_unknown" {
		t.Fatalf("cleanup passed unidentified in-flight CREATE: %v", err)
	}
	if f.instance(job.InstanceID)["deleted_at"] != nil {
		t.Fatal("unidentified submission released instance before reconciliation")
	}
	close(release)
	if err := <-dispatched; err != nil {
		t.Fatal(err)
	}
	var journal platform.Row
	if err := f.db.Table("task_steps").Where("task_id=? AND step_key='create.native'", oldJob.ID).Take(&journal).Error; err != nil || platform.Text(journal["operation_id"]) != "/1.0/operations/inflight?project=default" {
		t.Fatalf("late operation identity was not retained on revoked attempt: %+v %v", journal, err)
	}
	if err := runDelete(t.Context(), f.a, job); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"create.native", "state.start"} {
		_, err := f.a.Agent(platform.WithTaskDispatch(context.Background(), oldJob, action), 1, http.MethodPost, "/1.0/instances?project="+url.QueryEscape(platform.Text(r["project_name"])), platform.Row{})
		if err == nil {
			t.Fatal("revoked worker dispatched a new native mutation")
		}
	}
	if creates.Load() != 1 || f.instance(job.InstanceID)["deleted_at"] == nil {
		t.Fatal("old native dispatcher resurrected deleted instance")
	}
}

func TestMySQLMemberStuckDeleteRunningTargetReleasesGPUWithoutHostMetrics(t *testing.T) {
	f, r, old := memberStuckDeleteFixture(t)
	f.exec("UPDATE tasks SET status='unknown',lease_generation=3 WHERE id=?", old["id"])
	f.exec("UPDATE instances SET incus_uuid=NULL,observed_state='running',provisioning_state='unknown' WHERE id=?", r["id"])
	f.exec("INSERT INTO node_gpus(node_id,uuid,model,memory_mib,max_allocations,status,last_seen_at) VALUES(1,'GPU-cleanup','NVIDIA A100',81920,8,'available',NOW(6))")
	f.exec("INSERT INTO gpu_allocations(instance_id,user_id,node_id,gpu_id,gpu_price_version_id,gpu_hourly_pt,status,reserved_at,configured,observed) SELECT ?,2,1,id,1,2,'active',NOW(6),true,true FROM node_gpus WHERE uuid='GPU-cleanup'", r["id"])
	r = f.instance(platform.Uint(r["id"]))
	var stopped, gone atomic.Bool
	var stops, deletes, metrics atomic.Int64
	f.native = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		metadata := any(platform.Row{})
		guest := platform.Row{"name": r["incus_name"], "config": platform.Row{"volatile.uuid": "managed-uuid", "user.101cloud.instance_id": platform.Text(r["id"])}}
		switch {
		case strings.HasPrefix(request.URL.Path, "/agent/v1/instances/metrics"):
			metrics.Add(1)
			http.Error(w, "host metrics deliberately unavailable", http.StatusServiceUnavailable)
			return
		case request.Method == http.MethodGet && request.URL.Path == "/1.0/operations":
			metadata = platform.Row{"running": []any{}}
		case strings.HasPrefix(request.URL.Path, "/1.0/operations/"):
			metadata = platform.Row{"status_code": 200}
		case request.Method == http.MethodGet && request.URL.Path == "/1.0/instances":
			metadata = []any{guest}
			if gone.Load() {
				metadata = []any{}
			}
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/state"):
			state := "Running"
			if stopped.Load() {
				state = "Stopped"
			}
			metadata = platform.Row{"status": state}
		case request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/state"):
			var body platform.Row
			_ = json.NewDecoder(request.Body).Decode(&body)
			if body["action"] != "stop" || !platform.Bool(body["force"]) {
				t.Error("cleanup did not request confirmed force stop")
			}
			stops.Add(1)
			stopped.Store(true)
			_ = json.NewEncoder(w).Encode(platform.Row{"type": "async", "operation": "/1.0/operations/stop"})
			return
		case request.Method == http.MethodDelete:
			if !stopped.Load() {
				t.Error("delete preceded stop confirmation")
			}
			deletes.Add(1)
			gone.Store(true)
			_ = json.NewEncoder(w).Encode(platform.Row{"type": "async", "operation": "/1.0/operations/delete"})
			return
		case request.Method == http.MethodGet:
			metadata = guest
		default:
			t.Errorf("unexpected native request: %s %s", request.Method, request.URL)
		}
		_ = json.NewEncoder(w).Encode(platform.Row{"type": "sync", "metadata": metadata})
	})
	h := newMemberCleanupHTTP(t, f)
	path := fmt.Sprintf("/instances/%d", platform.Uint(r["id"]))
	status, accepted := h.request(http.MethodDelete, path, jsonText(stuckDeleteBody(r)), fmt.Sprintf(`"rev-%d"`, platform.Uint(r["revision"])), "running-member-cleanup", h.csrf, h.server.URL)
	if status != http.StatusAccepted {
		t.Fatalf("authenticated running cleanup: %d %+v", status, accepted)
	}
	job := stuckDeleteClaim(t, f, platform.Uint(object(accepted["data"])["task_id"]))
	if err := runDelete(t.Context(), f.a, job); err != nil {
		t.Fatal(err)
	}
	var allocation platform.Row
	if err := f.db.Table("gpu_allocations").Where("instance_id=?", r["id"]).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 1 || deletes.Load() != 1 || metrics.Load() != 0 || platform.Text(allocation["status"]) != "released" || !platform.Bool(allocation["release_verified"]) || f.instance(job.InstanceID)["deleted_at"] == nil {
		t.Fatalf("target absence did not suffice for cleanup: stops=%d deletes=%d metrics=%d allocation=%+v", stops.Load(), deletes.Load(), metrics.Load(), allocation)
	}
	if err := runDelete(t.Context(), f.a, job); err != nil || stops.Load() != 1 || deletes.Load() != 1 {
		t.Fatalf("worker replay repeated destructive operations: %v", err)
	}
	status, detail := h.request(http.MethodGet, path, "", "", "", "", "")
	if status != http.StatusNotFound {
		t.Fatalf("confirmed absent instance remained member-visible: %d %+v", status, detail)
	}
	var active, bound, held int64
	f.db.Table("usage_sessions").Where("instance_id=? AND status='active'", r["id"]).Count(&active)
	f.db.Table("ip_addresses").Where("instance_id=?", r["id"]).Count(&bound)
	f.db.Table("credit_reservations").Where("status='held'").Count(&held)
	if active != 0 || bound != 0 || held != 0 {
		t.Fatalf("confirmed absence retained resources: sessions=%d ip=%d held=%d", active, bound, held)
	}
	stale := platform.Job{ID: platform.Uint(old["id"]), Type: "instance.start", UserID: 2, NodeID: 1, InstanceID: job.InstanceID, Parameters: json.RawMessage(`{}`), LeaseGeneration: 3}
	if err := workerConfirm(t.Context(), f.a, stale, r, "running", "running", nil); err == nil {
		t.Fatal("revoked worker mutated deleted instance")
	}
	if _, err := f.a.Agent(platform.WithTaskDispatch(t.Context(), stale, "state.start"), 1, http.MethodPut, "/1.0/instances/"+platform.Text(r["incus_name"])+"/state?project=default", platform.Row{"action": "start"}); err == nil || stops.Load() != 1 || deletes.Load() != 1 {
		t.Fatalf("revoked worker regained native mutation authority: %v", err)
	}
}

func TestMySQLStuckDeleteRequiresHTTPAndIncus404Agreement(t *testing.T) {
	for _, tc := range []struct {
		name      string
		httpCode  int
		incusCode int
		success   bool
	}{
		{"authoritative_absent", 404, 404, true},
		{"forbidden_is_not_absent", 403, 404, false},
		{"wrong_envelope_is_not_absent", 404, 403, false},
		{"error_envelope_with_success_status", 200, 404, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, r, _ := memberStuckDeleteFixture(t)
			var mutations atomic.Int64
			f.native = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet {
					mutations.Add(1)
				}
				if request.URL.Path == "/1.0/operations" {
					stuckNativeAbsent(w, request)
					return
				}
				if request.URL.Path == "/1.0/instances" {
					w.WriteHeader(http.StatusServiceUnavailable)
					_ = json.NewEncoder(w).Encode(platform.Row{"type": "error", "error_code": 503})
					return
				}
				w.WriteHeader(tc.httpCode)
				_ = json.NewEncoder(w).Encode(platform.Row{"type": "error", "error_code": tc.incusCode})
			})
			job := stuckDeleteClaim(t, f, stuckDeleteAccept(f, r))
			err := runDelete(t.Context(), f.a, job)
			if (err == nil) != tc.success || (f.instance(job.InstanceID)["deleted_at"] != nil) != tc.success || mutations.Load() != 0 {
				t.Fatalf("native absence classification mismatch: success=%v err=%v mutations=%d", tc.success, err, mutations.Load())
			}
		})
	}
}

func TestMySQLStuckDeleteNeverTreatsCancelAcknowledgementAsQuiescence(t *testing.T) {
	f, r, old := memberStuckDeleteFixture(t)
	f.exec("UPDATE tasks SET status='unknown' WHERE id=?", old["id"])
	f.exec("INSERT INTO task_steps(task_id,step_key,attempt,status,parameters_redacted,operation_id,node_id,project_name) VALUES(?,'create.native',1,'unknown',JSON_OBJECT(),'/1.0/operations/still-pending?project=default',1,'default')", old["id"])
	var cancels atomic.Int64
	f.native = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/1.0/operations/still-pending") {
			if request.Method == http.MethodDelete {
				cancels.Add(1)
			}
			_ = json.NewEncoder(w).Encode(platform.Row{"type": "sync", "metadata": platform.Row{"status_code": 103, "may_cancel": true}})
			return
		}
		stuckNativeAbsent(w, request)
	})
	job := stuckDeleteClaim(t, f, stuckDeleteAccept(f, r))
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := runDelete(ctx, f.a, job); err == nil || cancels.Load() != 1 || f.instance(job.InstanceID)["deleted_at"] != nil {
		t.Fatalf("pending CREATE was treated as gone: cancellations=%d err=%v", cancels.Load(), err)
	}
	var ips int64
	if err := f.db.Table("ip_addresses").Where("instance_id=?", r["id"]).Count(&ips).Error; err != nil || ips == 0 {
		t.Fatalf("pending native mutation released IP reservation: count=%d err=%v", ips, err)
	}
}
