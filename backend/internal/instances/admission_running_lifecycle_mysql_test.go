package instances

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

func TestMySQLRunningLifecycleAdmissionAcknowledgementsAndPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, action, body, actions string
		admin                       bool
		status                      int
		code                        string
	}{
		{"resize_ack", "resize", `{}`, `["resize","stop","start"]`, false, 422, "invalid_request"},
		{"resize_stop_permission", "resize", `{"confirm_restart":true}`, `["resize","start"]`, false, 403, "action_not_allowed"},
		{"resize_start_permission", "resize", `{"confirm_restart":true}`, `["resize","stop"]`, false, 403, "action_not_allowed"},
		{"resize_permission", "resize", `{"confirm_restart":true}`, `["stop","start"]`, false, 403, "action_not_allowed"},
		{"delete_user_force", "delete", `{"mode":"force","confirm_data_loss_risk":true,"confirm_name":"guest","delete_data":true}`, `["delete"]`, false, 403, "force_not_allowed"},
		{"delete_admin_risk", "delete", `{"mode":"force","confirm_name":"guest","delete_data":true}`, `[]`, true, 422, "invalid_request"},
		{"delete_admin_force", "delete", `{"confirm_name":"guest","delete_data":true}`, `[]`, true, 409, "force_confirmation_required"},
		{"delete_exact_name", "delete", `{"mode":"force","confirm_data_loss_risk":true,"confirm_name":"wrong","delete_data":true}`, `[]`, true, 422, "invalid_request"},
		{"delete_data_ack", "delete", `{"mode":"force","confirm_data_loss_risk":true,"confirm_name":"guest"}`, `[]`, true, 422, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, a, _, _, _ := creationCleanupDB(t)
			if err := db.Table("instances").Where("id=15").Updates(platform.Row{"product_type": "custom", "allowed_user_actions": tc.actions, "logical_name": "guest", "observed_state": "running", "provisioning_state": "ready", "observed_at": a.Time(), "revision": 7}).Error; err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set("user", platform.User{ID: 2}); c.Set("admin", tc.admin) })
			router.POST("/instances/:id/action", platform.Handle(func(c *gin.Context) (any, error) { return lifecycle(c, a, tc.action) }))
			req := httptest.NewRequest(http.MethodPost, "/instances/15/action", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", `"rev-7"`)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.code) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var steps int64
			if err := db.Table("task_steps").Count(&steps).Error; err != nil {
				t.Fatal(err)
			}
			if steps != 0 {
				t.Fatal("rejected admission journaled native work")
			}
			instance, err := platform.Get(db, "instances", 15, false)
			if err != nil || platform.Uint(instance["revision"]) != 7 || platform.Uint(instance["vcpus"]) != 1 {
				t.Fatalf("rejected admission mutated instance: %+v %v", instance, err)
			}
		})
	}
}

