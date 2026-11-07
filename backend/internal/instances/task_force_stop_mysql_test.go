package instances

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
)

func forceStopHTTP(f *planConfigurationFixture, admin bool, id uint64, revision uint64, key string) *httptest.ResponseRecorder {
	prefix := "/api/v1"
	if admin {
		prefix += "/admin"
	}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("%s/tasks/%d/force-stop", prefix, id), strings.NewReader(`{"confirm":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, revision))
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

func forceStopStored(t *testing.T, f *planConfigurationFixture, id uint64) platform.Row {
	t.Helper()
	task, err := platform.Get(f.db, "tasks", id, false)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestMySQLForceStopEveryTaskTypeAndState(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	instanceID := platform.Uint(created["instance_id"])
	rootID := platform.Uint(created["task_id"])
	stopped := forceStopHTTP(f, false, rootID, 1, "cancel-initial-create")
	if stopped.Code != http.StatusOK {
		t.Fatalf("stop create: %d %s", stopped.Code, stopped.Body.String())
	}
	kinds := []string{"instance.create", "instance.start", "instance.stop", "instance.restart", "instance.resize", "instance.delete", "instance.import", "instance.reconcile", "instance.gpu", "instance.creation_cleanup", "image.prepare", "node.probe", "node.drain", "network.validate", "monitoring.console.prepare", "credit_enforcement_reconcile"}
	for _, kind := range kinds {
		for _, state := range []string{"queued", "running", "waiting", "unknown"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				// Each case seeds a pre-existing scheduler row directly: prior
				// force-stopped cases intentionally fence admission of new work.
				queued := platform.Row{"task_type": kind, "user_id": 2, "node_id": 1, "instance_id": instanceID, "is_instance_mutation": false, "status": "queued", "parameters_redacted": jsonText(platform.Row{"source_task_id": rootID}), "idempotency_scope": "force-stop-fixture", "idempotency_key": kind + "-" + state}
				if err := f.db.Table("tasks").Create(&queued).Error; err != nil {
					t.Fatal(err)
				}
				id, err := platform.InsertedID(queued)
				if err != nil {
					t.Fatal(err)
				}
				changes := platform.Row{"status": state}
				if state == "running" {
					changes["lease_owner"] = "force-stop-test"
					changes["lease_until"] = f.a.Time().Add(time.Minute)
				}
				if err = f.db.Table("tasks").Where("id=?", id).Updates(changes).Error; err != nil {
					t.Fatal(err)
				}
				w := forceStopHTTP(f, true, id, 1, "stop-"+kind+"-"+state)
				if w.Code != http.StatusOK {
					t.Fatalf("force stop: %d %s", w.Code, w.Body.String())
				}
				task := forceStopStored(t, f, id)
				if platform.Text(task["status"]) != "cancelled" || platform.Text(task["error_code"]) != "task_force_stopped" || platform.Uint(task["lease_generation"]) != 1 || task["lease_owner"] != nil || task["lease_until"] != nil {
					t.Fatalf("not revoked: %+v", task)
				}
				var envelope platform.Row
				if err = json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				dto := object(envelope["data"])
				if !platform.Bool(object(dto["force_stop"])["orchestration_stopped"]) || w.Header().Get("ETag") != `"rev-2"` {
					t.Fatalf("not synchronous DTO/ETag: %s", w.Body.String())
				}
			})
		}
	}
}

func TestMySQLForceStopOwnershipOriginIdempotencyAndTerminalHistory(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	id := platform.Uint(created["task_id"])
	task := forceStopStored(t, f, id)
	if platform.Uint(object(task["parameters_redacted"])["initiated_by_user_id"]) != 2 {
		t.Fatalf("missing trusted actor: %+v", task)
	}
	f.requestUserID = 1
	w := forceStopHTTP(f, false, id, 1, "foreign-stop")
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-account stop: %d %s", w.Code, w.Body.String())
	}
	f.requestUserID = 2
	queued, err := platform.Enqueue(f.db, "instance.stop", 2, 1, platform.Uint(created["instance_id"]), false, platform.Row{"reason": "credit-enforcement:1"})
	if err != nil {
		t.Fatal(err)
	}
	w = forceStopHTTP(f, false, platform.Uint(queued["task_id"]), 1, "enforcement-stop")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "task_stop_not_allowed") {
		t.Fatalf("enforcement bypass: %d %s", w.Code, w.Body.String())
	}
	w = forceStopHTTP(f, false, id, 99, "stale-stop")
	if w.Code != http.StatusPreconditionFailed || platform.Text(forceStopStored(t, f, id)["status"]) != "queued" {
		t.Fatalf("stale revision changed task: %d %s", w.Code, w.Body.String())
	}
	w = forceStopHTTP(f, false, id, 1, "member-stop")
	if w.Code != http.StatusOK {
		t.Fatalf("member stop: %d %s", w.Code, w.Body.String())
	}
	first := forceStopStored(t, f, id)
	replay := forceStopHTTP(f, false, id, 1, "member-stop")
	var originalEnvelope, replayEnvelope platform.Row
	if err = json.Unmarshal(w.Body.Bytes(), &originalEnvelope); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(replay.Body.Bytes(), &replayEnvelope); err != nil {
		t.Fatal(err)
	}
	if replay.Code != http.StatusOK || jsonText(originalEnvelope["data"]) != jsonText(replayEnvelope["data"]) {
		t.Fatalf("idempotent replay mismatch: %d %s", replay.Code, replay.Body.String())
	}
	again := forceStopHTTP(f, false, id, 2, "new-terminal-stop")
	if again.Code != http.StatusOK {
		t.Fatalf("terminal no-op: %d %s", again.Code, again.Body.String())
	}
	after := forceStopStored(t, f, id)
	if platform.Uint(after["revision"]) != platform.Uint(first["revision"]) || platform.Text(after["result_redacted"]) != platform.Text(first["result_redacted"]) {
		t.Fatalf("terminal history rewritten: %+v", after)
	}
	// The stopped create retains IP/accounting admission, not an invented rollback.
	var reserved int64
	if err = f.db.Table("ip_addresses").Where("instance_id=? AND status='reserved'", created["instance_id"]).Count(&reserved).Error; err != nil {
		t.Fatal(err)
	}
	if reserved != 1 || !platform.Bool(first["is_instance_mutation"]) {
		t.Fatalf("reservations or barrier released: reserved=%d task=%+v", reserved, first)
	}
	out, err := platform.Enqueue(f.db, "instance.start", 2, 1, platform.Uint(created["instance_id"]), true, platform.Row{})
	if err == nil {
		t.Fatalf("database mutation barrier bypassed: %+v", out)
	}
	f.call(false, http.MethodPost, "/instances/"+platform.Text(created["instance_id"])+"/start", platform.Row{}, 1, http.StatusConflict, "task_reconciliation_required")
}

func TestMySQLForceStopRecoveryGraphAndConcurrentRequests(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	source, err := platform.Enqueue(f.db, "node.probe", 1, 1, 0, false, platform.Row{})
	if err != nil {
		t.Fatal(err)
	}
	sourceID := platform.Uint(source["task_id"])
	f.exec("UPDATE tasks SET status='unknown' WHERE id=?", sourceID)
	wrapper, err := platform.Enqueue(f.db, "instance.reconcile", 1, 1, 0, false, platform.Row{"source_task_id": sourceID})
	if err != nil {
		t.Fatal(err)
	}
	wrapperID := platform.Uint(wrapper["task_id"])
	f.exec("UPDATE tasks SET status='running',lease_generation=4,lease_owner='late-wrapper',lease_until=? WHERE id=?", f.a.Time().Add(time.Minute), wrapperID)
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- forceStopHTTP(f, true, sourceID, 1, fmt.Sprintf("concurrent-%d", i))
		}()
	}
	wg.Wait()
	close(results)
	ok, conflictCount := 0, 0
	for w := range results {
		switch w.Code {
		case http.StatusOK:
			ok++
		case http.StatusPreconditionFailed:
			conflictCount++
		default:
			t.Fatalf("concurrent stop: %d %s", w.Code, w.Body.String())
		}
	}
	if ok != 1 || conflictCount != 1 {
		t.Fatalf("concurrent results: ok=%d conflicts=%d", ok, conflictCount)
	}
	for _, id := range []uint64{sourceID, wrapperID} {
		if platform.Text(forceStopStored(t, f, id)["status"]) != "cancelled" {
			t.Fatalf("recovery source survived stop: %d", id)
		}
	}
	old := platform.Job{ID: wrapperID, Type: "node.probe", UserID: 1, NodeID: 1, LeaseGeneration: 4}
	if err = f.a.TaskResult(t.Context(), old, platform.Row{"late": true}); err == nil {
		t.Fatal("late worker result accepted")
	}
	if err = f.a.Step(t.Context(), old, "next.native", "", "running"); err == nil {
		t.Fatal("late worker next step accepted")
	}
}

func TestMySQLForceStopHistoricalInitiatorAudit(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	id := platform.Uint(created["task_id"])
	f.exec("UPDATE tasks SET parameters_redacted=JSON_REMOVE(parameters_redacted,'$.initiated_by_user_id') WHERE id=?", id)
	w := forceStopHTTP(f, false, id, 1, "historic-member-stop")
	if w.Code != http.StatusOK {
		t.Fatalf("proven historical task rejected: %d %s", w.Code, w.Body.String())
	}
}

func TestMySQLForceStoppedControlTaskFencesOtherInstanceWork(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	instanceID := platform.Uint(created["instance_id"])
	f.exec("UPDATE tasks SET status='succeeded',finished_at=? WHERE id=?", f.a.Time(), created["task_id"])
	control, err := platform.Enqueue(f.db, "instance.stop", 2, 1, instanceID, false, platform.Row{"reason": "credit-enforcement:1"})
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := platform.Enqueue(f.db, "instance.start", 2, 1, instanceID, false, platform.Row{})
	if err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE tasks SET status='unknown' WHERE id=?", control["task_id"])
	f.exec("UPDATE tasks SET status='running',lease_generation=1,lease_owner='pre-existing',lease_until=? WHERE id=?", f.a.Time().Add(time.Minute), sibling["task_id"])
	w := forceStopHTTP(f, true, platform.Uint(control["task_id"]), 1, "stop-control")
	if w.Code != http.StatusOK {
		t.Fatalf("stop control: %d %s", w.Code, w.Body.String())
	}
	stopped := forceStopStored(t, f, platform.Uint(control["task_id"]))
	if platform.Bool(stopped["is_instance_mutation"]) {
		t.Fatal("test did not cover a nonmutation control source")
	}
	if err = platform.TaskMutationBarrier(f.db, instanceID); err == nil {
		t.Fatal("control source lost its instance mutation barrier")
	}
	if _, err = platform.Enqueue(f.db, "instance.start", 2, 1, instanceID, true, platform.Row{}); err == nil {
		t.Fatal("ordinary admission bypassed control barrier")
	}
	j := platform.Job{ID: platform.Uint(sibling["task_id"]), Type: "instance.start", UserID: 2, NodeID: 1, InstanceID: instanceID, LeaseGeneration: 1}
	if err = f.a.Step(t.Context(), j, "state.start", "", "running"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.a.Agent(platform.WithTaskDispatch(t.Context(), j, "state.start"), 1, http.MethodPut, "/1.0/instances/blocked/state?project=default", platform.Row{"action": "start"}); err == nil {
		t.Fatal("pre-existing worker dispatched through control barrier")
	}
	if err = workerConfirm(t.Context(), f.a, j, f.instance(instanceID), "running", "running", nil); err == nil {
		t.Fatal("pre-existing worker committed resource/financial state through control barrier")
	}
	// Member cancellation must not disable the platform's actual debt shutdown.
	if _, err = platform.Enqueue(f.db, "instance.stop", 2, 1, instanceID, false, platform.Row{"reason": "credit-enforcement:2"}); err != nil {
		t.Fatalf("retained member work bypassed credit shutdown authority: %v", err)
	}
}
