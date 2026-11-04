package instances

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func failedCleanupJob(t *testing.T, db *gorm.DB, a *platform.App, source platform.Job) platform.Job {
	t.Helper()
	cause := initializationError("init.template", "initialization_script_failed", "script_exit_nonzero", "unknown", nil)
	if err := db.Table("tasks").Where("id=?", source.ID).Updates(platform.Row{"status": "failed", "error_code": "initialization_script_failed", "result_redacted": jsonText(platform.Row{"error": platform.PublicError(cause, false)})}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("instances").Where("id=?", source.InstanceID).Updates(platform.Row{"incus_uuid": "target-uuid", "observed_state": "running", "provisioning_state": "initializing", "logical_name": "failed guest", "allowed_user_actions": "[]"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("task_steps").Create(&platform.Row{"task_id": source.ID, "step_key": "create.native", "attempt": 1, "status": "succeeded", "parameters_redacted": "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := markFailedCreation(t.Context(), a, source, cause, true); err != nil {
		t.Fatal(err)
	}
	cleanup := platform.Job{ID: 44, Type: "instance.creation_cleanup", UserID: source.UserID, NodeID: source.NodeID, InstanceID: source.InstanceID, LeaseGeneration: 1, Parameters: json.RawMessage(`{"force":true}`)}
	if err := db.Table("tasks").Create(&platform.Row{"id": cleanup.ID, "task_type": cleanup.Type, "instance_id": cleanup.InstanceID, "user_id": cleanup.UserID, "node_id": cleanup.NodeID, "status": "running", "lease_owner": "cleanup", "lease_generation": 1, "lease_until": a.Time().Add(5 * time.Minute), "parameters_redacted": "{}", "result_redacted": "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	return cleanup
}

func TestMySQLCreationRetirementProtectsDeliveryAndQueryInterruptions(t *testing.T) {
	for _, scenario := range []string{"query_interruption", "failed_query_interruption", "successful_legacy_create", "verified_delivery", "delivery_checkpoint", "later_start_failure", "definite_initialization"} {
		t.Run(scenario, func(t *testing.T) {
			db, a, job, _, mutations := creationCleanupDB(t)
			definite := true
			switch scenario {
			case "query_interruption":
				definite = false
			case "failed_query_interruption":
				definite = false
				if err := db.Table("tasks").Where("id=43").Update("status", "failed").Error; err != nil {
					t.Fatal(err)
				}
			case "successful_legacy_create", "later_start_failure":
				if err := db.Table("tasks").Where("id=43").Update("status", "succeeded").Error; err != nil {
					t.Fatal(err)
				}
				if scenario == "later_start_failure" {
					job.Type = "instance.start"
				}
			case "verified_delivery":
				if err := db.Table("task_steps").Create(&platform.Row{"task_id": 43, "step_key": "create.verify", "attempt": 1, "status": "succeeded", "parameters_redacted": "{}"}).Error; err != nil {
					t.Fatal(err)
				}
			case "delivery_checkpoint":
				if err := db.Table("instances").Where("id=15").Update("incus_config_snapshot", `{"creation_delivered":true}`).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := markFailedCreation(t.Context(), a, job, context.DeadlineExceeded, definite); err != nil {
				t.Fatal(err)
			}
			current, err := platform.Get(db, "instances", 15, false)
			if err != nil {
				t.Fatal(err)
			}
			want := scenario == "definite_initialization"
			if (current["creation_failed_at"] != nil) != want || mutations.Load() != 0 {
				t.Fatalf("unsafe retirement: %+v mutations=%d", current, mutations.Load())
			}
			if want {
				first := platform.Stamp(current["creation_failed_at"])
				if err := markFailedCreation(t.Context(), a, job, nil, true); err != nil {
					t.Fatal(err)
				}
				again, _ := platform.Get(db, "instances", 15, false)
				if !platform.Stamp(again["creation_failed_at"]).Equal(first) || platform.Text(again["desired_state"]) != "deleted" || again["deleted_at"] != nil {
					t.Fatalf("retirement was not idempotent/fenced: %+v", again)
				}
			}
		})
	}
}

func TestMySQLFailedCreationRejectsQueuedLifecycleJobs(t *testing.T) {
	db, a, source, _, mutations := creationCleanupDB(t)
	_ = failedCleanupJob(t, db, a, source)
	for _, action := range []string{"stop", "delete"} {
		stale := source
		stale.Type = "instance." + action
		var err error
		if action == "stop" {
			err = runLifecycle(t.Context(), a, stale)
		} else {
			err = runDelete(t.Context(), a, stale)
		}
		var public *platform.HTTPError
		if !errors.As(err, &public) || public.Code != "not_found" {
			t.Fatalf("retired creation allowed stale %s: %v", action, err)
		}
	}
	if mutations.Load() != 0 {
		t.Fatalf("stale lifecycle job dispatched %d native mutations", mutations.Load())
	}
}

func TestMySQLFailedCreationCleanupReusesDeleteAndIndependentGPUEvidence(t *testing.T) {
	var gone, stopped, offline atomic.Bool
	offline.Store(true)
	var stops, deletes, metrics atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var out any
		switch {
		case r.URL.Path == "/agent/v1/instances/metrics":
			metrics.Add(1)
			out = deletionFrame(time.Now())
		case r.URL.Path == "/1.0/instances" && r.Method == http.MethodGet:
			items := []platform.Row{}
			if !gone.Load() {
				items = append(items, platform.Row{"name": "test-pending", "config": platform.Row{"volatile.uuid": "target-uuid", "user.101cloud.instance_id": "15"}})
			}
			out = platform.Row{"metadata": items}
		case r.URL.Path == "/1.0/instances/test-pending/state":
			if r.Method == http.MethodPut {
				stops.Add(1)
				stopped.Store(true)
			}
			state := "Running"
			if stopped.Load() {
				state = "Stopped"
			}
			out = platform.Row{"metadata": platform.Row{"status": state}}
		case r.URL.Path == "/1.0/instances/test-pending":
			if r.Method == http.MethodDelete {
				deletes.Add(1)
				gone.Store(true)
				out = platform.Row{"metadata": platform.Row{}}
			} else {
				out = platform.Row{"metadata": platform.Row{"name": "test-pending", "config": platform.Row{"volatile.uuid": "target-uuid", "user.101cloud.instance_id": "15"}}}
			}
		default:
			t.Errorf("unexpected upstream %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	db, a, source, _, _ := creationCleanupDB(t, handler)
	job := failedCleanupJob(t, db, a, source)
	original, _ := platform.Get(db, "tasks", source.ID, false)
	if err := runCreationCleanup(t.Context(), a, job); err == nil || stops.Load() != 0 || deletes.Load() != 0 {
		t.Fatal("disconnected cleanup changed native resources")
	}
	offline.Store(false)
	if err := runCreationCleanup(t.Context(), a, job); err != nil {
		t.Fatal(err)
	}
	current, _ := platform.Get(db, "instances", 15, false)
	gpu, _ := platform.Get(db, "gpu_allocations", 1, false)
	ip, _ := platform.Get(db, "ip_addresses", 1, false)
	if current["deleted_at"] == nil || platform.Text(current["creation_cleanup_state"]) != "completed" || !platform.Bool(gpu["release_verified"]) || platform.Text(ip["status"]) != "available" || stops.Load() != 1 || deletes.Load() != 1 || metrics.Load() == 0 {
		t.Fatalf("cleanup incomplete: instance=%+v gpu=%+v ip=%+v stop=%d delete=%d metrics=%d", current, gpu, ip, stops.Load(), deletes.Load(), metrics.Load())
	}
	if err := runCreationCleanup(t.Context(), a, job); err != nil {
		t.Fatal(err)
	}
	after, _ := platform.Get(db, "tasks", source.ID, false)
	if platform.Text(after["result_redacted"]) != platform.Text(original["result_redacted"]) || deletes.Load() != 1 || stops.Load() != 1 {
		t.Fatal("duplicate cleanup replayed mutation or overwrote original failure")
	}
	view, err := taskDTO(db, after)
	if err != nil {
		t.Fatal(err)
	}
	summary := object(view["creation_cleanup"])
	if summary["status"] != "completed" || summary["instance_name"] != "failed guest" || view["resource_reservations_released"] != true || view["next_action"] == "correct_and_resubmit" {
		t.Fatalf("safe recovery summary missing: %+v", view)
	}
}

func TestMySQLFailedCreationCleanupRetainsUnknownMutationsAndForeignIdentity(t *testing.T) {
	for _, scenario := range []string{"disconnected", "foreign_uuid", "foreign_marker", "ambiguous_original", "original_running", "ambiguous_cleanup_delete"} {
		t.Run(scenario, func(t *testing.T) {
			var mutations atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations.Add(1)
					http.Error(w, "must not mutate", 500)
					return
				}
				if scenario == "disconnected" {
					http.Error(w, "offline", 503)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				cfg := platform.Row{"volatile.uuid": "target-uuid", "user.101cloud.instance_id": "15"}
				if scenario == "foreign_uuid" {
					cfg["volatile.uuid"] = "foreign"
				}
				if scenario == "foreign_marker" {
					cfg["user.101cloud.instance_id"] = "999"
				}
				out := platform.Row{"metadata": platform.Row{"name": "test-pending", "config": cfg}}
				if r.URL.Path == "/1.0/instances" {
					out["metadata"] = []platform.Row{{"name": "test-pending", "config": cfg}}
				}
				if strings.HasSuffix(r.URL.Path, "/state") {
					out["metadata"] = platform.Row{"status": "Stopped"}
				}
				if strings.HasPrefix(r.URL.Path, "/1.0/operations/") {
					out["metadata"] = platform.Row{"status_code": 103}
				}
				_ = json.NewEncoder(w).Encode(out)
			})
			db, a, source, _, _ := creationCleanupDB(t, handler)
			job := failedCleanupJob(t, db, a, source)
			if scenario == "ambiguous_original" || scenario == "original_running" {
				operation := ""
				if scenario == "original_running" {
					operation = "/1.0/operations/create"
				}
				if err := db.Table("task_steps").Where("task_id=43 AND step_key='create.native'").Updates(platform.Row{"status": "unknown", "operation_id": operation}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "ambiguous_cleanup_delete" {
				if err := a.Step(t.Context(), job, "delete.native", "", "running"); err != nil {
					t.Fatal(err)
				}
			}
			if err := runCreationCleanup(t.Context(), a, job); err == nil {
				t.Fatal("unconfirmed cleanup succeeded")
			}
			current, _ := platform.Get(db, "instances", 15, false)
			ip, _ := platform.Get(db, "ip_addresses", 1, false)
			if mutations.Load() != 0 || current["deleted_at"] != nil || platform.Text(current["creation_cleanup_state"]) != "unknown" || platform.Text(ip["status"]) != "reserved" {
				t.Fatalf("unsafe resource release: %+v ip=%+v mutations=%d", current, ip, mutations.Load())
			}
		})
	}
}

func TestMySQLRetiredCreationOwnershipAndTaskRecovery(t *testing.T) {
	db, a, source, _, _ := creationCleanupDB(t)
	_ = failedCleanupJob(t, db, a, source)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user", platform.User{ID: 2})
		if strings.HasPrefix(c.Request.URL.Path, "/admin/") {
			c.Set("admin", true)
		}
		c.Next()
	})
	router.GET("/instances/:id", platform.Handle(func(c *gin.Context) (any, error) { return owned(c, a) }))
	router.GET("/admin/instances/:id", platform.Handle(func(c *gin.Context) (any, error) { return owned(c, a) }))
	router.POST("/admin/instances/:id", platform.Handle(func(c *gin.Context) (any, error) { return owned(c, a) }))
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/instances/15", 404}, {"GET", "/admin/instances/15", 200}, {"POST", "/admin/instances/15", 404}} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
		if response.Code != tc.status {
			t.Fatalf("%s %s returned %d: %s", tc.method, tc.path, response.Code, response.Body.String())
		}
	}
	task, _ := platform.Get(db, "tasks", 43, false)
	view, err := taskDTO(db, task)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(view)
	if view["creation_progress"] == nil || object(view["creation_cleanup"])["instance_name"] != "failed guest" || view["resource_reservations_released"] != false || strings.Contains(string(encoded), "login_config_ciphertext") {
		t.Fatalf("unsafe or incomplete retired task: %s", encoded)
	}
}

func TestMySQLHistoricalFailedCreationDiscoveryAndCleanupLeaseRecovery(t *testing.T) {
	db, a, source, _, mutations := creationCleanupDB(t)
	if err := db.Table("tasks").Where("id=?", source.ID).Updates(platform.Row{"status": "unknown", "lease_owner": nil, "lease_until": nil, "error_code": "initialization_script_failed", "result_redacted": jsonText(platform.Row{"error": platform.PublicError(initializationError("init.template", "initialization_script_failed", "script_exit_nonzero", "unknown", nil), false)})}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("task_steps").Create(&platform.Row{"task_id": source.ID, "step_key": "init.template", "attempt": 1, "status": "failed", "parameters_redacted": "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := pollFailedCreations(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	current, _ := platform.Get(db, "instances", 15, false)
	if current["creation_failed_at"] == nil || platform.Text(current["desired_state"]) != "deleted" || current["deleted_at"] != nil || mutations.Load() != 0 {
		t.Fatalf("historical failure was not safely retired: %+v", current)
	}
	var cleanup []platform.Row
	if err := db.Table("tasks").Where("task_type='instance.creation_cleanup'").Find(&cleanup).Error; err != nil || len(cleanup) != 1 {
		t.Fatalf("cleanup journal missing/duplicated: %v %v", cleanup, err)
	}
	id := cleanup[0]["id"]
	if err := db.Table("tasks").Where("id=?", id).Updates(platform.Row{"status": "unknown", "lease_generation": 2, "error_code": "lease_expired"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("task_steps").Create(&platform.Row{"task_id": id, "step_key": "delete.native", "attempt": 2, "status": "running", "parameters_redacted": "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := pollFailedCreations(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Table("tasks").Where("task_type='instance.creation_cleanup'").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("crash recovery created a second mutation journal: %d %v", count, err)
	}
	recovered, _ := platform.Get(db, "tasks", platform.Uint(id), false)
	if platform.Text(recovered["status"]) != "queued" || platform.Uint(recovered["lease_generation"]) != 2 || mutations.Load() != 0 {
		t.Fatalf("unknown cleanup was not resumed read-only: %+v", recovered)
	}
}
