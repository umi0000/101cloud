package instances

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

// The native endpoint is real pinned TLS; only its Incus responses are controlled.
// All SQL tables inherited from creationCleanupDB are connection-local temporary tables.
type runningLifecycleNative struct {
	mu         sync.Mutex
	state      string
	resized    bool
	absent     bool
	failStop   bool
	force      bool
	calls      []string
	violations []string
}

func (n *runningLifecycleNative) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	respond := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/state"):
		var body platform.Row
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			n.violations = append(n.violations, err.Error())
		}
		action := platform.Text(body["action"])
		n.calls = append(n.calls, action)
		if action == "stop" {
			if platform.Bool(body["force"]) != n.force {
				n.violations = append(n.violations, "incorrect stop force")
			}
			if n.failStop {
				http.Error(w, "stop rejected", http.StatusConflict)
				return
			}
			n.state = "Stopped"
		} else if action == "start" {
			if !n.resized {
				n.violations = append(n.violations, "start before resize")
			}
			n.state = "Running"
		} else {
			n.violations = append(n.violations, "unexpected state action: "+action)
		}
		respond(platform.Row{"type": "async", "operation": "/1.0/operations/" + action})
	case r.Method == http.MethodPut:
		n.calls = append(n.calls, "resize")
		if n.state != "Stopped" {
			n.violations = append(n.violations, "resize before confirmed stop")
		}
		if r.Header.Get("If-Match") != `"lifecycle-etag"` {
			n.violations = append(n.violations, "resize missing observed ETag")
		}
		var body platform.Row
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			n.violations = append(n.violations, err.Error())
		}
		if !workerResourcesMatch(body, 2, "2147483648", "21474836480") {
			n.violations = append(n.violations, "incorrect resize config")
		}
		n.resized = true
		respond(platform.Row{"type": "async", "operation": "/1.0/operations/resize"})
	case r.Method == http.MethodDelete:
		n.calls = append(n.calls, "delete")
		if n.state != "Stopped" {
			n.violations = append(n.violations, "delete before confirmed stop")
		}
		n.absent = true
		respond(platform.Row{"type": "async", "operation": "/1.0/operations/delete"})
	case strings.HasPrefix(r.URL.Path, "/1.0/operations/"):
		if strings.Contains(r.URL.Path, "expired") {
			http.Error(w, "expired", http.StatusNotFound)
			return
		}
		respond(platform.Row{"metadata": platform.Row{"status_code": 200}})
	case r.URL.Path == "/agent/v1/instances/metrics":
		if r.URL.Query().Get("project") != "*" {
			n.violations = append(n.violations, "GPU evidence not host-wide")
		}
		respond(deletionFrame(time.Now()))
	case r.URL.Path == "/1.0/instances":
		list := []any{}
		if !n.absent {
			list = append(list, platform.Row{"name": "test-pending", "config": platform.Row{"volatile.uuid": "target-uuid"}})
		}
		respond(platform.Row{"metadata": list})
	case strings.HasSuffix(r.URL.Path, "/state"):
		respond(platform.Row{"metadata": platform.Row{"status": n.state}})
	case strings.Contains(r.URL.Path, "/storage-pools/"):
		respond(platform.Row{"metadata": platform.Row{"space": platform.Row{"total": "1099511627776", "used": "0"}}})
	default:
		cpu, memory := "1", "1073741824"
		if n.resized {
			cpu, memory = "2", "2147483648"
		}
		w.Header().Set("ETag", `"lifecycle-etag"`)
		respond(platform.Row{"metadata": platform.Row{"name": "test-pending", "architecture": "x86_64", "profiles": []string{}, "config": platform.Row{"volatile.uuid": "target-uuid", "limits.cpu": cpu, "limits.memory": memory}, "devices": platform.Row{"root": platform.Row{"type": "disk", "path": "/", "pool": "default", "size": "21474836480"}}}})
	}
}

