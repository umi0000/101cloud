package catalog

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

const imageJobTestFingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Agent pins require a non-CA server leaf, unlike httptest's default certificate.
func newImageTestAgentTLS(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

type imageJobFixture struct {
	t        *testing.T
	ctx      context.Context
	db       *gorm.DB
	a        *platform.App
	server   *httptest.Server
	nextNode uint64
}

func newImageJobFixture(t *testing.T, handler http.HandlerFunc) *imageJobFixture {
	t.Helper()
	ctx, db := openCatalogMySQL(t, "ci_image_jobs_")
	server := newImageTestAgentTLS(t, handler)
	a := platform.New(db, platform.Config{MasterKey: make([]byte, 32), NodeAllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	for _, statement := range []string{
		"INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'image-admin','unused-test-hash','admin','active')",
		"INSERT INTO policy_versions(id,kind,code,version,config,status,effective_at,created_by_user_id) VALUES(1,'metrics','image-test','v1',JSON_OBJECT(),'active','2000-01-01',1)",
		"INSERT INTO images(id,code,version,name,source_type,fingerprint,architecture,image_type,gpu_ready,os_name,os_release,properties,status,created_by_user_id) VALUES(1,'linux','test','Linux','local',UNHEX('" + imageJobTestFingerprint + "'),'x86_64','container',FALSE,'Linux','test',JSON_OBJECT(),'active',1)",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	return &imageJobFixture{t: t, ctx: ctx, db: db, a: a, server: server}
}

func (f *imageJobFixture) selection(source string, migrated bool) (platform.Job, uint64) {
	f.t.Helper()
	f.nextNode++
	node := f.nextNode
	server := f.server
	if node > 1 {
		server = newImageTestAgentTLS(f.t, f.server.Config.Handler)
	}
	token, err := f.a.Encrypt([]byte(strings.Repeat("A", 43)))
	if err != nil {
		f.t.Fatal(err)
	}
	pin := sha256.Sum256(server.Certificate().Raw)
	n := platform.Row{"id": node, "name": fmt.Sprintf("node-%d", node), "metrics_policy_id": 1, "agent_endpoint": server.URL, "token_ciphertext": token, "key_version": 1, "server_cert_sha256": pin[:], "default_storage_pool": "default", "default_project_name": "default", "allocatable_vcpus": 4, "allocatable_memory_bytes": 1, "allocatable_disk_bytes": 1, "resident_vcpu_budget": 4, "resident_memory_budget_bytes": 1, "resident_disk_budget_bytes": 1, "status": "active"}
	if err = f.db.Table("nodes").Create(&n).Error; err != nil {
		f.t.Fatal(err)
	}
	fp, _ := hex.DecodeString(imageJobTestFingerprint)
	row := platform.Row{"image_id": 1, "node_id": node, "project_name": fmt.Sprintf("project-%d", node), "architecture": "x86_64", "fingerprint": fp, "cloud_init": false, "gpu_ready": false, "variant": "default", "properties": "{}", "status": "pending"}
	if source != "" {
		row["source_id"] = source
	}
	if err = f.db.Table("image_nodes").Create(&row).Error; err != nil {
		f.t.Fatal(err)
	}
	id := platform.Uint(row["@id"])
	if id == 0 {
		id = platform.Uint(row["id"])
	}
	params := platform.Row{"image_node_id": fmt.Sprint(id)}
	if !migrated {
		params["config"] = imagePrepareIdentity(row, platform.Row{"image_type": "container"})
	}
	task, err := platform.Enqueue(f.db, "image.prepare", 1, node, 0, false, params)
	if err != nil {
		f.t.Fatal(err)
	}
	jobID := platform.Uint(task["task_id"])
	encoded, err := json.Marshal(params)
	if err != nil {
		f.t.Fatal(err)
	}
	j := platform.Job{ID: jobID, Type: "image.prepare", UserID: 1, NodeID: node, LeaseGeneration: 1, Parameters: encoded}
	if err = f.db.Table("tasks").Where("id=?", j.ID).Updates(platform.Row{"status": "running", "lease_owner": "image-test", "lease_generation": 1, "lease_until": f.a.Time().Add(time.Minute), "started_at": f.a.Time()}).Error; err != nil {
		f.t.Fatal(err)
	}
	if err = f.db.Table("image_nodes").Where("id=?", id).Update("task_id", j.ID).Error; err != nil {
		f.t.Fatal(err)
	}
	return j, id
}

func (f *imageJobFixture) state(id uint64, want string) platform.Row {
	f.t.Helper()
	row, err := platform.Get(f.db, "image_nodes", id, false)
	if err != nil {
		f.t.Fatal(err)
	}
	if platform.Text(row["status"]) != want {
		f.t.Fatalf("node image status=%s want=%s row=%v", row["status"], want, row)
	}
	return row
}

func imageJobNative(w http.ResponseWriter, metadata platform.Row) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(platform.Row{"type": "sync", "metadata": metadata})
}

func imageJobNativeImage() platform.Row {
	return platform.Row{"fingerprint": imageJobTestFingerprint, "architecture": "x86_64", "type": "container", "properties": platform.Row{"variant": "cloud"}}
}

func imageJobMissing(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(platform.Row{"type": "error", "error_code": 404})
}

func TestMySQLImagePrepareLocalNativeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, status                   string
		metadata                       platform.Row
		missing, readError, incomplete bool
	}{
		{name: "ready", status: "ready", metadata: imageJobNativeImage()},
		{name: "fingerprint mismatch", status: "failed", metadata: platform.Row{"fingerprint": strings.Repeat("b", 64), "architecture": "x86_64", "type": "container"}},
		{name: "architecture mismatch", status: "failed", metadata: platform.Row{"fingerprint": imageJobTestFingerprint, "architecture": "aarch64", "type": "container"}},
		{name: "type mismatch", status: "failed", metadata: platform.Row{"fingerprint": imageJobTestFingerprint, "architecture": "x86_64", "type": "virtual-machine"}},
		{name: "explicit missing", status: "failed", missing: true},
		{name: "read error", status: "unknown", readError: true},
		{name: "404 without native missing evidence", status: "unknown", incomplete: true},
		{name: "incomplete identity", status: "unknown", metadata: platform.Row{"fingerprint": imageJobTestFingerprint}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mutations atomic.Int64
			f := newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations.Add(1)
				}
				if r.URL.Path != "/1.0/images/"+imageJobTestFingerprint || r.URL.Query().Get("project") != "project-1" {
					t.Errorf("unexpected native request: %s %s", r.Method, r.URL)
				}
				if tc.missing {
					imageJobMissing(w)
					return
				}
				if tc.readError || tc.incomplete {
					w.Header().Set("Content-Type", "application/json")
					if tc.readError {
						w.WriteHeader(500)
					} else {
						w.WriteHeader(404)
					}
					_, _ = w.Write([]byte(`{"type":"error"}`))
					return
				}
				imageJobNative(w, tc.metadata)
			})
			j, id := f.selection("", false)
			err := imagePrepareJob(f.ctx, f.a, j)
			if (err == nil) != (tc.status == "ready") {
				t.Fatalf("unexpected job result: %v", err)
			}
			row := f.state(id, tc.status)
			if tc.status == "ready" && platform.Stamp(row["last_verified_at"]).IsZero() {
				t.Fatal("ready without verification time")
			}
			if mutations.Load() != 0 {
				t.Fatal("local verification mutated native state")
			}
		})
	}
}