func TestMySQLRunningResizeReservationPreservesActiveComputeSnapshot(t *testing.T) {
	n := &runningLifecycleNative{state: "Running"}
	db, a, job := runningResizeDB(t, n, true)
	old := `{"vcpus":1,"memory_bytes":"1073741824","disk_bytes":"21474836480"}`
	if err := db.Table("usage_sessions").Where("id=1").Updates(platform.Row{"metering_state": "running", "config_snapshot": old, "cpu_hourly_pt": "3.000000", "memory_gib_hourly_pt": "4.000000"}).Error; err != nil {
		t.Fatal(err)
	}
	instance, err := platform.Get(db, "instances", 15, false)
	if err != nil {
		t.Fatal(err)
	}
	instance["vcpus"], instance["memory_bytes"], instance["disk_bytes"] = uint64(2), uint64(2147483648), uint64(21474836480)
	instance["cpu_hourly_pt"], instance["memory_gib_hourly_pt"] = "8.000000", "9.000000"
	if err := billing.InstanceUsage(t.Context(), db, instance, "resize_running_reserved", nil); err != nil {
		t.Fatal(err)
	}
	session, err := platform.Get(db, "usage_sessions", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := object(session["config_snapshot"])
	if platform.Uint(cfg["vcpus"]) != 1 || platform.Text(cfg["memory_bytes"]) != "1073741824" || platform.Text(session["cpu_hourly_pt"]) != "3.000000" || platform.Text(session["memory_gib_hourly_pt"]) != "4.000000" || platform.Text(session["metering_state"]) != "running" {
		t.Fatalf("pre-stop compute snapshot/rates mutated: %+v", session)
	}
	assertRunningNative(t, n, "")
	step, err := workerStep(t.Context(), a, job, "state.stop")
	if err != nil || step != nil {
		t.Fatalf("reservation dispatched stop: %+v %v", step, err)
	}
}

func TestMySQLRunningLifecycleAdmissionPersistsApprovedIntent(t *testing.T) {
	for _, scenario := range []string{"running_resize", "stopped_resize", "running_force_delete", "running_shrink", "running_mixed_resize"} {
		t.Run(scenario, func(t *testing.T) {
			n := &runningLifecycleNative{state: "Running"}
			db, a, _ := runningResizeDB(t, n, true)
			for _, statement := range []string{
				"INSERT INTO users VALUES (2,'active')",
				"INSERT INTO user_limits VALUES (2,100,100,1099511627776,1099511627776,100)",
			} {
				if err := db.Exec(statement).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Table("nodes").Where("id=1").Updates(platform.Row{"administrative_state": "active", "health": "online", "connection_verified_at": a.Time(), "resources_sampled_at": a.Time()}).Error; err != nil {
				t.Fatal(err)
			}
			state := "running"
			if scenario == "stopped_resize" {
				state = "stopped"
			}
			oldCPU, oldMemory := uint64(1), "1073741824"
			if scenario == "running_shrink" || scenario == "running_mixed_resize" {
				oldCPU = 4
				if scenario == "running_shrink" {
					oldMemory = "4294967296"
				}
				if err := db.Table("instances").Where("id=15").Updates(platform.Row{"vcpus": oldCPU, "memory_bytes": oldMemory}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Table("nodes").Where("id=1").Update("allocatable_vcpus", oldCPU).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Table("instances").Where("id=15").Updates(platform.Row{"product_type": "custom", "allowed_user_actions": `["resize","stop","start","delete"]`, "logical_name": "guest", "observed_state": state, "desired_state": state, "provisioning_state": "ready", "observed_at": a.Time(), "revision": 7}).Error; err != nil {
				t.Fatal(err)
			}
			action := "resize"
			body := `{"confirm_restart":true,"resources":{"vcpus":2,"memory_bytes":"2147483648","disk_bytes":"21474836480"}}`
			admin := scenario == "running_force_delete"
			if admin {
				action = "delete"
				body = `{"mode":"force","confirm_data_loss_risk":true,"confirm_name":"guest","delete_data":true}`
			}
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set("user", platform.User{ID: 2}); c.Set("admin", admin) })
			router.POST("/instances/:id/action", platform.Handle(func(c *gin.Context) (any, error) { return lifecycle(c, a, action) }))
			req := httptest.NewRequest(http.MethodPost, "/instances/15/action", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", `"rev-7"`)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var queued platform.Row
			if err := db.Table("tasks").Where("status='queued'").Take(&queued).Error; err != nil {
				t.Fatal(err)
			}
			params := object(queued["parameters_redacted"])
			if platform.Text(queued["task_type"]) != "instance."+action {
				t.Fatalf("wrong task=%+v", queued)
			}
			want := state
			if admin {
				want = "deleted"
				if !platform.Bool(params["force"]) {
					t.Fatalf("force approval lost: %+v", params)
				}
			} else {
				if platform.Bool(params["restart_after_resize"]) != (state == "running") {
					t.Fatalf("restart approval lost: %+v", params)
				}
				previous, target := object(params["previous_resources"]), object(params["resources"])
				if platform.Uint(previous["vcpus"]) != oldCPU || platform.Text(previous["memory_bytes"]) != oldMemory || platform.Uint(target["vcpus"]) != 2 {
					t.Fatalf("resource approval lost: %+v", params)
				}
			}
			instance, err := platform.Get(db, "instances", 15, false)
			if err != nil || platform.Text(instance["desired_state"]) != want || platform.Text(instance["observed_state"]) != state {
				t.Fatalf("queued state=%+v err=%v", instance, err)
			}
			if scenario == "running_shrink" || scenario == "running_mixed_resize" {
				wantMemory := "2147483648"
				if scenario == "running_shrink" {
					wantMemory = oldMemory
				}
				if platform.Uint(instance["vcpus"]) != oldCPU || platform.Text(instance["memory_bytes"]) != wantMemory {
					t.Fatalf("old running reservation released before STOP: %+v", instance)
				}
				// Another instance must still see all four actually running CPUs,
				// not the two CPUs approved for the future stopped configuration.
				candidate := platform.Row{"id": 16, "user_id": 2, "node_id": 1, "product_type": "elastic", "vcpus": 1, "memory_bytes": "1073741824", "disk_bytes": "1"}
				admissionErr := reserveExisting(t.Context(), a, db, candidate, nil)
				api, ok := admissionErr.(*platform.HTTPError)
				if !ok || api.Code != "node_capacity_unavailable" {
					t.Fatalf("pending shrink allowed node CPU oversubscription: %v", admissionErr)
				}
			}
			assertRunningNative(t, n, "")
		})
	}
}
