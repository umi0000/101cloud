package instances

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"101cloud/backend/internal/platform"
)

func TestMySQLCloudInitPrepareNewStoppedCreation(t *testing.T) {
	for _, recoverStep := range []string{"", "running", "unknown", "succeeded"} {
		t.Run("recover-"+recoverStep, func(t *testing.T) {
			var calls atomic.Int64
			var a *platform.App
			var job platform.Job
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/1.0/instances/test-pending":
					cfg := platform.Row{"volatile.uuid": "prepare-uuid", "user.101cloud.instance_id": "15", "volatile.base_image": strings.Repeat("a", 64)}
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"name": "test-pending", "type": "container", "config": cfg, "expanded_config": cfg}})
				case "/1.0/instances/test-pending/state":
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status": "Stopped"}})
				case "/agent/v1/instances/cloud-init/prepare":
					calls.Add(1)
					if r.Method != http.MethodPost {
						t.Error("prepare must POST")
					}
					var body platform.Row
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if len(body) != 5 || body["project"] != "default" || body["name"] != "test-pending" || body["expected_uuid"] != "prepare-uuid" || body["expected_instance_id"] != "15" || body["expected_image_fingerprint"] != strings.Repeat("a", 64) {
						t.Errorf("unsafe preparation identity: %v", body)
					}
					step, err := workerStep(context.Background(), a, job, cloudInitPrepareStep)
					if err != nil || platform.Text(step["status"]) != "running" || !platform.Bool(object(step["result_redacted"])["native_dispatch_started"]) {
						t.Error("prepare dispatched without durable fenced intent")
					}
					_ = json.NewEncoder(w).Encode(platform.Row{"checked": true, "disabled_marker_removed": calls.Load() == 1})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(500)
				}
			})
			db, app, j, instance, _ := creationCleanupDB(t, handler)
			a, job = app, j
			instance["incus_uuid"] = "prepare-uuid"
			instance["incus_config_snapshot"] = `{"creation":{"cloud_init_capable":true}}`
			if err := db.Table("instances").Where("id=15").Updates(instance).Error; err != nil {
				t.Fatal(err)
			}
			if err := a.Step(t.Context(), job, "create.native", "", "succeeded"); err != nil {
				t.Fatal(err)
			}
			if recoverStep != "" {
				if err := a.Step(t.Context(), job, cloudInitPrepareStep, "", recoverStep); err != nil {
					t.Fatal(err)
				}
			}
			if err := workerPrepareCloudInit(t.Context(), a, job, instance); err != nil {
				t.Fatal(err)
			}
			if err := workerPrepareCloudInit(t.Context(), a, job, instance); err != nil {
				t.Fatal(err)
			}
			want := int64(1)
			if recoverStep == "succeeded" {
				want = 0
			}
			if calls.Load() != want {
				t.Fatalf("prepare calls=%d want=%d", calls.Load(), want)
			}
		})
	}
}

func TestMySQLCloudInitPrepareNeverTouchesExistingOrUnverifiedGuest(t *testing.T) {
	for _, scenario := range []string{"start", "restart", "import", "vm", "legacy", "started", "delivered", "cancelled", "retired", "missing-uuid", "missing-project", "ownership", "image", "running", "unconfirmed-create", "revoked", "bad-result", "agent-error"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/1.0/instances/test-pending":
					cfg := platform.Row{"volatile.uuid": "prepare-uuid", "user.101cloud.instance_id": "15", "volatile.base_image": strings.Repeat("a", 64)}
					if scenario == "ownership" {
						cfg["user.101cloud.instance_id"] = "99"
					}
					if scenario == "image" {
						cfg["volatile.base_image"] = strings.Repeat("b", 64)
					}
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"name": "test-pending", "type": "container", "config": cfg, "expanded_config": cfg}})
				case "/1.0/instances/test-pending/state":
					state := "Stopped"
					if scenario == "running" {
						state = "Running"
					}
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status": state}})
				case "/agent/v1/instances/cloud-init/prepare":
					calls.Add(1)
					if scenario == "agent-error" {
						w.WriteHeader(503)
						_, _ = w.Write([]byte(`{"error":"private"}`))
						return
					}
					_ = json.NewEncoder(w).Encode(platform.Row{"checked": false, "disabled_marker_removed": false})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(500)
				}
			})
			db, a, job, instance, _ := creationCleanupDB(t, handler)
			instance["incus_uuid"] = "prepare-uuid"
			instance["incus_config_snapshot"] = `{"creation":{"cloud_init_capable":true}}`
			wantSkip := false
			switch scenario {
			case "start", "restart", "import":
				job.Type = "instance." + scenario
				wantSkip = true
			case "vm":
				instance["image_type"] = "virtual-machine"
				wantSkip = true
			case "legacy":
				instance["incus_config_snapshot"] = "{}"
				wantSkip = true
			case "started":
				if err := a.Step(t.Context(), job, "state.start", "", "unknown"); err != nil {
					t.Fatal(err)
				}
				wantSkip = true
			case "delivered":
				instance["incus_config_snapshot"] = `{"creation":{"cloud_init_capable":true},"creation_delivered":true}`
			case "cancelled":
				instance["desired_state"] = "stopped"
			case "retired":
				instance["creation_failed_at"] = a.Time()
			case "missing-uuid":
				instance["incus_uuid"] = ""
			case "missing-project":
				instance["project_name"] = ""
			}
			if err := db.Table("instances").Where("id=15").Updates(instance).Error; err != nil {
				t.Fatal(err)
			}
			if scenario != "unconfirmed-create" {
				if err := a.Step(t.Context(), job, "create.native", "", "succeeded"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "revoked" {
				if err := db.Table("tasks").Where("id=43").Updates(platform.Row{"status": "cancelled", "lease_generation": 2}).Error; err != nil {
					t.Fatal(err)
				}
			}
			err := workerPrepareCloudInit(t.Context(), a, job, instance)
			if (err == nil) != wantSkip {
				t.Fatalf("error=%v wantSkip=%t", err, wantSkip)
			}
			wantCalls := int64(0)
			if scenario == "bad-result" || scenario == "agent-error" {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatalf("prepare calls=%d want=%d", calls.Load(), wantCalls)
			}
			start, err := workerStep(t.Context(), a, job, "state.start")
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "started" && start != nil {
				t.Fatal("failed preparation journaled first start")
			}
		})
	}
}
