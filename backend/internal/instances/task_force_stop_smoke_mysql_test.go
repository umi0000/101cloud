package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

// This is an actual loopback HTTP -> SQL mutation -> production worker ->
// certificate-pinned TLS native request cancellation, not a status-only fake.
func TestMySQLForceStopHTTPWorkerTLSSmoke(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	instanceID, createID := platform.Uint(created["instance_id"]), platform.Uint(created["task_id"])
	instance := f.instance(instanceID)
	name := platform.Text(instance["incus_name"])
	f.exec("UPDATE tasks SET status='succeeded',finished_at=? WHERE id=?", f.a.Time(), createID)
	f.exec("UPDATE instances SET provisioning_state='ready',observed_state='running',desired_state='stopped',incus_uuid='smoke-uuid',observed_at=? WHERE id=?", f.a.Time(), instanceID)
	var puts, deletes atomic.Int64
	submitted := make(chan struct{})
	releaseNative := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseNative) }) })
	f.native = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			http.Error(w, "unexpected delete", http.StatusBadRequest)
		case r.Method == http.MethodPut:
			if puts.Add(1) == 1 {
				close(submitted)
			}
			// A real remote system may ignore HTTP cancellation and still finish later.
			<-releaseNative
			_ = json.NewEncoder(w).Encode(platform.Row{"type": "async", "operation": "/1.0/operations/late-stop"})
		case strings.HasSuffix(r.URL.Path, "/state"):
			_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status": "Running"}})
		default:
			_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"name": name, "config": platform.Row{"volatile.uuid": "smoke-uuid"}}})
		}
	})
	// No unrelated catalog pollers: run the same production claim/execute loop
	// with the actual instance lifecycle consumer and same endpoint App registry.
	a := platform.New(f.db, f.a.Config)
	workerContexts := make(chan context.Context, 1)
	a.RegisterJob("instance.stop", func(ctx context.Context, a *platform.App, j platform.Job) error {
		workerContexts <- ctx
		return runLifecycle(ctx, a, j)
	})
	router := gin.New()
	router.Use(platform.RequestMiddleware())
	admin := router.Group("/api/v1/admin", func(c *gin.Context) { c.Set("user", platform.User{ID: 1, Role: "admin"}); c.Next() }, platform.Admin(), a.Mutations())
	admin.POST("/tasks/:id/force-stop", platform.Handle(func(c *gin.Context) (any, error) { return forceStopTask(c, a) }))
	server := httptest.NewServer(router)
	defer server.Close()
	stop := func(id uint64, revision uint64, key string, expected ...int) platform.Row {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/admin/tasks/%d/force-stop", server.URL, id), strings.NewReader(`{"confirm":true}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, revision))
		client := &http.Client{Timeout: 5 * time.Second}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var envelope platform.Row
		if err = json.NewDecoder(response.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		status := http.StatusOK
		if len(expected) != 0 {
			status = expected[0]
		}
		if response.StatusCode != status {
			t.Fatalf("HTTP force-stop: %d %+v", response.StatusCode, envelope)
		}
		return object(envelope["data"])
	}
	enqueue := func(mutation bool) uint64 {
		t.Helper()
		task, err := platform.Enqueue(f.db, "instance.stop", 2, 1, instanceID, mutation, platform.Row{})
		if err != nil {
			t.Fatal(err)
		}
		return platform.Uint(task["task_id"])
	}
	queuedID := enqueue(false)
	stop(queuedID, 1, "smoke-queued")
	var reservedBefore int64
	if err := f.db.Table("ip_addresses").Where("instance_id=? AND status='reserved'", instanceID).Count(&reservedBefore).Error; err != nil {
		t.Fatal(err)
	}
	unknownID := enqueue(false)
	f.exec("UPDATE tasks SET status='unknown',error_code='lease_expired' WHERE id=?", unknownID)
	inFlightID := enqueue(true)
	ctx, cancel := context.WithCancel(t.Context())
	workerDone := make(chan struct{})
	go func() { a.Run(ctx); close(workerDone) }()
	defer func() {
		cancel()
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			t.Error("workers did not stop")
		}
	}()
	select {
	case <-submitted:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not reach TLS native mutation")
	}
	running := forceStopStored(t, f, inFlightID)
	workerCtx := <-workerContexts
	f.exec("CREATE TRIGGER force_stop_rollback BEFORE INSERT ON audit_logs FOR EACH ROW BEGIN IF NEW.event_type='api.mutation' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='force stop rollback regression'; END IF; END")
	stop(inFlightID, platform.Uint(running["revision"]), "smoke-in-flight", http.StatusInternalServerError)
	f.exec("DROP TRIGGER force_stop_rollback")
	rolledBack := forceStopStored(t, f, inFlightID)
	if workerCtx.Err() != nil || platform.Text(rolledBack["status"]) != "running" || platform.Uint(rolledBack["lease_generation"]) != platform.Uint(running["lease_generation"]) || platform.Uint(rolledBack["revision"]) != platform.Uint(running["revision"]) {
		t.Fatalf("rejected force-stop interrupted execution or changed its lease: ctx=%v task=%+v", workerCtx.Err(), rolledBack)
	}
	dto := stop(inFlightID, platform.Uint(running["revision"]), "smoke-in-flight")
	if workerCtx.Err() != context.Canceled {
		t.Fatal("committed force-stop did not cancel active execution")
	}
	if !platform.Bool(object(dto["force_stop"])["requires_reconciliation"]) {
		t.Fatalf("external uncertainty omitted: %+v", dto)
	}
	releaseOnce.Do(func() { close(releaseNative) })
	stop(unknownID, 1, "smoke-unknown")
	// Let the real claimant tick again: none of these cancelled tasks can re-run.
	timer := time.NewTimer(1200 * time.Millisecond)
	<-timer.C
	if puts.Load() != 1 || deletes.Load() != 0 {
		t.Fatalf("native replay/cancel/DELETE: puts=%d deletes=%d", puts.Load(), deletes.Load())
	}
	for _, id := range []uint64{queuedID, inFlightID, unknownID} {
		task := forceStopStored(t, f, id)
		if platform.Text(task["status"]) != "cancelled" || platform.Text(task["error_code"]) != "task_force_stopped" {
			t.Fatalf("late worker resurrected task: %+v", task)
		}
	}
	var step platform.Row
	if err := f.db.Table("task_steps").Where("task_id=? AND step_key='state.stop'", inFlightID).Take(&step).Error; err != nil {
		t.Fatal(err)
	}
	if !platform.Bool(object(step["result_redacted"])["native_dispatch_started"]) || platform.Text(step["status"]) != "unknown" {
		t.Fatalf("lost dispatched journal: %+v", step)
	}
	var reservedAfter int64
	if err := f.db.Table("ip_addresses").Where("instance_id=? AND status='reserved'", instanceID).Count(&reservedAfter).Error; err != nil {
		t.Fatal(err)
	}
	if reservedBefore != 1 || reservedAfter != reservedBefore || !platform.Bool(forceStopStored(t, f, inFlightID)["is_instance_mutation"]) {
		t.Fatalf("resource or barrier released: %d -> %d", reservedBefore, reservedAfter)
	}
	if platform.Text(f.instance(instanceID)["observed_state"]) != "running" {
		t.Fatal("force-stop falsely reported physical stop")
	}
	t.Logf("HTTP/TLS/worker smoke: queued=%d in_flight=%d unknown=%d; native PUT=1 DELETE=0; late completion fenced; IP and mutation barrier retained", queuedID, inFlightID, unknownID)
}
