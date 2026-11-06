package instances

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	cryptsha512 "github.com/sergeymakinen/go-crypt/sha512"
)

func customLoginBody(f *planConfigurationFixture) platform.Row {
	return platform.Row{
		"owner_user_id": "2", "name": "custom-login-instance",
		"approved_config": platform.Row{
			"instance_type": "container", "image_id": "1", "node_id": "1", "network_option_id": "1",
			"resources":       Resources{VCPUs: 2, MemoryBytes: "2147483648", DiskBytes: "1073741824"},
			"gpu":             platform.Row{"count": 0, "model": nil, "access_mode": "shared", "max_hourly_pt": "0.000000"},
			"init_parameters": platform.Row{}, "allowed_user_actions": []string{"stop", "console"},
			"gpu_user_permissions": map[string]bool{"idle": false},
		},
		"policy_versions": f.createBody("1", "0")["policy_versions"],
	}
}

func TestMySQLCustomLoginHTTPAndSharedFirstBoot(t *testing.T) {
	for _, tc := range []struct{ mode, phase string }{
		{"password", ""}, {"ssh_key", ""}, {"none", ""},
		{"password", "cloud_init"}, {"ssh_key", "post_start"},
	} {
		t.Run(tc.mode+"-"+tc.phase, func(t *testing.T) {
			f := newPlanConfigurationFixture(t)
			password, key := " custom root 密码 ", testSSHPublicKey(t)
			body := customLoginBody(f)
			if tc.mode == "password" {
				body["login"] = LoginRequest{RootPassword: password}
			} else if tc.mode == "ssh_key" {
				body["login"] = LoginRequest{SSHPublicKeys: []string{key + " administrator comment", key}}
			}
			if tc.phase != "" {
				f.exec(`INSERT INTO init_template_versions(id,code,name,version,phase,script_content,script_sha256,parameter_schema,timeout_seconds,status,created_by_user_id) VALUES(1,'custom-login','Custom login','1',?,'true',UNHEX(SHA2('true',256)),JSON_OBJECT('type','object'),60,'active',1)`, tc.phase)
				object(body["approved_config"])["init_template_version_id"] = "1"
			}
			raw := jsonText(body)
			call := func(payload string, status int) platform.Row {
				t.Helper()
				req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/custom-instances", strings.NewReader(payload))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Idempotency-Key", "custom-login-replay")
				w := httptest.NewRecorder()
				f.router.ServeHTTP(w, req)
				if w.Code != status {
					t.Fatalf("custom create status=%d want=%d", w.Code, status)
				}
				if strings.Contains(w.Body.String(), password) || strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), "$6$") {
					t.Fatal("custom create response leaked credentials")
				}
				var envelope platform.Row
				if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				return object(envelope["data"])
			}
			created := call(raw, http.StatusAccepted)
			id := platform.Uint(created["instance_id"])
			r := f.instance(id)
			sealed := []byte(platform.Text(r["login_config_ciphertext"]))
			if (len(sealed) != 0) != (tc.mode != "none") || bytes.Contains(sealed, []byte(password)) || bytes.Contains(sealed, []byte(key)) {
				t.Fatal("custom credentials not stored exclusively as ciphertext")
			}
			if platform.Text(r["billing_mode"]) != "admin_exempt" || platform.Text(r["product_type"]) != "custom" || platform.Uint(r["instance_plan_id"]) != 0 || platform.Uint(r["user_id"]) != 2 {
				t.Fatal("login changed custom admission identity or billing exemption")
			}
			if !reflect.DeepEqual(rows(r["allowed_user_actions"]), rows(`["stop","console"]`)) || !reflect.DeepEqual(object(r["gpu_user_permissions"]), object(`{"idle":false}`)) {
				t.Fatal("login changed custom permission grants")
			}
			creation := object(object(r["incus_config_snapshot"])["creation"])
			if creation["login_mode"] != tc.mode || !platform.Bool(creation["cloud_init_capable"]) {
				t.Fatal("custom admission did not freeze exact image/login capability")
			}
			replay := call(raw, http.StatusAccepted)
			if platform.Uint(replay["instance_id"]) != id || platform.Uint(replay["task_id"]) != platform.Uint(created["task_id"]) || !bytes.Equal(sealed, []byte(platform.Text(f.instance(id)["login_config_ciphertext"]))) || f.count("instances") != 1 || f.count("tasks") != 1 || f.reads.Load() != 1 {
				t.Fatal("custom replay duplicated admission or changed credentials")
			}
			body["login"] = LoginRequest{RootPassword: "changed root password"}
			call(jsonText(body), http.StatusConflict)

			nativeBody, err := workerCreateBody(t.Context(), f.a, r)
			if err != nil {
				t.Fatal(err)
			}
			cfg := object(nativeBody["config"])
			data := platform.Text(cfg["cloud-init.user-data"])
			if !strings.HasPrefix(data, "#cloud-config\n") {
				t.Fatal("custom worker omitted cloud-config")
			}
			var cloud platform.Row
			if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "#cloud-config\n")), &cloud); err != nil {
				t.Fatal(err)
			}
			assertMinimalCloudConfig(t, data, tc.mode != "none")
			if tc.mode == "password" {
				root := object(rows(cloud["users"])[0])
				if cryptsha512.Check(platform.Text(root["hashed_passwd"]), password) != nil || root["lock_passwd"] != false {
					t.Fatal("custom password not applied with original whitespace")
				}
			} else if tc.mode == "ssh_key" {
				root := object(rows(cloud["users"])[0])
				if root["hashed_passwd"] != "*" || root["lock_passwd"] != false || !reflect.DeepEqual(rows(root["ssh_authorized_keys"]), []any{key}) {
					t.Fatal("custom public keys were not normalized or password disabled")
				}
			}
			eth := object(object(object(cfg["cloud-init.network-config"])["ethernets"])["eth0"])
			if eth["dhcp4"] != false || !reflect.DeepEqual(rows(eth["addresses"]), []any{"192.0.2.10/24"}) {
				t.Fatal("custom login changed static network user-data")
			}

			passwordHash := ""
			if tc.mode == "password" {
				passwordHash = platform.Text(object(rows(cloud["users"])[0])["hashed_passwd"])
			}
			// Exercise the same fenced prepare/readiness functions used by member create,
			// with a real admitted row and only an in-process pinned TLS Agent fixture.
			job := platform.Job{ID: platform.Uint(created["task_id"]), Type: "instance.create", UserID: 2, NodeID: 1, InstanceID: id, LeaseGeneration: 1}
			f.exec(`UPDATE tasks SET status='running',lease_owner='custom-login-test',lease_generation=1,lease_until=? WHERE id=?`, f.a.Time().Add(5*time.Minute), job.ID)
			f.exec(`UPDATE instances SET incus_uuid='custom-login-uuid' WHERE id=?`, id)
			r = f.instance(id)
			prepares, execs := 0, []string{}
			f.native = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				path := "/1.0/instances/" + platform.Text(r["incus_name"])
				var response platform.Row
				switch {
				case req.Method == http.MethodGet && req.URL.Path == path:
					config := platform.Row{"volatile.uuid": "custom-login-uuid", "user.101cloud.instance_id": strconv.FormatUint(id, 10), "volatile.base_image": workerFingerprint(r["image_fingerprint"])}
					response = platform.Row{"metadata": platform.Row{"name": r["incus_name"], "type": "container", "config": config, "expanded_config": config}}
				case req.Method == http.MethodGet && req.URL.Path == path+"/state":
					response = platform.Row{"metadata": platform.Row{"status": "Stopped"}}
				case req.Method == http.MethodPost && req.URL.Path == "/agent/v1/instances/cloud-init/prepare":
					var input platform.Row
					if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if len(input) != 5 || input["expected_instance_id"] != strconv.FormatUint(id, 10) || input["expected_uuid"] != "custom-login-uuid" || input["project"] != "default" || input["expected_image_fingerprint"] != workerFingerprint(r["image_fingerprint"]) {
						t.Error("custom prepare identity not fenced")
					}
					prepares++
					response = platform.Row{"checked": true, "disabled_marker_removed": true}
				case req.Method == http.MethodPost && req.URL.Path == path+"/exec":
					var input platform.Row
					if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					command := rows(input["command"])
					if platform.Bool(input["record-output"]) || platform.Bool(input["interactive"]) || strings.Contains(jsonText(input), password) || strings.Contains(jsonText(input), key) || (passwordHash != "" && strings.Contains(jsonText(input), passwordHash)) {
						t.Error("readiness/template leaked credential configuration")
					}
					if len(execs) == 0 {
						mode := "login"
						if tc.mode == "none" {
							mode = "network"
						}
						want := []any{"/usr/bin/timeout", "--signal=KILL", "600", "/bin/sh", "-c", CloudInitReadyCommand, "cloud-init-ready", mode}
						if !reflect.DeepEqual(command, want) {
							t.Error("custom readiness bypassed shared credential checks")
						}
					} else if len(command) != 6 || (command[5] != "true" && command[5] != "/usr/bin/cloud-init status --wait >/dev/null 2>&1") {
						t.Error("custom template no longer executes original script/phase")
					}
					execs = append(execs, jsonText(command))
					response = platform.Row{"operation": "/1.0/operations/custom-init-" + strconv.Itoa(len(execs))}
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/wait"):
					response = platform.Row{"metadata": platform.Row{"status_code": 200, "metadata": platform.Row{"return": 0}}}
				default:
					t.Errorf("unexpected custom worker request: %s %s", req.Method, req.URL.Path)
					http.Error(w, "unexpected request", 500)
					return
				}
				_ = json.NewEncoder(w).Encode(response)
			})
			if err := f.a.Step(t.Context(), job, "create.native", "", "succeeded"); err != nil {
				t.Fatal(err)
			}
			if err := workerPrepareCloudInit(t.Context(), f.a, job, r); err != nil {
				t.Fatal(err)
			}
			if err := workerPrepareCloudInit(t.Context(), f.a, job, r); err != nil {
				t.Fatal(err)
			}
			if err := workerInitialize(t.Context(), f.a, job, r); err != nil {
				t.Fatal(err)
			}
			current := f.instance(id)
			if platform.Stamp(current["login_initialized_at"]).IsZero() {
				t.Fatal("custom readiness missing durable completion")
			}
			if err := workerInitialize(t.Context(), f.a, job, current); err != nil {
				t.Fatal(err)
			}
			wantExecs := 1
			if tc.phase == "post_start" {
				wantExecs = 2
			}
			if tc.phase == "cloud_init" {
				wantExecs = 3
			}
			if prepares != 1 || len(execs) != wantExecs {
				t.Fatal("custom first boot repeated or skipped preparation/readiness/template")
			}
			view := f.call(true, http.MethodGet, "/instances/"+strconv.FormatUint(id, 10), platform.Row{}, 0, http.StatusOK, "")
			for label, value := range map[string]any{"instance DTO": view, "snapshot": current["incus_config_snapshot"], "parameters": current["init_parameters"]} {
				encoded := jsonText(value)
				if strings.Contains(encoded, password) || strings.Contains(encoded, key) || strings.Contains(encoded, "$6$") || strings.Contains(encoded, "cloud-init.user-data") || strings.Contains(encoded, "login_config_ciphertext") {
					t.Fatalf("credentials leaked into %s", label)
				}
			}
			for _, table := range []string{"tasks", "task_steps", "audit_logs", "backend_requests", "usage_sessions", "instance_init_runs"} {
				var records []platform.Row
				if err := f.db.Table(table).Find(&records).Error; err != nil {
					t.Fatal(err)
				}
				encoded := jsonText(records)
				if strings.Contains(encoded, password) || strings.Contains(encoded, key) || strings.Contains(encoded, "$6$") || strings.Contains(encoded, "cloud-init.user-data") {
					t.Fatalf("custom credentials leaked into %s", table)
				}
			}
		})
	}
}

