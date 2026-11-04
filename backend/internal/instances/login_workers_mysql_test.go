package instances

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"101cloud/backend/internal/platform"
)

func TestMySQLLoginInitializationWaitsOnceWithoutAdminTemplateOrSecretSnapshots(t *testing.T) {
	var execs atomic.Int64
	var observedData string
	var passwordHash string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			execs.Add(1)
			var body platform.Row
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid wait request")
			}
			want := []any{"/usr/bin/timeout", "--signal=KILL", "600", "/bin/sh", "-c", CloudInitReadyCommand, "cloud-init-ready", "login"}
			if !reflect.DeepEqual(rows(body["command"]), want) || platform.Bool(body["record-output"]) || platform.Bool(body["interactive"]) {
				t.Error("login did not execute a bounded output-disabled cloud-init wait")
			}
			if strings.Contains(jsonText(body), "user-data") || strings.Contains(jsonText(body), passwordHash) {
				t.Error("wait command transmitted credential configuration")
			}
			_ = json.NewEncoder(w).Encode(platform.Row{"operation": "/1.0/operations/login-wait"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/wait"):
			_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status_code": 200, "metadata": platform.Row{"return": 0}}})
		case r.Method == http.MethodGet && r.URL.Path == "/1.0/instances/test-pending":
			w.Header().Set("ETag", "native-etag")
			config := platform.Row{"volatile.uuid": "login-uuid", "cloud-init.user-data": observedData}
			_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"name": "test-pending", "config": config, "expanded_config": config}})
		default:
			t.Errorf("unexpected login native request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	})
	db, a, job, r, _ := creationCleanupDB(t, handler)
	sealed, err := sealLogin(a, &LoginRequest{RootPassword: "valid root password"})
	if err != nil {
		t.Fatal(err)
	}
	r["login_config_ciphertext"] = sealed
	if err = db.Table("instances").Where("id=?", job.InstanceID).Update("login_config_ciphertext", sealed).Error; err != nil {
		t.Fatal(err)
	}
	body, err := workerCreateBody(t.Context(), a, r)
	if err != nil {
		t.Fatal(err)
	}
	observedData = platform.Text(object(body["config"])["cloud-init.user-data"])
	var cloud platform.Row
	if err := json.Unmarshal([]byte(strings.TrimPrefix(observedData, "#cloud-config\n")), &cloud); err != nil {
		t.Fatal(err)
	}
	passwordHash = platform.Text(object(rows(cloud["users"])[0])["hashed_passwd"])
	if !strings.HasPrefix(observedData, "#cloud-config\n") {
		t.Fatal("native create omitted decrypted cloud-config")
	}
	// Observation/writable metadata stays transient. In particular, preserving
	// user-data for a native PUT must never turn it into an instance/task snapshot.
	observed, _, err := workerObserve(t.Context(), a, r)
	if err != nil || object(workerWritable(observed)["config"])["cloud-init.user-data"] != observedData {
		t.Fatal("native writable configuration lost the guest's first-boot data")
	}
	if err = workerInitialize(t.Context(), a, job, r); err != nil {
		t.Fatal(err)
	}
	current, err := workerInstance(t.Context(), a, job)
	if err != nil || platform.Stamp(current["login_initialized_at"]).IsZero() {
		t.Fatal("login first-boot completion was not checkpointed")
	}
	if err = workerInitialize(t.Context(), a, job, current); err != nil || execs.Load() != 1 {
		t.Fatal("subsequent start repeated first-boot initialization")
	}
	// A new task (such as a later restart/start) also respects the instance-level
	// checkpoint, not just the original task's mutation journal.
	later := job
	later.ID++
	if err = workerInitialize(t.Context(), a, later, current); err != nil || execs.Load() != 1 {
		t.Fatal("new lifecycle task repeated credential initialization")
	}
	for _, table := range []string{"tasks", "task_steps", "usage_sessions"} {
		var records []platform.Row
		if err = db.Table(table).Find(&records).Error; err != nil {
			t.Fatal(err)
		}
		if strings.Contains(jsonText(records), "cloud-init.user-data") || strings.Contains(jsonText(records), passwordHash) {
			t.Fatalf("observed or writable user-data leaked into %s", table)
		}
	}
	if platform.Text(current["incus_config_snapshot"]) != "{}" {
		t.Fatal("transient native metadata was persisted as an ordinary snapshot")
	}
}

func TestMySQLLoginUnknownWaitDoesNotRedispatch(t *testing.T) {
	db, a, job, r, mutations := creationCleanupDB(t)
	r["login_config_ciphertext"] = []byte("opaque-not-needed-by-wait")
	step := platform.Row{"task_id": job.ID, "step_key": "login.cloud-ready", "attempt": 1, "status": "running", "parameters_redacted": "{}", "started_at": a.Time()}
	if err := db.Table("task_steps").Create(&step).Error; err != nil {
		t.Fatal(err)
	}
	if err := workerLoginInitialize(t.Context(), a, job, r); err == nil {
		t.Fatal("ambiguous wait submission was treated as safely replayable")
	}
	if mutations.Load() != 0 {
		t.Fatal("ambiguous initialization was redispatched")
	}
}

