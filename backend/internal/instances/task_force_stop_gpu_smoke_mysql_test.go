package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
)

func TestMySQLGPUForceStopHTTPWorkerTLSPreflightSmoke(t *testing.T) {
	f, id, taskID := gpuStopFixture(t)
	var mutations, metrics atomic.Int64
	f.native = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			mutations.Add(1)
			http.Error(w, "unexpected native mutation", 500)
			return
		}
		switch {
		case r.URL.Path == "/agent/v1/instances/metrics":
			metrics.Add(1)
			// A real liveGPU call must reject incomplete ownership evidence.
			_ = json.NewEncoder(w).Encode(platform.Row{"collection_complete": true, "collected_at": time.Now().UTC().Format(time.RFC3339Nano), "instances": []any{platform.Row{"uuid": "gpu-stop-uuid", "name": f.instance(id)["incus_name"], "quality": platform.Row{"attribution_complete": false}}}})
		case r.URL.Path == "/agent/v1/resources":
			_ = json.NewEncoder(w).Encode(platform.Row{"gpus": []any{platform.Row{"uuid": "GPU-stop-1", "pci_address": "0000:41:00.0"}, platform.Row{"uuid": "GPU-stop-3", "pci_address": "0000:43:00.0"}}})
		case strings.HasSuffix(r.URL.Path, "/state"):
			_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status": "Running"}})
		default:
			_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"config": platform.Row{"volatile.uuid": "gpu-stop-uuid"}, "expanded_devices": platform.Row{"gpu1": platform.Row{"type": "gpu", "gputype": "physical", "pci": "41:00.0"}, "gpu3": platform.Row{"type": "gpu", "gputype": "physical", "pci": "43:00.0"}}}})
		}
	})
	a := platform.New(f.db, f.a.Config)
	a.RegisterJob("instance.gpu", runGPU)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop")
		}
	}()
	deadline := time.After(10 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for platform.Text(forceStopStored(t, f, taskID)["status"]) != "unknown" {
		select {
		case <-deadline:
			t.Fatal("production worker did not record preflight unknown")
		case <-ticker.C:
		}
	}
	cancel()
	<-done
	if metrics.Load() != 1 || mutations.Load() != 0 {
		t.Fatalf("not a liveGPU-only failure: metrics=%d mutations=%d", metrics.Load(), mutations.Load())
	}
	var configure int64
	if err := f.db.Table("task_steps").Where("task_id=? AND step_key='gpu.configure'", taskID).Count(&configure).Error; err != nil {
		t.Fatal(err)
	}
	if configure != 0 {
		t.Fatal("preflight failure reached gpu.configure")
	}
	server := httptest.NewServer(f.router)
	defer server.Close()
	request := func(method, path, body string, revision uint64) platform.Row {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+"/api/v1"+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if method != http.MethodGet {
			req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, revision))
			req.Header.Set("Idempotency-Key", fmt.Sprintf("gpu-smoke-%s-%d", path, revision))
		}
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var envelope platform.Row
		if err = json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		want := http.StatusOK
		if strings.HasSuffix(path, "/stop") {
			want = http.StatusAccepted
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d %+v", method, path, resp.StatusCode, envelope)
		}
		return object(envelope["data"])
	}
	task := forceStopStored(t, f, taskID)
	request(http.MethodPost, fmt.Sprintf("/tasks/%d/force-stop", taskID), `{"confirm":true}`, platform.Uint(task["revision"]))
	assertGPUStopResources(t, f, true)
	view := request(http.MethodGet, fmt.Sprintf("/instances/%d", id), "", 0)
	if view["active_task_id"] != nil || view["active_task_status"] != nil {
		t.Fatalf("stale active task DTO: %+v", view)
	}
	request(http.MethodPost, fmt.Sprintf("/instances/%d/stop", id), `{}`, platform.Uint(f.instance(id)["revision"]))
	if mutations.Load() != 0 {
		t.Fatal("cancellation dispatched native mutation")
	}
}

func TestMySQLGPUHistoricalCancelledSourceChainGETOnlyReconcile(t *testing.T) {
	f, id, taskID := gpuStopFixture(t)
	var mutations atomic.Int64
	f.native = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations.Add(1)
			http.Error(w, "unexpected mutation", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{}})
	})
	f.exec("UPDATE tasks SET status='cancelled',finished_at=NOW(6),error_code='task_force_stopped',result_redacted=?,lease_generation=1 WHERE id=?", `{"error":{"code":"observation_incomplete","message":"实时 GPU 归属信息不完整，暂不能安全调整分配。"},"force_stop":{"requires_reconciliation":true}}`, taskID)
	prior, err := platform.Enqueue(f.db, "instance.reconcile", 2, 1, id, false, platform.Row{"source_task_id": taskID, "read_only_force_stop_reconcile": true})
	if err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE tasks SET status='cancelled',finished_at=NOW(6),error_code='task_force_stopped',result_redacted=? WHERE id=?", `{"force_stop":{"requires_reconciliation":true}}`, prior["task_id"])
	server := httptest.NewServer(f.router)
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/admin/tasks/%s/reconcile", server.URL, platform.Text(prior["task_id"])), strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "historical-gpu-reconcile")
	request.Header.Set("If-Match", `"rev-1"`)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope platform.Row
	if err = json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("admin reconcile: %d %+v", response.StatusCode, envelope)
	}
	wrapperID := platform.Uint(object(envelope["data"])["task_id"])
	a := platform.New(f.db, f.a.Config)
	a.RegisterJob("instance.reconcile", runReconcile)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("historical reconcile worker did not stop")
		}
	}()
	deadline := time.After(10 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for platform.Text(forceStopStored(t, f, wrapperID)["status"]) != "succeeded" {
		select {
		case <-deadline:
			t.Fatalf("historical reconcile did not finish: %+v", forceStopStored(t, f, wrapperID))
		case <-ticker.C:
		}
	}
	assertGPUStopResources(t, f, true)
	if err = platform.TaskMutationBarrier(f.db, id); err != nil {
		t.Fatalf("safe historical chain remains locked: %v", err)
	}
	if platform.Text(forceStopStored(t, f, taskID)["status"]) != "cancelled" || mutations.Load() != 0 {
		t.Fatal("reconciliation rewrote history or performed native mutation")
	}
}