func TestMySQLCustomLoginRejectsInvalidCredentialsAndImport(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	for _, tc := range []struct {
		login any
		code  string
	}{
		{LoginRequest{}, "invalid_request"},
		{LoginRequest{RootPassword: "short"}, "invalid_request"},
		{LoginRequest{RootPassword: "valid password", SSHPublicKeys: []string{testSSHPublicKey(t)}}, "invalid_request"},
		{LoginRequest{SSHPublicKeys: []string{"invalid-private-input"}}, "invalid_request"},
		{platform.Row{"root_password": "valid password", "confirm_password": "private-input"}, "validation_failed"},
	} {
		body := customLoginBody(f)
		body["login"] = tc.login
		f.call(true, http.MethodPost, "/custom-instances", body, 0, http.StatusUnprocessableEntity, tc.code)
	}
	for _, location := range []string{"approved_config", "init_parameters"} {
		body := customLoginBody(f)
		config := object(body["approved_config"])
		if location == "approved_config" {
			config["login"] = LoginRequest{RootPassword: "valid password"}
		} else {
			object(config["init_parameters"])["login"] = LoginRequest{RootPassword: "valid password"}
		}
		f.call(true, http.MethodPost, "/custom-instances", body, 0, http.StatusUnprocessableEntity, "")
	}
	body := customLoginBody(f)
	delete(body, "name")
	body["incus_uuid"] = "11111111-1111-1111-1111-111111111111"
	body["login"] = LoginRequest{RootPassword: "valid password"}
	f.call(true, http.MethodPost, "/nodes/1/instance-imports", body, 0, http.StatusUnprocessableEntity, "validation_failed")
	for _, table := range []string{"instances", "tasks", "backend_requests", "usage_sessions", "credit_reservations"} {
		if f.count(table) != 0 {
			t.Fatalf("rejected custom/import login persisted %s", table)
		}
	}
	if f.reads.Load() != 0 {
		t.Fatal("rejected custom/import login reached native dispatch")
	}
}