func TestMySQLImagePrepareRemoteProgressAndDurableIdentity(t *testing.T) {
	var f *imageJobFixture
	var downloaded atomic.Bool
	var posts, polls atomic.Int64
	f = newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("project") != "project-1" {
			t.Errorf("project was not pinned: %s", r.URL)
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/1.0/images/"):
			if downloaded.Load() {
				imageJobNative(w, imageJobNativeImage())
			} else {
				imageJobMissing(w)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/1.0/images":
			posts.Add(1)
			var config, native platform.Row
			if err := f.db.Table("task_steps").Where("step_key=?", imagePrepareConfigKey).Take(&config).Error; err != nil {
				t.Error(err)
			}
			if err := f.db.Table("task_steps").Where("step_key=?", imagePrepareNativeKey).Take(&native).Error; err != nil {
				t.Error(err)
			}
			if platform.Text(native["status"]) != "running" || platform.Text(native["project_name"]) != "project-1" || platform.Text(native["operation_id"]) != "" {
				t.Errorf("POST without durable intent: %v", native)
			}
			if !strings.Contains(platform.Text(config["parameters_redacted"]), imageJobTestFingerprint) {
				t.Errorf("POST without immutable identity: %v", config)
			}
			var body struct {
				Public bool              `json:"public"`
				Source map[string]string `json:"source"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Public || body.Source["server"] != "https://images.linuxcontainers.org" || body.Source["fingerprint"] != imageJobTestFingerprint || body.Source["mode"] != "pull" || body.Source["protocol"] != "simplestreams" || body.Source["type"] != "image" {
				t.Errorf("incorrect pull: %+v", body)
			}
			_ = json.NewEncoder(w).Encode(platform.Row{"operation": "/1.0/operations/pull-1"})
		case r.URL.Path == "/1.0/operations/pull-1":
			var native platform.Row
			if err := f.db.Table("task_steps").Where("step_key=?", imagePrepareNativeKey).Take(&native).Error; err != nil {
				t.Error(err)
			}
			if platform.Text(native["operation_id"]) != "pull-1" || platform.Text(native["project_name"]) != "project-1" {
				t.Errorf("poll before durable operation/project: %v", native)
			}
			if polls.Add(1) == 1 {
				imageJobNative(w, platform.Row{"status_code": 103, "metadata": platform.Row{"download_progress": "30% (3MB/s)"}})
			} else {
				var task platform.Row
				if err := f.db.Table("tasks").Where("id=?", native["task_id"]).Take(&task).Error; err != nil {
					t.Error(err)
				}
				if !strings.Contains(platform.Text(task["result_redacted"]), "30%") {
					t.Errorf("native progress not persisted: %v", task)
				}
				downloaded.Store(true)
				imageJobNative(w, platform.Row{"status_code": 200, "metadata": platform.Row{"download_progress": "100%"}})
			}
		default:
			t.Errorf("unexpected native request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	j, id := f.selection("images", false)
	if err := imagePrepareJob(f.ctx, f.a, j); err != nil {
		t.Fatal(err)
	}
	f.state(id, "ready")
	if posts.Load() != 1 || polls.Load() != 2 {
		t.Fatalf("posts=%d polls=%d", posts.Load(), polls.Load())
	}
	// The task retains its requested cloud flag, not native shared properties.
	if platform.Bool(f.state(id, "ready")["cloud_init"]) {
		t.Fatal("explicit per-node cloud flag overwritten")
	}
}

func TestMySQLImagePrepareAmbiguousResponseNeverRedispatches(t *testing.T) {
	var available atomic.Bool
	var posts atomic.Int64
	f := newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			posts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error_code":503}`))
		case strings.HasPrefix(r.URL.Path, "/1.0/images/"):
			if available.Load() {
				imageJobNative(w, imageJobNativeImage())
			} else {
				imageJobMissing(w)
			}
		case r.URL.Path == "/1.0/operations":
			imageJobNative(w, platform.Row{})
		default:
			t.Errorf("unexpected %s", r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	j, id := f.selection("images", false)
	if err := imagePrepareJob(f.ctx, f.a, j); err == nil {
		t.Fatal("ambiguous POST succeeded")
	}
	f.state(id, "unknown")
	for range 2 {
		j.LeaseGeneration++
		if err := f.db.Table("tasks").Where("id=?", j.ID).Update("lease_generation", j.LeaseGeneration).Error; err != nil {
			t.Fatal(err)
		}
		if err := imagePrepareReconcileJob(f.ctx, f.a, j); err == nil {
			t.Fatal("missing ambiguous download resolved without evidence")
		}
		f.state(id, "unknown")
	}
	if posts.Load() != 1 {
		t.Fatalf("ambiguous download redispatched %d times", posts.Load())
	}
	available.Store(true)
	if err := imagePrepareReconcileJob(f.ctx, f.a, j); err != nil {
		t.Fatal(err)
	}
	f.state(id, "ready")
	if posts.Load() != 1 {
		t.Fatal("reconciliation resent the pull")
	}
}

func TestMySQLImagePrepareOperationSuccessRequiresNativeVerification(t *testing.T) {
	for _, tc := range []struct {
		name, status    string
		native          platform.Row
		operationStatus int
	}{
		{"missing after success", "unknown", nil, 200},
		{"mismatch after success", "failed", platform.Row{"fingerprint": imageJobTestFingerprint, "architecture": "aarch64", "type": "container"}, 200},
		{"known operation failure", "failed", nil, 400},
		{"unrecognized operation status", "unknown", nil, 999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posted atomic.Bool
			var posts atomic.Int64
			f := newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost:
					posts.Add(1)
					posted.Store(true)
					_ = json.NewEncoder(w).Encode(platform.Row{"operation": "/1.0/operations/verify-1"})
				case strings.HasPrefix(r.URL.Path, "/1.0/images/"):
					if posted.Load() && tc.native != nil {
						imageJobNative(w, tc.native)
					} else {
						imageJobMissing(w)
					}
				case r.URL.Path == "/1.0/operations/verify-1":
					imageJobNative(w, platform.Row{"status_code": tc.operationStatus})
				default:
					t.Errorf("unexpected %s", r.URL)
					http.Error(w, "unexpected", 500)
				}
			})
			j, id := f.selection("images", false)
			if err := imagePrepareJob(f.ctx, f.a, j); err == nil {
				t.Fatal("operation alone marked image ready")
			}
			f.state(id, tc.status)
			if err := imagePrepareReconcileJob(f.ctx, f.a, j); err == nil {
				t.Fatal("reconcile marked unavailable image ready")
			}
			if posts.Load() != 1 {
				t.Fatal("known operation was redispatched")
			}
		})
	}
}

