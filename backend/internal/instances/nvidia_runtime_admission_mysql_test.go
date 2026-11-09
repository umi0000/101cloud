package instances

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

func nvidiaAdmissionBody(f *planConfigurationFixture, admin bool) (string, platform.Row) {
	if admin {
		return "/api/v1/admin/custom-instances", customLoginBody(f)
	}
	return "/api/v1/instances", f.createBody("1", "2.5")
}

func nvidiaAdmissionRequest(t *testing.T, f *planConfigurationFixture, path, key, payload string, status int) platform.Row {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, req)
	if response.Code != status {
		t.Fatalf("%s: status=%d want=%d: %s", path, response.Code, status, response.Body.String())
	}
	var envelope platform.Row
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return object(envelope["data"])
}

func assertNvidiaAdmissionWithoutGPU(t *testing.T, f *planConfigurationFixture) {
	t.Helper()
	if count := f.count("gpu_allocations"); count != 0 {
		t.Fatalf("runtime preparation allocated GPUs: %d", count)
	}
	var count int64
	if err := f.db.Table("usage_sessions").Where("charge_kind='gpu' OR gpu_allocation_id IS NOT NULL OR gpu_hourly_pt<>0").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("runtime preparation created GPU usage: %d %v", count, err)
	}
	for _, table := range []string{"credit_reservations", "usage_charges"} {
		if err := f.db.Table(table + " AS r").Joins("JOIN usage_sessions AS s ON s.id=r.session_id").Where("s.charge_kind='gpu' OR s.gpu_allocation_id IS NOT NULL OR s.gpu_hourly_pt<>0").Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("runtime preparation created GPU %s: %d %v", table, count, err)
		}
	}
}

func TestMySQLNvidiaRuntimeAdmissionFreezesSelectedProjectWithoutGPU(t *testing.T) {
	for _, role := range []struct {
		name  string
		admin bool
	}{{"member-plan", false}, {"admin-custom", true}} {
		for _, tc := range []struct {
			name          string
			global, ready bool
			vm            bool
		}{
			{"project-ready-global-false", false, true, false},
			{"project-not-ready-global-true", true, false, false},
			{"ordinary-cpu", false, false, false},
			{"vm-gpu-ready-contradiction", true, true, true},
		} {
			t.Run(role.name+"/"+tc.name, func(t *testing.T) {
				f := newPlanConfigurationFixture(t)
				// Even an available physical GPU and a zero GPU quota must not
				// turn library preparation into a GPU reservation or quota request.
				f.exec(`UPDATE user_limits SET max_gpu_allocations=0 WHERE user_id=2`)
				f.exec(`INSERT INTO node_gpus(node_id,uuid,model,memory_mib,max_allocations,status,last_seen_at) VALUES(1,'GPU-not-requested','NVIDIA A100',81920,8,'available',NOW(6))`)
				f.exec(`UPDATE images SET gpu_ready=? WHERE id=1`, tc.global)
				f.exec(`UPDATE image_nodes SET gpu_ready=? WHERE image_id=1 AND node_id=1 AND project_name='default'`, tc.ready)
				f.exec(`INSERT INTO image_nodes(image_id,node_id,project_name,architecture,fingerprint,cloud_init,gpu_ready,properties,status,last_verified_at) VALUES(1,1,'other-project','x86_64',UNHEX(SHA2('plan-image',256)),TRUE,?,JSON_OBJECT(),'ready',NOW(6))`, !tc.ready)
				path, body := nvidiaAdmissionBody(f, role.admin)
				if tc.vm {
					f.exec(`UPDATE images SET image_type='virtual-machine' WHERE id=1`)
					f.exec(`UPDATE instance_plans SET image_type='virtual-machine',gpu_model=NULL,gpu_max_hourly_pt=0,gpu_policy='{"idle":{"enabled":false},"rebalance":{"enabled":false,"max_hourly_pt":"0.000000"}}',gpu_user_permissions=JSON_OBJECT() WHERE id=1`)
					if role.admin {
						object(body["approved_config"])["instance_type"] = "virtual-machine"
					} else {
						body["gpu"] = platform.Row{"count": 0, "model": nil, "access_mode": "shared", "max_hourly_pt": "0"}
					}
				}
				payload := jsonText(body)
				created := nvidiaAdmissionRequest(t, f, path, "nvidia-runtime-frozen", payload, http.StatusAccepted)
				id := platform.Uint(created["instance_id"])
				first := f.instance(id)
				creation := object(object(first["incus_config_snapshot"])["creation"])
				want := tc.ready && !tc.vm
				if got, ok := creation["nvidia_runtime"].(bool); !ok || got != want || platform.Uint(creation["gpu_count"]) != 0 {
					t.Fatalf("selected image runtime decision not frozen: %v want=%t", creation, want)
				}
				assertNvidiaAdmissionWithoutGPU(t, f)
				before := platform.Text(first["incus_config_snapshot"])
				// Catalog edits may affect a future admission, never this approved
				// task or a replay of its original HTTP idempotency key.
				f.exec(`UPDATE images SET gpu_ready=? WHERE id=1`, !tc.global)
				f.exec(`UPDATE image_nodes SET gpu_ready=? WHERE image_id=1 AND node_id=1 AND project_name='default'`, !tc.ready)
				replayed := nvidiaAdmissionRequest(t, f, path, "nvidia-runtime-frozen", payload, http.StatusAccepted)
				if platform.Uint(replayed["instance_id"]) != id || platform.Uint(replayed["task_id"]) != platform.Uint(created["task_id"]) || platform.Text(f.instance(id)["incus_config_snapshot"]) != before || f.count("instances") != 1 || f.count("tasks") != 1 || f.reads.Load() != 1 {
					t.Fatal("catalog edit or idempotent replay changed frozen creation or repeated admission")
				}
				body["name"] = "changed-runtime-request"
				nvidiaAdmissionRequest(t, f, path, "nvidia-runtime-frozen", jsonText(body), http.StatusConflict)
				if platform.Text(f.instance(id)["incus_config_snapshot"]) != before || f.count("instances") != 1 || f.count("tasks") != 1 {
					t.Fatal("same-key changed request rewrote approved runtime decision")
				}
				fresh := nvidiaAdmissionRequest(t, f, path, "nvidia-runtime-new-admission", payload, http.StatusAccepted)
				freshCreation := object(object(f.instance(platform.Uint(fresh["instance_id"]))["incus_config_snapshot"])["creation"])
				if platform.Bool(freshCreation["nvidia_runtime"]) != (!tc.ready && !tc.vm) || platform.Text(f.instance(id)["incus_config_snapshot"]) != before {
					t.Fatal("future admission did not use changed project capability independently of frozen task")
				}
				assertNvidiaAdmissionWithoutGPU(t, f)
			})
		}
	}
}

