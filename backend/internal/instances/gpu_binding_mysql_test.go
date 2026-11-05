package instances

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"101cloud/backend/internal/platform"
)

func TestMySQLGPUPolicyViewReturnsFrozenBindingWithoutAllocation(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	id := platform.Uint(created["instance_id"])
	path := "/instances/" + strconv.FormatUint(id, 10)
	f.exec("UPDATE tasks SET status='succeeded',finished_at=NOW(6) WHERE id=?", created["task_id"])
	f.exec("UPDATE instances SET observed_state='running',provisioning_state='ready',observed_at=NOW(6) WHERE id=?", id)
	if n := f.count("gpu_allocations"); n != 0 {
		t.Fatalf("fixture unexpectedly allocated GPU: %d", n)
	}

	// A catalog edit after admission must not replace the instance's frozen ID,
	// model, access mode, or rate. No physical allocation is needed to read them.
	f.exec(`INSERT INTO gpu_price_versions(id,code,version,name,gpu_model,access_mode,gpu_hourly_pt,status,effective_at,created_by_user_id) VALUES(2,'changed-plan-gpu','1','Changed plan GPU','NVIDIA H100','exclusive',9,'active',NOW(6)-INTERVAL 1 HOUR,1)`)
	f.exec(`INSERT INTO policy_versions(id,kind,code,version,source_id,config,status,effective_at,created_by_user_id) VALUES(7,'gpu-pricing','changed-plan-gpu','1',2,'{"gpu_model":"NVIDIA H100","access_mode":"exclusive","gpu_hourly_pt":"9.000000"}','active',NOW(6)-INTERVAL 1 HOUR,1)`)
	f.exec(`UPDATE instance_plans SET policy_versions=JSON_SET(policy_versions,'$."gpu-pricing"','7'),gpu_model='NVIDIA H100',gpu_access_mode='exclusive',revision=revision+1 WHERE id=1`)

	want := platform.Row{"policy_version_id": "6", "model": "NVIDIA A100", "access_mode": "shared", "gpu_hourly_pt": "2.000000"}
	for _, admin := range []bool{false, true} {
		view := f.call(admin, http.MethodGet, path+"/gpu-policy", platform.Row{}, 0, http.StatusOK, "")
		if got := object(view["gpu_binding"]); !reflect.DeepEqual(got, want) {
			t.Fatalf("admin=%t binding=%+v want=%+v", admin, got, want)
		}
	}

	// An unrelated user receives the same not-found privacy behavior and cannot
	// mutate the owner's instance.
	f.exec("INSERT INTO users(id,username,password_hash,role,status) VALUES(3,'other-owner','test-hash','user','active')")
	f.requestUserID = 3
	f.call(false, http.MethodGet, path+"/gpu-policy", platform.Row{}, 0, http.StatusNotFound, "not_found")
	f.call(false, http.MethodPut, path+"/gpu-policy", instanceGPUPolicy{Idle: storedIdlePolicy{Enabled: true, TimeoutSeconds: 900, Action: "detach_gpu"}, Rebalance: storedRebalancePolicy{Enabled: true, AllowLive: true, MaxHourlyPT: "2.000000"}}, platform.Uint(f.instance(id)["revision"]), http.StatusNotFound, "not_found")
	f.requestUserID = 2

	// A legitimate owner PUT retains the read-only binding; the administrator
	// sees the exact same value after the route returns its replacement view.
	cfg := instanceGPUPolicy{Idle: storedIdlePolicy{Enabled: true, TimeoutSeconds: 1200, Action: "detach_gpu"}, Rebalance: storedRebalancePolicy{Enabled: true, AllowLive: true, MaxHourlyPT: "2.000000"}}
	view := f.call(false, http.MethodPut, path+"/gpu-policy", cfg, platform.Uint(f.instance(id)["revision"]), http.StatusOK, "")
	if got := object(view["gpu_binding"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("PUT response binding=%+v want=%+v", got, want)
	}
	adminView := f.call(true, http.MethodGet, path+"/gpu-policy", platform.Row{}, 0, http.StatusOK, "")
	if got := object(adminView["gpu_binding"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("admin response binding=%+v want=%+v", got, want)
	}
}

func TestMySQLGPUPolicyBindingNullAndInvalidBinding(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	id := platform.Uint(created["instance_id"])
	path := "/instances/" + strconv.FormatUint(id, 10)
	f.exec("UPDATE tasks SET status='succeeded',finished_at=NOW(6) WHERE id=?", created["task_id"])
	f.exec("UPDATE instances SET observed_state='running',provisioning_state='ready',observed_at=NOW(6),product_type='custom',instance_plan_id=NULL,plan_snapshot=NULL,policy_versions=JSON_REMOVE(policy_versions,?) WHERE id=?", `$."gpu-pricing"`, id)
	for _, admin := range []bool{false, true} {
		view := f.call(admin, http.MethodGet, path+"/gpu-policy", platform.Row{}, 0, http.StatusOK, "")
		if value, exists := view["gpu_binding"]; !exists || value != nil {
			t.Fatalf("admin=%t no-binding response=%+v", admin, value)
		}
	}
	cfg := instanceGPUPolicy{Idle: storedIdlePolicy{Enabled: true, TimeoutSeconds: 1200, Action: "detach_gpu"}, Rebalance: storedRebalancePolicy{Enabled: true, AllowLive: true, MaxHourlyPT: "2.000000"}}
	view := f.call(false, http.MethodPut, path+"/gpu-policy", cfg, platform.Uint(f.instance(id)["revision"]), http.StatusOK, "")
	if value, exists := view["gpu_binding"]; !exists || value != nil {
		t.Fatalf("unbound PUT response=%+v", value)
	}
	if n := f.count("gpu_allocations"); n != 0 {
		t.Fatalf("binding read/mutation created GPU allocations: %d", n)
	}

	// A stale policy version is an error, not an implicit plan/catalog fallback.
	f.exec("UPDATE instances SET policy_versions=JSON_SET(policy_versions,?,'999999') WHERE id=?", `$."gpu-pricing"`, id)
	for _, admin := range []bool{false, true} {
		prefix := "/api/v1"
		if admin {
			prefix += "/admin"
		}
		f.requests++
		req := httptest.NewRequest(http.MethodGet, prefix+path+"/gpu-policy", nil)
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, req)
		var envelope map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode invalid-binding response: %v", err)
		}
		if w.Code != http.StatusUnprocessableEntity || platform.Text(object(envelope["error"])["code"]) != "validation_failed" {
			t.Fatalf("admin=%t stale binding status=%d response=%s", admin, w.Code, w.Body.String())
		}
	}
}
