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
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			execs.Add(1)
			var body platform.Row
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid wait request")
			}
			want := []any{"/usr/bin/timeout", "--signal=KILL", "600", "/bin/sh", "-c", "/usr/bin/cloud-init status --wait >/dev/null 2>&1 && test -f /var/lib/cloud/instance/boot-finished && test -f /etc/101cloud/ssh-login-mode"}
			if !reflect.DeepEqual(rows(body["command"]), want) || platform.Bool(body["record-output"]) || platform.Bool(body["interactive"]) {
				t.Error("login did not execute a bounded output-disabled cloud-init wait")
			}
			if strings.Contains(jsonText(body), "user-data") || strings.Contains(jsonText(body), "$6$") {
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
	sealed, err := sealLogin(a, &LoginRequest{SSHPublicKeys: []string{loginTestKey(t)}})
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
		if strings.Contains(jsonText(records), "cloud-init.user-data") || strings.Contains(jsonText(records), "ssh-ed25519 ") || strings.Contains(jsonText(records), "$6$") {
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
	var execs atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			execs.Add(1)
			_ = json.NewEncoder(w).Encode(platform.Row{"operation": "/1.0/operations/failed-login-wait"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/wait"):
			_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status_code": 200, "metadata": platform.Row{"return": 1}}})
		case r.Method == http.MethodGet && r.URL.Path == "/1.0/instances/test-pending":
			_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"name": "test-pending", "config": platform.Row{"volatile.uuid": "login-uuid"}}})
		default:
			t.Errorf("unexpected native request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	})
	_, a, job, r, _ := creationCleanupDB(t, handler)
	r["login_config_ciphertext"] = []byte("wait-does-not-decrypt-credentials")
	if err := workerInitialize(t.Context(), a, job, r); err == nil {
		t.Fatal("failed cloud-init was accepted as completed")
	}
	if err := workerInitialize(t.Context(), a, job, r); err == nil || execs.Load() != 1 {
		t.Fatal("failed recorded cloud-init wait was blindly redispatched")
	}
	current, err := workerInstance(t.Context(), a, job)
	if err != nil || !platform.Stamp(current["login_initialized_at"]).IsZero() {
		t.Fatal("failed cloud-init wrote successful initialization checkpoint")
	}
}