func TestMySQLNvidiaRuntimeAdmissionPreservesGPUApprovalAndAllocation(t *testing.T) {
	for _, role := range []struct {
		name  string
		admin bool
	}{{"member-plan", false}, {"admin-custom", true}} {
		for _, scenario := range []string{"approved", "project-not-ready", "quota-zero"} {
			t.Run(role.name+"/"+scenario, func(t *testing.T) {
				f := newPlanConfigurationFixture(t)
				f.exec(`UPDATE policy_versions SET config=JSON_SET(config,'$.sm_weight',0.5,'$.memory_weight',0.5) WHERE id=4`)
				f.exec(`UPDATE nodes SET resources=JSON_OBJECT('architecture','x86_64','gpus',JSON_ARRAY(JSON_OBJECT('uuid','GPU-runtime-approved','model','NVIDIA A100','memory_mib',81920))),resources_sampled_at=NOW(6) WHERE id=1`)
				f.exec(`INSERT INTO node_gpu_load_snapshots(node_id,sampled_at,resources) VALUES(1,NOW(6),'{"gpus":[{"uuid":"GPU-runtime-approved","utilization_percent":0,"memory_utilization_percent":0}]}')`)
				f.exec(`INSERT INTO node_gpus(node_id,uuid,model,memory_mib,max_allocations,status,last_seen_at) VALUES(1,'GPU-runtime-approved','NVIDIA A100',81920,8,'available',NOW(6))`)
				path, body := nvidiaAdmissionBody(f, role.admin)
				gpu := platform.Row{"count": 1, "model": "NVIDIA A100", "access_mode": "shared", "max_hourly_pt": "2.5"}
				if role.admin {
					object(body["approved_config"])["gpu"] = gpu
				} else {
					body["gpu"] = gpu
				}
				status := http.StatusAccepted
				switch scenario {
				case "project-not-ready":
					f.exec(`UPDATE image_nodes SET gpu_ready=FALSE WHERE image_id=1 AND node_id=1 AND project_name='default'`)
					status = http.StatusUnprocessableEntity
				case "quota-zero":
					f.exec(`UPDATE user_limits SET max_gpu_allocations=0 WHERE user_id=2`)
					status = http.StatusConflict
				}
				created := nvidiaAdmissionRequest(t, f, path, "nvidia-runtime-gpu-approval", jsonText(body), status)
				if scenario != "approved" {
					for _, table := range []string{"instances", "tasks", "gpu_allocations", "usage_sessions", "credit_reservations"} {
						if f.count(table) != 0 {
							t.Fatalf("GPU approval failure persisted %s", table)
						}
					}
					return
				}
				id := platform.Uint(created["instance_id"])
				instance := f.instance(id)
				creation := object(object(instance["incus_config_snapshot"])["creation"])
				if !platform.Bool(creation["nvidia_runtime"]) || platform.Uint(creation["gpu_count"]) != 1 || f.count("gpu_allocations") != 1 {
					t.Fatalf("approved GPU request changed: %v", creation)
				}
				var allocation platform.Row
				if err := f.db.Table("gpu_allocations").Where("instance_id=?", id).Take(&allocation).Error; err != nil {
					t.Fatal(err)
				}
				if platform.Text(allocation["status"]) != "reserved" || platform.Bool(allocation["configured"]) || platform.Bool(allocation["observed"]) || platform.Text(allocation["gpu_hourly_pt"]) != "2.000000" {
					t.Fatalf("library preparation bypassed explicit GPU allocation lifecycle: %v", allocation)
				}
			})
		}
	}
}

