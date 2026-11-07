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

// This boundary regression uses the migrated disposable MySQL fixture, real
// admission HTTP, production task worker, and pinned TLS Agent transport. Agent
// responses model two distinct owners of one physical card, not a real GPU run.
func TestMySQLGPUSameCardRebindHTTPWorkerTLS(t *testing.T) {
	for _, scenario := range []string{
		"idle_owner_rebind", "release_card_absent", "cached_then_fresh_release",
		"active_owner_stored", "stored_stale", "stored_wrong_uuid", "stored_unknown_references",
		"active_owner_live", "live_stale", "live_wrong_uuid", "live_wrong_name", "live_wrong_project", "live_unknown_references", "live_incomplete_attribution", "native_wrong_uuid",
		"release_stale", "release_wrong_uuid", "release_still_configured", "release_still_observed", "release_unknown_references", "release_memory_in_use",
	} {
		t.Run(scenario, func(t *testing.T) {
			f := newPlanConfigurationFixture(t)
			const activeUUID = "11111111-1111-4111-8111-111111111111"
			const idleUUID = "22222222-2222-4222-8222-222222222222"
			owners := make([]platform.Row, 0, 2)
			for i, uuid := range []string{activeUUID, idleUUID} {
				body := f.createBody("1", "2.5")
				body["name"] = fmt.Sprintf("same-card-owner-%d", i)
				created := f.call(false, http.MethodPost, "/instances", body, 0, http.StatusAccepted, "")
				id := platform.Uint(created["instance_id"])
				f.exec("UPDATE tasks SET status='succeeded',finished_at=NOW(6) WHERE id=?", created["task_id"])
				f.exec(`UPDATE instances SET provisioning_state='ready',observed_state='running',incus_uuid=?,observed_at=NOW(6),allowed_user_actions='["gpu_rebind"]' WHERE id=?`, uuid, id)
				owners = append(owners, f.instance(id))
			}
			f.exec(`UPDATE policy_versions SET config=JSON_SET(config,'$.sm_weight',0.5,'$.memory_weight',0.5) WHERE id=4`)
			for i := 1; i <= 2; i++ {
				f.exec("INSERT INTO node_gpus(id,node_id,uuid,model,memory_mib,max_allocations,status,last_seen_at) VALUES(?,1,?,'NVIDIA A100',81920,8,'available',NOW(6))", i, fmt.Sprintf("GPU-rebind-%d", i))
			}
			f.exec(`INSERT INTO node_gpu_load_snapshots(node_id,sampled_at,resources) VALUES(1,NOW(6),'{"gpus":[{"uuid":"GPU-rebind-1","utilization_percent":29,"memory_utilization_percent":1},{"uuid":"GPU-rebind-2","utilization_percent":0,"memory_utilization_percent":0}]}')`)
			for i, owner := range owners {
				f.exec("INSERT INTO gpu_allocations(id,instance_id,user_id,node_id,gpu_id,gpu_price_version_id,gpu_hourly_pt,status,access_mode,reserved_at,configured,observed) VALUES(?,?,2,1,1,1,2,'active','shared',NOW(6),true,true)", i+1, owner["id"])
				if err := billing.InstanceUsage(t.Context(), f.db, owner, "gpu_attached", []uint64{uint64(i + 1)}); err != nil {
					t.Fatal(err)
				}
			}
			r := owners[1]
			allocationID := uint64(2)
			if strings.HasPrefix(scenario, "active_owner_") {
				r, allocationID = owners[0], 1
			}
			stamp := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
			sampleUUID := platform.Text(r["incus_uuid"])
			if scenario == "stored_stale" {
				stamp = stamp.Add(-2 * time.Minute)
			}
			if scenario == "stored_wrong_uuid" {
				sampleUUID = activeUUID
			}
			// active_owner_live models an admission-time idle observation followed
			// by new shared-control use before worker preflight; it must not release.
			full := scenario != "active_owner_stored"
			sample := platform.Row{"instance_id": r["id"], "node_id": 1, "agent_boot_id": []byte(strings.Repeat("s", 16)), "sample_sequence": 1, "instance_uuid": sampleUUID, "instance_state": "running", "sampled_at": stamp, "window_start_at": stamp, "window_end_at": stamp, "window_seconds": 0, "gpu_state": "idle_candidate", "collection_complete": true, "metrics_complete": false, "attribution_complete": full, "reference_scan_quality": "complete"}
			if err := f.db.Table("instance_metric_samples").Create(&sample).Error; err != nil {
				t.Fatal(err)
			}
			sampleID, err := platform.InsertedID(sample)
			if err != nil {
				t.Fatal(err)
			}
			cardSample := platform.Row{"sample_id": sampleID, "instance_id": r["id"], "node_id": 1, "sampled_at": stamp, "gpu_uuid": "GPU-rebind-1", "gpu_id": 1, "gpu_state": "idle_candidate", "memory_used_bytes": 0, "utilization_pct": nil, "process_count": 0, "reference_count": 0, "attribution_complete": full, "reference_scan_quality": "complete", "compute_process_support": "supported", "graphics_process_support": "supported", "utilization_support": "supported", "memory_support": "supported"}
			if scenario == "active_owner_stored" {
				cardSample["gpu_state"], cardSample["process_count"], cardSample["reference_count"], cardSample["memory_used_bytes"] = "in_use", 1, nil, 678*1024*1024
			}
			if scenario == "stored_unknown_references" {
				cardSample["reference_count"] = nil
				cardSample["reference_scan_quality"] = "unknown"
			}
			if err = f.db.Table("instance_gpu_metric_samples").Create(&cardSample).Error; err != nil {
				t.Fatal(err)
			}

			var mutations, metricReads, releaseReads atomic.Int64
			var configured atomic.Bool
			preMutationStamp := time.Now().UTC().Format(time.RFC3339Nano)
			quality := func(full bool, reasons ...string) platform.Row {
				return platform.Row{"complete": false, "attribution_complete": full, "context_attribution_complete": true, "reasons": reasons}
			}
			activeQ := func() platform.Row { return quality(false, "gpu_shared_control_references") }
			idleQ := func() platform.Row { return quality(true, "gpu_utilization_no_samples") }
			f.native = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				// Match the real project query contract, not merely a URL suffix.
				if request.URL.Path != "/agent/v1/resources" && request.URL.Query().Get("project") != platform.Text(r["project_name"]) {
					t.Errorf("wrong Agent project: %s", request.URL)
					http.Error(w, "wrong project", http.StatusBadRequest)
					return
				}
				if request.Method == http.MethodPut && request.URL.Path == "/agent/v1/instances/"+platform.Text(r["incus_name"])+"/gpu" {
					mutations.Add(1)
					var payload platform.Row
					if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
						t.Error(err)
						http.Error(w, "bad request", http.StatusBadRequest)
						return
					}
					_, addOnly := payload["add_only"]
					if addOnly || !platform.Bool(payload["allow_live"]) || jsonText(payload["gpu_uuids"]) != `["GPU-rebind-2"]` {
						t.Errorf("incorrect rebind payload: %+v", payload)
						http.Error(w, "bad rebind", http.StatusBadRequest)
						return
					}
					configured.Store(true)
					_ = json.NewEncoder(w).Encode(platform.Row{"operation_id": "rebind-operation", "operation_completed": true, "configuration_state": "observed"})
					return
				}
				if request.Method != http.MethodGet {
					t.Errorf("unexpected Agent mutation: %s %s", request.Method, request.URL)
					http.Error(w, "unexpected mutation", http.StatusBadRequest)
					return
				}
				switch request.URL.Path {
				case "/agent/v1/instances/metrics":
					metricReads.Add(1)
					activeCard := platform.Row{"uuid": "GPU-rebind-1", "configured": true, "observed": true, "state": "in_use", "process_count": 1, "referencing_process_count": nil, "used_memory_bytes": 678 * 1024 * 1024, "sm_utilization_percent": 29, "quality": activeQ()}
					idleCard := platform.Row{"uuid": "GPU-rebind-1", "configured": true, "observed": false, "state": "idle_candidate", "process_count": 0, "referencing_process_count": 0, "used_memory_bytes": 0, "sm_utilization_percent": nil, "quality": idleQ()}
					active := platform.Row{"uuid": activeUUID, "name": owners[0]["incus_name"], "project": owners[0]["project_name"], "state": "Running", "quality": activeQ(), "gpus": []any{activeCard}}
					idle := platform.Row{"uuid": idleUUID, "name": owners[1]["incus_name"], "project": owners[1]["project_name"], "state": "Running", "quality": idleQ(), "gpus": []any{idleCard}}
					frame := platform.Row{"collection_complete": true, "collected_at": time.Now().UTC().Format(time.RFC3339Nano), "stale": false, "instances": []any{active, idle}, "quality": activeQ(), "host_quality": activeQ()}
					if configured.Load() {
						n := releaseReads.Add(1)
						idleCard["configured"] = false
						idleCard["state"] = "not_attached"
						idle["gpus"] = []any{idleCard, platform.Row{"uuid": "GPU-rebind-2", "configured": true, "observed": false, "state": "idle_candidate", "process_count": 0, "referencing_process_count": 0, "used_memory_bytes": 0, "sm_utilization_percent": nil, "quality": idleQ()}}
						switch scenario {
						case "release_card_absent":
							idle["gpus"] = rows(idle["gpus"])[1:]
						case "cached_then_fresh_release":
							if n == 1 {
								frame["collected_at"] = preMutationStamp
								idleCard["configured"] = true
							}
						case "release_stale":
							frame["collected_at"] = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
						case "release_wrong_uuid":
							idle["uuid"] = activeUUID
						case "release_still_configured":
							idleCard["configured"] = true
						case "release_still_observed":
							idleCard["observed"] = true
						case "release_unknown_references":
							idleCard["referencing_process_count"] = nil
						case "release_memory_in_use":
							idleCard["used_memory_bytes"] = 1
						}
					} else {
						switch scenario {
						case "live_stale":
							frame["collected_at"] = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
						case "live_wrong_uuid":
							idle["uuid"] = activeUUID
						case "live_wrong_name":
							idle["name"] = active["name"]
						case "live_wrong_project":
							idle["project"] = "another-project"
						case "live_unknown_references":
							idleCard["referencing_process_count"] = nil
						case "live_incomplete_attribution":
							idle["quality"], idleCard["quality"] = activeQ(), activeQ()
						}
					}
					_ = json.NewEncoder(w).Encode(frame)
				case "/agent/v1/resources":
					_ = json.NewEncoder(w).Encode(platform.Row{"gpus": []any{platform.Row{"uuid": "GPU-rebind-1", "pci_address": "0000:41:00.0"}, platform.Row{"uuid": "GPU-rebind-2", "pci_address": "0000:42:00.0"}}})
				case "/1.0/instances/" + platform.Text(r["incus_name"]) + "/state":
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status": "Running"}})
				case "/1.0/instances/" + platform.Text(r["incus_name"]):
					uuid := platform.Text(r["incus_uuid"])
					if scenario == "native_wrong_uuid" {
						uuid = activeUUID
					}
					pci := "0000:41:00.0"
					if configured.Load() {
						pci = "0000:42:00.0"
					}
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"config": platform.Row{"volatile.uuid": uuid}, "expanded_devices": platform.Row{"physical-gpu": platform.Row{"type": "gpu", "gputype": "physical", "pci": pci}}}})
				default:
					t.Errorf("unexpected Agent read: %s", request.URL)
					http.Error(w, "unexpected read", http.StatusNotFound)
				}
			})

			server := httptest.NewServer(f.router)
			defer server.Close()
			request, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/instances/%v/gpu-rebindings", server.URL, r["id"]), strings.NewReader(jsonText(platform.Row{"allocation_ids": []string{fmt.Sprint(allocationID)}, "allow_live": true})))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, platform.Uint(r["revision"])))
			request.Header.Set("Idempotency-Key", "same-card-"+scenario)
			response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var envelope platform.Row
			if err = json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if scenario == "active_owner_stored" || strings.HasPrefix(scenario, "stored_") {
				if response.StatusCode != http.StatusConflict || platform.Text(object(envelope["error"])["code"]) != "observation_incomplete" || mutations.Load() != 0 {
					t.Fatalf("unsafe stored evidence admitted: status=%d response=%+v mutations=%d", response.StatusCode, envelope, mutations.Load())
				}
				if f.count("gpu_allocations") != 2 {
					t.Fatal("rejected admission reserved a target GPU")
				}
				return
			}
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("rebind admission failed: status=%d response=%+v", response.StatusCode, envelope)
			}
			taskID := platform.Uint(object(envelope["data"])["task_id"])
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
					t.Error("rebind worker did not stop")
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
					t.Fatalf("rebind worker did not settle: %+v", task)
				case <-ticker.C:
				}
			}
			cancel()
			<-done
			var source, other platform.Row
			if err = f.db.Table("gpu_allocations").Where("id=?", allocationID).Take(&source).Error; err != nil {
				t.Fatal(err)
			}
			otherID := uint64(1)
			if allocationID == 1 {
				otherID = 2
			}
			if err = f.db.Table("gpu_allocations").Where("id=?", otherID).Take(&other).Error; err != nil {
				t.Fatal(err)
			}
			if platform.Text(other["status"]) != "active" || !platform.Bool(other["configured"]) || !platform.Bool(other["observed"]) || platform.Bool(other["release_verified"]) {
				t.Fatalf("other owner's allocation changed: %+v", other)
			}
			success := scenario == "idle_owner_rebind" || scenario == "release_card_absent" || scenario == "cached_then_fresh_release"
			if success {
				wantReads := int64(1)
				if scenario == "cached_then_fresh_release" {
					wantReads = 2
				}
				if platform.Text(task["status"]) != "succeeded" || mutations.Load() != 1 || releaseReads.Load() != wantReads || metricReads.Load() != wantReads+1 || !platform.Bool(source["release_verified"]) || platform.Text(source["status"]) != "released" || platform.Bool(source["configured"]) || platform.Bool(source["observed"]) {
					t.Fatalf("rebind/release not verified: task=%+v source=%+v mutations=%d metrics=%d release=%d", task, source, mutations.Load(), metricReads.Load(), releaseReads.Load())
				}
				var target platform.Row
				if err = f.db.Table("gpu_allocations").Where("instance_id=? AND gpu_id=2", r["id"]).Take(&target).Error; err != nil || platform.Text(target["status"]) != "active" || !platform.Bool(target["configured"]) {
					t.Fatalf("target not activated: %+v err=%v", target, err)
				}
				result := object(task["result_redacted"])
				if !platform.Bool(result["release_verified"]) || !platform.Bool(result["configuration_verified"]) {
					t.Fatalf("task did not publish verified result: %+v", task)
				}
			} else {
				wantMutations := int64(0)
				if strings.HasPrefix(scenario, "release_") {
					wantMutations = 1
				}
				code := "observation_incomplete"
				if scenario == "native_wrong_uuid" {
					code = "instance_identity_changed"
				}
				if scenario == "release_memory_in_use" {
					code = "gpu_in_use"
				}
				if platform.Text(task["status"]) != "unknown" || platform.Text(task["error_code"]) != code || mutations.Load() != wantMutations || platform.Bool(source["release_verified"]) || platform.Text(source["status"]) == "released" {
					t.Fatalf("unsafe evidence released GPU: task=%+v source=%+v mutations=%d", task, source, mutations.Load())
				}
			}
		})
	}
}