func TestMySQLCustomLoginRequiresExactProjectImageCapability(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	// Global metadata and a ready cloud-init image in another project must not
	// substitute for the selected network binding's exact node/project image.
	f.exec(`UPDATE images SET properties=JSON_OBJECT('cloud_init',true) WHERE id=1`)
	f.exec(`UPDATE image_nodes SET cloud_init=FALSE WHERE image_id=1 AND node_id=1 AND project_name='default'`)
	f.exec(`INSERT INTO image_nodes(image_id,node_id,project_name,architecture,fingerprint,cloud_init,gpu_ready,properties,status,last_verified_at) VALUES(1,1,'other-project','x86_64',UNHEX(SHA2('plan-image',256)),TRUE,TRUE,JSON_OBJECT(),'ready',NOW(6))`)
	body := customLoginBody(f)
	body["login"] = LoginRequest{RootPassword: "valid password"}
	f.call(true, http.MethodPost, "/custom-instances", body, 0, http.StatusConflict, "image_login_unsupported")
	for _, table := range []string{"instances", "tasks", "backend_requests", "usage_sessions", "credit_reservations"} {
		if f.count(table) != 0 {
			t.Fatalf("unsupported custom login persisted %s", table)
		}
	}
	if f.reads.Load() != 0 {
		t.Fatal("unsupported custom login reached native dispatch")
	}
	delete(body, "login")
	created := f.call(true, http.MethodPost, "/custom-instances", body, 0, http.StatusAccepted, "")
	r := f.instance(platform.Uint(created["instance_id"]))
	if platform.Text(r["login_config_ciphertext"]) != "" || platform.Bool(object(object(r["incus_config_snapshot"])["creation"])["cloud_init_capable"]) {
		t.Fatal("network-only legacy custom create gained credentials or capability")
	}
}