func TestMySQLImagePreparePerNodePartialFailure(t *testing.T) {
	f := newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation %s", r.Method)
		}
		if r.URL.Query().Get("project") == "project-1" {
			imageJobNative(w, imageJobNativeImage())
		} else {
			imageJobMissing(w)
		}
	})
	first, firstID := f.selection("", false)
	second, secondID := f.selection("", false)
	if err := imagePrepareJob(f.ctx, f.a, first); err != nil {
		t.Fatal(err)
	}
	if err := imagePrepareJob(f.ctx, f.a, second); err == nil {
		t.Fatal("missing peer node image accepted")
	}
	f.state(firstID, "ready")
	f.state(secondID, "failed")
}

func TestMySQLImagePrepareLeaseAndSelectionFencing(t *testing.T) {
	for _, kind := range []string{"lease", "task", "identity"} {
		t.Run(kind, func(t *testing.T) {
			var reads atomic.Int64
			f := newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) { reads.Add(1); imageJobNative(w, imageJobNativeImage()) })
			j, id := f.selection("", false)
			var err error
			switch kind {
			case "lease":
				err = f.db.Table("tasks").Where("id=?", j.ID).Update("lease_generation", 2).Error
			case "task":
				err = f.db.Table("image_nodes").Where("id=?", id).Update("task_id", nil).Error
			case "identity":
				err = f.db.Table("image_nodes").Where("id=?", id).Update("architecture", "aarch64").Error
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = imagePrepareJob(f.ctx, f.a, j); err == nil {
				t.Fatal("stale task accepted")
			}
			f.state(id, "pending")
			if reads.Load() != 0 {
				t.Fatal("stale task reached native API")
			}
		})
	}
}