func TestMySQLLoginFailedCloudInitDoesNotMarkInitialized(t *testing.T) {
	for _, tc := range []struct {
		name, code, reason string
		diagnostic         int
	}{
		{"legacy-host-key-failure", "initialization_tool_missing", "ssh_host_key_generation_missing_tool", 58},
		{"network-module-failure", "initialization_cloud_init_failed", "cloud_init_errors", 65},
		{"password-not-applied", "initialization_password_not_ready", "root_password_not_ready", 55},
		{"unrelated-degraded-module", "initialization_cloud_init_degraded", "cloud_init_degraded", 56},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var execs atomic.Int64
			var probes atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
					var body platform.Row
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					if strings.Contains(jsonText(body["command"]), "statuses =") {
						probes.Add(1)
						command := rows(body["command"])
						want := []any{"/usr/bin/timeout", "--signal=KILL", "15", "/usr/bin/python3", "-c", CloudInitDiagnosticScript, "login"}
						if !reflect.DeepEqual(command, want) || platform.Bool(body["record-output"]) || platform.Bool(body["interactive"]) || len(object(body["environment"])) != 1 {
							t.Error("diagnostic was not fixed, output-disabled and credential-free")
						}
						_ = json.NewEncoder(w).Encode(platform.Row{"operation": "/1.0/operations/read-only-diagnostic"})
					} else {
						execs.Add(1)
						_ = json.NewEncoder(w).Encode(platform.Row{"operation": "/1.0/operations/failed-login-wait"})
					}
				case r.Method == http.MethodGet && r.URL.Path == "/1.0/operations/read-only-diagnostic/wait":
					if r.URL.Query().Get("project") == "" {
						t.Error("diagnostic operation lost project scope")
					}
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status_code": 200, "metadata": platform.Row{"return": tc.diagnostic}}})
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/wait"):
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status_code": 200, "metadata": platform.Row{"return": 1}}})
				case r.Method == http.MethodGet && r.URL.Path == "/1.0/instances/test-pending":
					_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"name": "test-pending", "config": platform.Row{"volatile.uuid": "login-uuid"}}})
				default:
					t.Errorf("unexpected native request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusInternalServerError)
				}
			})
			db, a, job, r, _ := creationCleanupDB(t, handler)
			r["login_config_ciphertext"] = []byte("wait-does-not-decrypt-credentials")
			if err := db.Table("instances").Where("id=?", job.InstanceID).Update("login_config_ciphertext", r["login_config_ciphertext"]).Error; err != nil {
				t.Fatal(err)
			}
			if err := workerInitialize(t.Context(), a, job, r); err == nil {
				t.Fatal("failed cloud-init was accepted as completed")
			} else {
				public := platform.PublicError(err, true)
				if public["code"] != tc.code || public["details"].(map[string]any)["reason"] != tc.reason {
					t.Fatalf("safe root cause was lost: %+v", public)
				}
			}
			if err := workerInitialize(t.Context(), a, job, r); err == nil || execs.Load() != 1 {
				t.Fatal("failed recorded cloud-init wait was blindly redispatched")
			}
			if probes.Load() != 2 {
				t.Fatal("recovery must inspect the original operation without replaying it; only the read-only diagnostic may be repeated")
			}
			current, err := workerInstance(t.Context(), a, job)
			if err != nil || !platform.Stamp(current["login_initialized_at"]).IsZero() {
				t.Fatal("failed cloud-init wrote successful initialization checkpoint")
			}
			step, err := workerStep(t.Context(), a, job, "login.cloud-ready")
			if err != nil || platform.Text(step["status"]) != "failed" {
				t.Fatal("failed exec was persisted as successful stage")
			}
		})
	}
}

func TestMySQLNetworkOnlyCreateSuppressesSSHAndWaitsOnce(t *testing.T) {
	var execs atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			execs.Add(1)
			var body platform.Row
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			want := []any{"/usr/bin/timeout", "--signal=KILL", "600", "/bin/sh", "-c", CloudInitReadyCommand, "cloud-init-ready", "network"}
			if !reflect.DeepEqual(rows(body["command"]), want) || platform.Bool(body["record-output"]) {
				t.Error("network-only initialization did not use strict bounded wait")
			}
			_ = json.NewEncoder(w).Encode(platform.Row{"operation": "/1.0/operations/network-wait"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/wait"):
			_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status_code": 200, "metadata": platform.Row{"return": 0}}})
		case r.Method == http.MethodGet && r.URL.Path == "/1.0/instances/test-pending":
			_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"name": "test-pending", "config": platform.Row{"volatile.uuid": "network-uuid"}}})
		default:
			t.Errorf("unexpected native request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	})
	db, a, job, r, _ := creationCleanupDB(t, handler)
	legacy, err := workerCreateBody(t.Context(), a, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := object(legacy["config"])["cloud-init.user-data"]; exists {
		t.Fatal("historical creation without frozen capability gained new user-data")
	}
	r["incus_config_snapshot"] = jsonText(platform.Row{"creation": platform.Row{"cloud_init_capable": true, "cloud_init": true, "login_mode": "none"}})
	if err := db.Table("instances").Where("id=?", job.InstanceID).Update("incus_config_snapshot", r["incus_config_snapshot"]).Error; err != nil {
		t.Fatal(err)
	}
	body, err := workerCreateBody(t.Context(), a, r)
	if err != nil {
		t.Fatal(err)
	}
	assertMinimalCloudConfig(t, platform.Text(object(body["config"])["cloud-init.user-data"]), false)
	if object(body["config"])["cloud-init.network-config"] != object(legacy["config"])["cloud-init.network-config"] {
		t.Fatal("base cloud-config changed static network addressing/routes/DNS")
	}
	if err := workerInitialize(t.Context(), a, job, r); err != nil {
		t.Fatal(err)
	}
	current, err := workerInstance(t.Context(), a, job)
	if err != nil || platform.Stamp(current["login_initialized_at"]).IsZero() {
		t.Fatal("network-only completion was not checkpointed")
	}
	if err := workerInitialize(t.Context(), a, job, current); err != nil || execs.Load() != 1 {
		t.Fatal("network-only initialization repeated after completion")
	}
}
