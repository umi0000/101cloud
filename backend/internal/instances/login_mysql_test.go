package instances

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
)

func TestMySQLLoginCreateEncryptsAndReplaysWithoutSecretSnapshots(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	password := " test root 密码 "
	key := loginTestKey(t)
	body := f.createBody("1", "2.5")
	body["login"] = LoginRequest{RootPassword: password, SSHPublicKeys: []string{key + " yaml: [comment]", key}}
	raw := jsonText(body)
	call := func(payload string, status int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/instances", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "login-create-retry")
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("create/replay status=%d want=%d", w.Code, status)
		}
		if strings.Contains(w.Body.String(), password) || strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), "$6$") {
			t.Fatal("create/replay response leaked credentials")
		}
		var envelope map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return object(envelope["data"])
	}
	created := call(raw, http.StatusAccepted)
	id := platform.Uint(created["instance_id"])
	first := f.instance(id)
	sealed := []byte(platform.Text(first["login_config_ciphertext"]))
	if len(sealed) == 0 || bytes.Contains(sealed, []byte(password)) || bytes.Contains(sealed, []byte(key)) {
		t.Fatal("create did not seal dedicated login configuration")
	}
	data, err := loginUserData(f.a, first)
	if err != nil || !strings.HasPrefix(data, "#cloud-config\n") {
		t.Fatal("persisted login cannot produce worker cloud-config")
	}
	replayed := call(raw, http.StatusAccepted)
	if platform.Uint(replayed["instance_id"]) != id || platform.Uint(replayed["task_id"]) != platform.Uint(created["task_id"]) || !bytes.Equal(sealed, []byte(platform.Text(f.instance(id)["login_config_ciphertext"]))) || f.count("instances") != 1 || f.count("tasks") != 1 || f.reads.Load() != 1 {
		t.Fatal("same-key retry changed credentials or duplicated admission")
	}
	body["login"] = LoginRequest{RootPassword: "a different password"}
	call(jsonText(body), http.StatusConflict)
	view := f.call(false, http.MethodGet, "/instances/"+strconv.FormatUint(id, 10), platform.Row{}, 0, http.StatusOK, "")
	if strings.Contains(jsonText(view), "login_config") || strings.Contains(jsonText(view), "$6$") {
		t.Fatal("instance DTO exposed login configuration")
	}
	for _, table := range []string{"tasks", "task_steps", "audit_logs", "backend_requests", "usage_sessions"} {
		var records []platform.Row
		if err = f.db.Table(table).Find(&records).Error; err != nil {
			t.Fatal(err)
		}
		encoded := jsonText(records)
		if strings.Contains(encoded, password) || strings.Contains(encoded, key) || strings.Contains(encoded, "$6$") || strings.Contains(encoded, "cloud-init.user-data") {
			t.Fatalf("credential leaked to %s", table)
		}
	}
	if snapshot := platform.Text(first["incus_config_snapshot"]); strings.Contains(snapshot, "user-data") || strings.Contains(snapshot, key) || strings.Contains(snapshot, "$6$") {
		t.Fatal("credential leaked to ordinary instance snapshot")
	}
}

func TestMySQLLoginRequiresExactNodeImageCloudInitCapability(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	f.exec("UPDATE image_nodes SET cloud_init=FALSE WHERE image_id=1 AND node_id=1")
	// Global metadata cannot override the exact node/project image capability.
	f.exec(`UPDATE images SET properties=JSON_OBJECT('cloud_init',true) WHERE id=1`)
	body := f.createBody("1", "2.5")
	body["login"] = LoginRequest{SSHPublicKeys: []string{loginTestKey(t)}}
	f.call(false, http.MethodPost, "/instances", body, 0, http.StatusConflict, "image_login_unsupported")
	for _, table := range []string{"instances", "tasks", "usage_sessions", "credit_reservations"} {
		if f.count(table) != 0 {
			t.Fatalf("unsupported image leaked admission effects into %s", table)
		}
	}
	if f.reads.Load() != 0 {
		t.Fatal("unsupported login reached native dispatch")
	}
	delete(body, "login")
	f.call(false, http.MethodPost, "/instances", body, 0, http.StatusAccepted, "")
}

func TestMySQLQuoteRejectsLoginWithoutPersistingIt(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	body := f.createBody("1", "2.5")
	delete(body, "policy_versions")
	body["login"] = LoginRequest{SSHPublicKeys: []string{loginTestKey(t)}}
	f.call(false, http.MethodPost, "/quotes", body, 0, http.StatusUnprocessableEntity, "validation_failed")
	if f.count("instances") != 0 || f.count("tasks") != 0 || f.count("backend_requests") != 0 {
		t.Fatal("quote with login persisted request or credentials")
	}
}