func TestMySQLNvidiaRuntimeAdmissionDoesNotPrepareNativeImport(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	body := customLoginBody(f)
	delete(body, "name")
	body["incus_uuid"] = "11111111-1111-1111-1111-111111111111"
	queued := nvidiaAdmissionRequest(t, f, "/api/v1/admin/nodes/1/instance-imports", "nvidia-runtime-native-import", jsonText(body), http.StatusAccepted)
	taskID := platform.Uint(queued["task_id"])
	task, err := platform.Get(f.db, "tasks", taskID, false)
	if err != nil {
		t.Fatal(err)
	}
	var parameters struct {
		Approved Spec              `json:"approved_config"`
		Versions map[string]string `json:"policy_versions"`
	}
	if err = json.Unmarshal([]byte(platform.Text(task["parameters_redacted"])), &parameters); err != nil {
		t.Fatal(err)
	}
	image, err := platform.Get(f.db, "images", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	native := platform.Row{
		"name": "native-not-prepared", "architecture": "x86_64", "status": "Stopped",
		"config": platform.Row{"volatile.uuid": body["incus_uuid"], "volatile.base_image": workerFingerprint(image["fingerprint"])},
		"_ip":    "192.0.2.10", "_gpu_uuids": []string{}, "_import_task_id": taskID,
	}
	before := jsonText(native)
	var imported platform.Row
	// Consume the real HTTP-enqueued approval through the production import
	// admission; native discovery/config validation belongs to worker regressions.
	if err = f.db.Transaction(func(db *gorm.DB) error {
		var admissionErr error
		imported, admissionErr = reserveImported(t.Context(), f.a, db, 2, parameters.Approved, native, parameters.Versions)
		return admissionErr
	}); err != nil {
		t.Fatal(err)
	}
	stored := f.instance(platform.Uint(imported["id"]))
	creation := object(object(stored["incus_config_snapshot"])["creation"])
	if runtime, ok := creation["nvidia_runtime"].(bool); !ok || runtime || platform.Text(stored["provisioning_state"]) != "ready" || platform.Uint(stored["import_task_id"]) != taskID {
		t.Fatalf("native import gained runtime preparation: %v", creation)
	}
	if jsonText(native) != before || f.reads.Load() != 0 || f.count("tasks") != 1 {
		t.Fatal("native import admission changed native config or dispatched create preparation")
	}
	assertNvidiaAdmissionWithoutGPU(t, f)
}