func TestMySQLImagePrepareMigratedCloudInitEvidence(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint("explicit=", explicit), func(t *testing.T) {
			f := newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) { imageJobNative(w, imageJobNativeImage()) })
			j, id := f.selection("", true)
			if explicit {
				if err := f.db.Table("images").Where("id=1").Update("properties", `{"cloud_init":false}`).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := imagePrepareJob(f.ctx, f.a, j); err != nil {
				t.Fatal(err)
			}
			if cloud := platform.Bool(f.state(id, "ready")["cloud_init"]); cloud == explicit {
				t.Fatalf("legacy cloud evidence=%v explicit=%v", cloud, explicit)
			}
			if err := imagePrepareReconcileJob(f.ctx, f.a, j); err != nil {
				t.Fatalf("derived capability invalidated immutable identity: %v", err)
			}
		})
	}
}

func TestMySQLImagePrepareReconcileWrapperUsesRootJournal(t *testing.T) {
	var available atomic.Bool
	var posts atomic.Int64
	f := newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			posts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error_code":503}`))
		case strings.HasPrefix(r.URL.Path, "/1.0/images/"):
			if available.Load() {
				imageJobNative(w, imageJobNativeImage())
			} else {
				imageJobMissing(w)
			}
		case r.URL.Path == "/1.0/operations":
			imageJobNative(w, platform.Row{})
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	root, id := f.selection("images", false)
	if err := imagePrepareJob(f.ctx, f.a, root); err == nil {
		t.Fatal("ambiguous dispatch resolved")
	}
	params := platform.Row{"source_task_id": root.ID}
	task, err := platform.Enqueue(f.db, "instance.reconcile", root.UserID, root.NodeID, 0, false, params)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := platform.Job{ID: platform.Uint(task["task_id"]), Type: "image.prepare", UserID: root.UserID, NodeID: root.NodeID, LeaseGeneration: 1, Parameters: encoded}
	if err = f.db.Table("tasks").Where("id=?", wrapper.ID).Updates(platform.Row{"status": "running", "lease_generation": 1, "lease_owner": "wrapper-test", "lease_until": f.a.Time().Add(time.Minute), "started_at": f.a.Time()}).Error; err != nil {
		t.Fatal(err)
	}
	if err = f.db.Table("tasks").Where("id=?", root.ID).Updates(platform.Row{"status": "unknown", "lease_owner": fmt.Sprintf("reconcile:%d", wrapper.ID)}).Error; err != nil {
		t.Fatal(err)
	}
	if err = imagePrepareReconcileJob(f.ctx, f.a, wrapper); err == nil {
		t.Fatal("wrapper redispatched or resolved absent image")
	}
	f.state(id, "unknown")
	available.Store(true)
	before, err := platform.Get(f.db, "tasks", root.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = imagePrepareReconcileJob(f.ctx, f.a, wrapper); err != nil {
		t.Fatal(err)
	}
	after, err := platform.Get(f.db, "tasks", root.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if platform.Uint(before["revision"]) != platform.Uint(after["revision"]) {
		t.Fatal("worker changed root revision used by generic reconciliation CAS")
	}
	f.state(id, "ready")
	var wrapperJournals int64
	if err = f.db.Table("task_steps").Where("task_id=?", wrapper.ID).Count(&wrapperJournals).Error; err != nil {
		t.Fatal(err)
	}
	if wrapperJournals != 0 || posts.Load() != 1 {
		t.Fatalf("wrapper journals=%d posts=%d", wrapperJournals, posts.Load())
	}
}

func TestMySQLImagePrepareCompletionIsFenced(t *testing.T) {
	for _, kind := range []string{"lease", "task"} {
		t.Run(kind, func(t *testing.T) {
			var f *imageJobFixture
			var j platform.Job
			var id uint64
			f = newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var err error
				if kind == "lease" {
					err = f.db.Table("tasks").Where("id=?", j.ID).Update("lease_generation", 2).Error
				} else {
					err = f.db.Table("image_nodes").Where("id=?", id).Update("task_id", nil).Error
				}
				if err != nil {
					t.Error(err)
				}
				imageJobNative(w, imageJobNativeImage())
			})
			j, id = f.selection("", false)
			if err := imagePrepareJob(f.ctx, f.a, j); err == nil {
				t.Fatal("superseded native observation published readiness")
			}
			f.state(id, "pending")
		})
	}
}