func runningResizeDB(t *testing.T, native *runningLifecycleNative, restart bool) (*gorm.DB, *platform.App, platform.Job) {
	t.Helper()
	db, a, job, _, _ := creationCleanupDB(t, native)
	state := "stopped"
	if restart {
		state = "running"
	}
	params, err := json.Marshal(platform.Row{"restart_after_resize": restart, "previous_resources": Resources{VCPUs: 1, MemoryBytes: "1073741824", DiskBytes: "21474836480"}, "resources": Resources{VCPUs: 2, MemoryBytes: "2147483648", DiskBytes: "21474836480"}})
	if err != nil {
		t.Fatal(err)
	}
	job.Type, job.Parameters = "instance.resize", params
	if err = db.Table("instances").Where("id=15").Updates(platform.Row{"incus_uuid": "target-uuid", "observed_state": state, "desired_state": state, "provisioning_state": "resizing"}).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Table("tasks").Where("id=43").Updates(platform.Row{"task_type": job.Type, "parameters_redacted": string(params), "lease_until": a.Time().Add(5 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	return db, a, job
}

func assertRunningNative(t *testing.T, n *runningLifecycleNative, want string) {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	if strings.Join(n.calls, ",") != want || len(n.violations) != 0 {
		t.Fatalf("native calls=%v want=%q violations=%v", n.calls, want, n.violations)
	}
}

func TestMySQLRunningResizeDurableOrdering(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name, state, calls := "stopped", "Stopped", "resize"
		if restart {
			name, state, calls = "running", "Running", "stop,resize,start"
		}
		t.Run(name, func(t *testing.T) {
			n := &runningLifecycleNative{state: state}
			db, a, job := runningResizeDB(t, n, restart)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := runResize(ctx, a, job); err != nil {
				t.Fatal(err)
			}
			assertRunningNative(t, n, calls)
			instance, err := platform.Get(db, "instances", 15, false)
			if err != nil || platform.Uint(instance["vcpus"]) != 2 || platform.Text(instance["observed_state"]) != name || platform.Text(instance["desired_state"]) != name {
				t.Fatalf("resize final state=%+v err=%v", instance, err)
			}
			if restart {
				var steps []platform.Row
				if err = db.Table("task_steps").Where("task_id=43 AND step_key IN ('state.stop','resize.native','resize.confirmed','state.start')").Order("id").Find(&steps).Error; err != nil {
					t.Fatal(err)
				}
				keys := []string{}
				for _, step := range steps {
					keys = append(keys, platform.Text(step["step_key"]))
					if platform.Text(step["status"]) != "succeeded" {
						t.Fatalf("unfinished journal: %+v", step)
					}
				}
				if strings.Join(keys, ",") != "state.stop,resize.native,resize.confirmed,state.start" {
					t.Fatalf("journal order=%v", keys)
				}
			}
		})
	}
}

func TestMySQLRunningForceDeleteAndFailedStopKeepReservations(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "confirmed_force_stop"
		if fail {
			name = "failed_force_stop"
		}
		t.Run(name, func(t *testing.T) {
			n := &runningLifecycleNative{state: "Running", force: true, failStop: fail}
			db, a, job := deletionDB(t, n, "", "")
			job.Parameters = json.RawMessage(`{"force":true,"delete_data":true}`)
			if err := db.Table("instances").Where("id=15").Update("observed_state", "running").Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Table("tasks").Where("id=43").Update("parameters_redacted", string(job.Parameters)).Error; err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := runDelete(ctx, a, job)
			if fail {
				if err == nil {
					t.Fatal("failed stop accepted")
				}
				assertRunningNative(t, n, "stop")
				assertDeletionReservations(t, db, false)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertRunningNative(t, n, "stop,delete")
				assertDeletionReservations(t, db, true)
			}
		})
	}
}

func TestMySQLRunningResizeRecoveryNeverReplaysEarlierStages(t *testing.T) {
	for _, stage := range []string{"resize.native", "resize.confirmed", "state.start"} {
		for _, operation := range []string{"", "/1.0/operations/expired?project=default"} {
			name := stage + "/partial"
			if operation != "" {
				name = stage + "/expired"
			}
			t.Run(name, func(t *testing.T) {
				n := &runningLifecycleNative{state: "Stopped", resized: true}
				if stage == "state.start" {
					n.state = "Running"
				}
				db, a, job := runningResizeDB(t, n, true)
				keys := []string{"state.stop", "resize.native", "resize.confirmed", "state.start"}
				for _, key := range keys {
					status, op := "succeeded", ""
					if key == stage {
						status, op = "running", operation
						if op != "" {
							status = "waiting"
						}
					}
					if err := db.Table("task_steps").Create(&platform.Row{"task_id": 43, "step_key": key, "attempt": 1, "status": status, "operation_id": op, "parameters_redacted": "{}", "result_redacted": "{}"}).Error; err != nil {
						t.Fatal(err)
					}
					if key == stage {
						break
					}
				}
				if err := db.Table("tasks").Where("id=43").Updates(platform.Row{"status": "unknown", "lease_owner": nil}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Table("tasks").Create(&platform.Row{"id": 44, "task_type": "instance.reconcile", "user_id": 2, "node_id": 1, "instance_id": 15, "status": "running", "parameters_redacted": `{"source_task_id":"43"}`, "lease_generation": 1, "lease_until": a.Time().Add(time.Minute), "result_redacted": "{}"}).Error; err != nil {
					t.Fatal(err)
				}
				job.ID, job.Type, job.Parameters = 44, "instance.reconcile", json.RawMessage(`{"source_task_id":"43"}`)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := runReconcile(ctx, a, job); err != nil {
					t.Fatal(err)
				}
				want := "start"
				if stage == "state.start" {
					want = ""
				}
				assertRunningNative(t, n, want)
				source, err := platform.Get(db, "tasks", 43, false)
				if err != nil || platform.Text(source["status"]) != "succeeded" {
					t.Fatalf("source=%+v err=%v", source, err)
				}
			})
		}
	}
}

func TestMySQLRunningResizeStartIntentNeverReappliesConfiguration(t *testing.T) {
	n := &runningLifecycleNative{state: "Running", resized: false}
	db, a, job := runningResizeDB(t, n, true)
	for _, key := range []string{"state.stop", "resize.native", "resize.confirmed", "state.start"} {
		status := "succeeded"
		if key == "state.start" {
			status = "running"
		}
		if err := db.Table("task_steps").Create(&platform.Row{"task_id": 43, "step_key": key, "attempt": 1, "status": status, "parameters_redacted": "{}", "result_redacted": "{}"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runResize(ctx, a, job); err == nil {
		t.Fatal("configuration drift after start intent accepted")
	}
	assertRunningNative(t, n, "")
}

func TestMySQLRunningResizeFailedStopRetainsGPUAndOldResources(t *testing.T) {
	n := &runningLifecycleNative{state: "Running", failStop: true}
	db, a, job := runningResizeDB(t, n, true)
	if err := db.Table("gpu_allocations").Where("id=1").Updates(platform.Row{"status": "active", "configured": true, "observed": true}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runResize(ctx, a, job); err == nil {
		t.Fatal("failed resize stop accepted")
	}
	assertRunningNative(t, n, "stop")
	instance, err := platform.Get(db, "instances", 15, false)
	if err != nil || platform.Uint(instance["vcpus"]) != 1 || platform.Text(instance["memory_bytes"]) != "1073741824" {
		t.Fatalf("resources changed before confirmed stop: %+v %v", instance, err)
	}
	gpu, err := platform.Get(db, "gpu_allocations", 1, false)
	if err != nil || platform.Text(gpu["status"]) != "active" || platform.Bool(gpu["release_verified"]) || platform.Uint(gpu["active_slot"]) != 1 {
		t.Fatalf("GPU released after failed stop: %+v %v", gpu, err)
	}
}
