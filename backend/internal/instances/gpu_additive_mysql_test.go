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

	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/platform"
)

const additiveInstanceUUID = "11111111-1111-4111-8111-111111111111"

// Uses the fully migrated isolated MySQL database, actual loopback HTTP admission,
// production queue worker and certificate-pinned TLS Agent transport. The Agent
// responses are protocol fixtures, not claims about a real GPU or Incus runtime.
func TestMySQLGPUAdditiveHTTPWorkerTLS(t *testing.T) {
	for _, tc := range []struct {
		name    string
		action  string
		allowed bool
	}{
		{"owned_shared_control", "attach", true},
		{"unrelated_host_shared_control", "attach", true},
		{"old_healthy_full_attribution", "attach", true},
		{"missing_context_old_agent", "attach", false},
		{"nonboolean_context", "attach", false},
		{"unknown_references", "attach", false},
		{"card_unknown_references", "attach", false},
		{"reference_scan_incomplete", "attach", false},
		{"missing_shared_reason", "attach", false},
		{"extra_native_gpu", "attach", false},
		{"extra_native_gpu_full_attribution", "attach", false},
		{"unresolved_native_gpu", "attach", false},
		{"native_identity_changed", "attach", false},
		{"stale_frame", "attach", false},
		{"future_frame", "attach", false},
		{"explicit_stale_frame", "attach", false},
		{"incomplete_collection", "attach", false},
		{"wrong_uuid", "attach", false},
		{"wrong_name", "attach", false},
		{"wrong_project", "attach", false},
		{"instance_not_running", "attach", false},
		{"old_agent_rejects_add_only", "attach", false},
		{"shared_control_detach", "detach", false},
		{"shared_control_rebind", "rebind", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlanConfigurationFixture(t)
			created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
			id := platform.Uint(created["instance_id"])
			f.exec("UPDATE tasks SET status='succeeded',finished_at=NOW(6) WHERE id=?", created["task_id"])
			f.exec(`UPDATE instances SET provisioning_state='ready',observed_state='running',incus_uuid=?,observed_at=NOW(6),allowed_user_actions='["gpu_attach","gpu_detach","gpu_rebind"]' WHERE id=?`, additiveInstanceUUID, id)
			f.exec(`UPDATE policy_versions SET config=JSON_SET(config,'$.sm_weight',0.5,'$.memory_weight',0.5) WHERE id=4`)
			for i := 1; i <= 2; i++ {
				f.exec("INSERT INTO node_gpus(id,node_id,uuid,model,memory_mib,max_allocations,status,last_seen_at) VALUES(?,1,?,'NVIDIA A100',81920,8,'available',NOW(6))", i, fmt.Sprintf("GPU-add-%d", i))
			}
			f.exec(`INSERT INTO node_gpu_load_snapshots(node_id,sampled_at,resources) VALUES(1,NOW(6),'{"gpus":[{"uuid":"GPU-add-1","utilization_percent":50,"memory_utilization_percent":10},{"uuid":"GPU-add-2","utilization_percent":0,"memory_utilization_percent":0}]}')`)
			f.exec("INSERT INTO gpu_allocations(id,instance_id,user_id,node_id,gpu_id,gpu_price_version_id,gpu_hourly_pt,status,access_mode,reserved_at,configured,observed) VALUES(1,?,2,1,1,1,2,'active','shared',NOW(6),true,true)", id)
			r := f.instance(id)
			if err := billing.InstanceUsage(t.Context(), f.db, r, "gpu_attached", []uint64{1}); err != nil {
				t.Fatal(err)
			}
			q := platform.Row{"complete": false, "attribution_complete": false, "context_attribution_complete": true, "reasons": []string{"gpu_shared_control_references", "gpu_utilization_no_samples"}}
			instance := platform.Row{"uuid": additiveInstanceUUID, "name": r["incus_name"], "project": r["project_name"], "state": "Running", "started_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), "quality": q}
			instance["gpus"] = []any{platform.Row{"uuid": "GPU-add-1", "configured": true, "observed": true, "state": "in_use", "process_count": 1, "referencing_process_count": nil, "used_memory_bytes": 1024, "quality": q}}
			frame := platform.Row{"collection_complete": true, "collected_at": time.Now().UTC().Format(time.RFC3339Nano), "stale": false, "instances": []any{instance}, "host_quality": platform.Row{"attribution_complete": false, "reasons": []string{"gpu_shared_control_references"}}}
			devices := platform.Row{"existing-gpu-name": platform.Row{"type": "gpu", "gputype": "physical", "pci": "0000:41:00.0"}}
			native := platform.Row{"config": platform.Row{"volatile.uuid": additiveInstanceUUID}, "expanded_devices": devices}
			switch tc.name {
			case "unrelated_host_shared_control", "old_healthy_full_attribution":
				q["attribution_complete"] = true
				q["reasons"] = []string{"gpu_utilization_no_samples"}
				delete(q, "context_attribution_complete")
				if tc.name == "old_healthy_full_attribution" {
					frame["host_quality"] = platform.Row{"attribution_complete": true}
				}
			case "missing_context_old_agent":
				delete(q, "context_attribution_complete")
			case "nonboolean_context":
				q["context_attribution_complete"] = "true"
			case "unknown_references":
				q["reasons"] = []string{"gpu_shared_control_references", "gpu_references_unresolved"}
			case "card_unknown_references":
				card := object(rows(instance["gpus"])[0])
				card["quality"] = platform.Row{"attribution_complete": false, "context_attribution_complete": true, "reasons": []string{"gpu_shared_control_references", "gpu_references_unresolved"}}
			case "reference_scan_incomplete":
				q["reasons"] = []string{"gpu_shared_control_references", "gpu_reference_scan_incomplete"}
			case "missing_shared_reason":
				q["reasons"] = []string{"gpu_utilization_no_samples"}
			case "extra_native_gpu", "extra_native_gpu_full_attribution":
				devices["unexpected-native-gpu"] = platform.Row{"type": "gpu", "gputype": "physical", "pci": "0000:43:00.0"}
				if tc.name == "extra_native_gpu_full_attribution" {
					q["attribution_complete"] = true
				}
			case "unresolved_native_gpu":
				devices["unexpected-native-gpu"] = platform.Row{"type": "gpu", "gputype": "physical", "pci": "0000:99:00.0"}
			case "native_identity_changed":
				object(native["config"])["volatile.uuid"] = "22222222-2222-4222-8222-222222222222"
			case "stale_frame":
				frame["collected_at"] = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
			case "future_frame":
				frame["collected_at"] = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
			case "explicit_stale_frame":
				frame["stale"] = true
			case "incomplete_collection":
				frame["collection_complete"] = false
			case "wrong_uuid":
				instance["uuid"] = "22222222-2222-4222-8222-222222222222"
			case "wrong_name":
				instance["name"] = "another-instance"
			case "wrong_project":
				instance["project"] = "another-project"
			case "instance_not_running":
				instance["state"] = "stopped"
			}
			var mutations, metrics atomic.Int64
			var configured atomic.Bool
			f.native = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/gpu") {
					mutations.Add(1)
					var payload platform.Row
					if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
						t.Error(err)
						http.Error(w, "bad request", http.StatusBadRequest)
						return
					}
					addOnly, explicit := payload["add_only"].(bool)
					if !explicit || !addOnly || !platform.Bool(payload["allow_live"]) || jsonText(payload["gpu_uuids"]) != `["GPU-add-1","GPU-add-2"]` {
						t.Errorf("unsafe additive protocol payload: %+v", payload)
						http.Error(w, "unsafe additive request", http.StatusBadRequest)
						return
					}
					if tc.name == "old_agent_rejects_add_only" {
						w.WriteHeader(http.StatusBadRequest)
						_ = json.NewEncoder(w).Encode(platform.Row{"error": "unknown field add_only"})
						return
					}
					configured.Store(true)
					_ = json.NewEncoder(w).Encode(platform.Row{"operation_id": "additive-operation", "operation_completed": true, "configuration_state": "observed"})
					return
				}
				if request.Method != http.MethodGet {
					mutations.Add(1)
					t.Errorf("unexpected native mutation: %s %s", request.Method, request.URL)
					http.Error(w, "unexpected mutation", http.StatusBadRequest)
					return
				}
				switch request.URL.Path {
				case "/agent/v1/instances/metrics":
					metrics.Add(1)
					_ = json.NewEncoder(w).Encode(frame)
				case "/agent/v1/resources":
					_ = json.NewEncoder(w).Encode(platform.Row{"gpus": []any{platform.Row{"uuid": "GPU-add-1", "pci_address": "0000:41:00.0"}, platform.Row{"uuid": "GPU-add-2", "pci_address": "0000:42:00.0"}, platform.Row{"uuid": "GPU-add-3", "pci_address": "0000:43:00.0"}}})
				case "/1.0/instances/" + platform.Text(r["incus_name"]) + "/state":
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status": "Running"}})
				case "/1.0/instances/" + platform.Text(r["incus_name"]):
					if configured.Load() {
						_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"config": native["config"], "expanded_devices": platform.Row{"existing-gpu-name": devices["existing-gpu-name"], "new-gpu-name": platform.Row{"type": "gpu", "gputype": "physical", "pci": "0000:42:00.0"}}}})
					} else {
						_ = json.NewEncoder(w).Encode(platform.Row{"metadata": native})
					}
				default:
					t.Errorf("unexpected Agent read: %s", request.URL)
					http.Error(w, "unexpected read", http.StatusNotFound)
				}
			})
			server := httptest.NewServer(f.router)
			defer server.Close()
			call := func(path string, body platform.Row, wantStatus int) platform.Row {
				t.Helper()
				request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/instances/"+fmt.Sprint(id)+path, strings.NewReader(jsonText(body)))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, platform.Uint(r["revision"])))
				request.Header.Set("Idempotency-Key", "additive-"+tc.name)
				response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				var envelope platform.Row
				if err = json.NewDecoder(response.Body).Decode(&envelope); err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != wantStatus {
					t.Fatalf("admission status=%d want=%d: %+v", response.StatusCode, wantStatus, envelope)
				}
				return envelope
			}
			var taskID uint64
			if tc.action == "attach" {
				response := call("/gpu-attachments", platform.Row{"count": 1, "allow_live": true, "model": "NVIDIA A100", "access_mode": "shared", "max_hourly_pt": "2.5"}, http.StatusAccepted)
				taskID = platform.Uint(object(response["data"])["task_id"])
			} else {
				// Stored evidence must independently block release; context quality is
				// intentionally not persisted and cannot turn this sample into idle.
				f.exec(`INSERT INTO instance_metric_samples(instance_id,node_id,agent_boot_id,sample_sequence,instance_uuid,instance_state,sampled_at,window_start_at,window_end_at,window_seconds,gpu_state,collection_complete,metrics_complete,attribution_complete,reference_scan_quality) VALUES(?,1,UNHEX(REPEAT('11',16)),1,?,'running',NOW(6),NOW(6),NOW(6),0,'in_use',true,false,false,'partial')`, id, additiveInstanceUUID)
				path := "/gpu-detachments"
				if tc.action == "rebind" {
					path = "/gpu-rebindings"
				}
				response := call(path, platform.Row{"allocation_ids": []string{"1"}, "allow_live": true}, http.StatusConflict)
				if platform.Text(object(response["error"])["code"]) != "observation_incomplete" {
					t.Fatalf("release admission did not retain strict stored evidence: %+v", response)
				}
				// Model a task admitted earlier while idle. Current live evidence must
				// still reject both removals and rebindings before Agent mutation.
				f.exec("UPDATE gpu_allocations SET status='releasing',release_requested_at=NOW(6) WHERE id=1")
				targets := []uint64{}
				if tc.action == "rebind" {
					f.exec("INSERT INTO gpu_allocations(id,instance_id,user_id,node_id,gpu_id,gpu_price_version_id,gpu_hourly_pt,status,access_mode,reserved_at,configured,observed) VALUES(2,?,2,1,2,1,2,'reserved','shared',NOW(6),false,false)", id)
					targets = append(targets, 2)
				}
				queued, err := platform.Enqueue(f.db, "instance.gpu", 2, 1, id, true, platform.Row{"action": tc.action, "allocation_ids": []uint64{1}, "target_ids": targets, "allow_live": true})
				if err != nil {
					t.Fatal(err)
				}
				taskID = platform.Uint(queued["task_id"])
			}
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
					t.Error("additive worker did not stop")
				}
			}()
			deadline := time.After(10 * time.Second)
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			var task platform.Row
		waitTask:
			for {
				task = forceStopStored(t, f, taskID)
				switch platform.Text(task["status"]) {
				case "succeeded", "failed", "unknown", "cancelled":
					break waitTask
				}
				select {
				case <-deadline:
					t.Fatalf("worker did not settle: %+v", task)
				case <-ticker.C:
				}
			}
			cancel()
			<-done
			if tc.allowed {
				if platform.Text(task["status"]) != "succeeded" || mutations.Load() != 1 || metrics.Load() != 1 || !configured.Load() {
					t.Fatalf("additive success missing: task=%+v mutations=%d metrics=%d", task, mutations.Load(), metrics.Load())
				}
				var active int64
				if err := f.db.Table("gpu_allocations").Where("instance_id=? AND status='active' AND configured=true AND released_at IS NULL", id).Count(&active).Error; err != nil || active != 2 {
					t.Fatalf("existing/new allocations not preserved active: count=%d err=%v", active, err)
				}
			} else {
				wantMutations := int64(0)
				if tc.name == "old_agent_rejects_add_only" {
					wantMutations = 1
				}
				if platform.Text(task["status"]) != "unknown" || mutations.Load() != wantMutations || configured.Load() {
					t.Fatalf("unsafe evidence reached configuration: task=%+v mutations=%d", task, mutations.Load())
				}
				if tc.name != "old_agent_rejects_add_only" {
					code := "observation_incomplete"
					if tc.name == "native_identity_changed" {
						code = "instance_identity_changed"
					}
					if platform.Text(task["error_code"]) != code {
						t.Fatalf("unexpected rejection: %+v", task)
					}
				}
				var released int64
				if err := f.db.Table("gpu_allocations").Where("instance_id=? AND (status='released' OR release_verified=true)", id).Count(&released).Error; err != nil || released != 0 {
					t.Fatalf("uncertain evidence released allocations: count=%d err=%v", released, err)
				}
			}
		})
	}
}