func TestMySQLImagePrepareJournalRejectsChangedIdentity(t *testing.T) {
	var reads atomic.Int64
	f := newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) { reads.Add(1); imageJobMissing(w) })
	j, id := f.selection("", false)
	if err := imagePrepareJob(f.ctx, f.a, j); err == nil {
		t.Fatal("missing local accepted")
	}
	if err := f.db.Table("image_nodes").Where("id=?", id).Update("architecture", "aarch64").Error; err != nil {
		t.Fatal(err)
	}
	var input imagePrepareInput
	if err := json.Unmarshal(j.Parameters, &input); err != nil {
		t.Fatal(err)
	}
	input.Config.Architecture = "aarch64"
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	j.Parameters = encoded
	if err = imagePrepareReconcileJob(f.ctx, f.a, j); err == nil {
		t.Fatal("immutable journal was rewritten")
	}
	if reads.Load() != 1 {
		t.Fatal("changed identity reached native API")
	}
}

func TestMySQLImagePrepareAmbiguousResponseAdoptsExactOperation(t *testing.T) {
	var available atomic.Bool
	var posts atomic.Int64
	var f *imageJobFixture
	f = newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			posts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error_code":503}`))
		case strings.HasPrefix(r.URL.Path, "/1.0/images/"):
			if available.Load() {
				imageJobNative(w, imageJobNativeImage())
			} else {
				imageJobMissing(w)
			}
		case r.URL.Path == "/1.0/operations":
			imageJobNative(w, platform.Row{"running": []platform.Row{
				{"id": "unrelated", "project": "project-1", "resources": platform.Row{"images": []string{"/1.0/images/" + strings.Repeat("b", 64)}}},
				{"id": "recovered-pull", "project": "project-1", "resources": platform.Row{"images": []string{"/1.0/images/" + imageJobTestFingerprint + "?project=project-1"}}},
			}})
		case r.URL.Path == "/1.0/operations/recovered-pull":
			var native platform.Row
			if err := f.db.Table("task_steps").Where("step_key=?", imagePrepareNativeKey).Take(&native).Error; err != nil {
				t.Error(err)
			}
			if platform.Text(native["operation_id"]) != "recovered-pull" || platform.Text(native["project_name"]) != "project-1" {
				t.Errorf("recovered operation polled before durable adoption: %v", native)
			}
			available.Store(true)
			imageJobNative(w, platform.Row{"status_code": 200})
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	j, id := f.selection("images", false)
	if err := imagePrepareJob(f.ctx, f.a, j); err == nil {
		t.Fatal("ambiguous response resolved")
	}
	f.state(id, "unknown")
	if err := imagePrepareReconcileJob(f.ctx, f.a, j); err != nil {
		t.Fatal(err)
	}
	f.state(id, "ready")
	if posts.Load() != 1 {
		t.Fatal("recovered operation caused redispatch")
	}
}
