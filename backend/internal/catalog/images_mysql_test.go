package catalog

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

// Uses a real migrated MySQL schema and the same certificate-pinned Agent path
// as production. Catalog discovery is served only by the selected node Agent.
func TestMySQLImageRegistrationAndNodeAvailability(t *testing.T) {
	ctx, db := openCatalogMySQL(t, "ci_image_api_")
	fp, armFP := strings.Repeat("a", 64), strings.Repeat("b", 64)
	var discoveryCalls atomic.Int32
	server := newImageTestAgentTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("A", 43) {
			t.Error("missing pinned Agent authorization")
			w.WriteHeader(401)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("API dispatched native mutation %s", r.Method)
			w.WriteHeader(405)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/agent/v1/image-sources/images/images") {
			t.Errorf("unexpected Agent target %s", r.URL)
			w.WriteHeader(404)
			return
		}
		discoveryCalls.Add(1)
		artifact := platform.Row{"fingerprint": fp, "os_name": "alpine", "os_release": "3.22", "architecture": "x86_64", "instance_type": "container", "cloud_init": false, "variant": "default", "description": "Alpine", "aliases": []string{"alpine/3.22"}, "size_bytes": "123456"}
		if strings.HasSuffix(r.URL.Path, armFP) {
			artifact["fingerprint"], artifact["architecture"], artifact["variant"], artifact["cloud_init"] = armFP, "aarch64", "cloud", true
		}
		if strings.HasSuffix(r.URL.Path, "/images") {
			if r.URL.Query().Get("architecture") != "x86_64" {
				t.Error("remote filters were not forwarded")
			}
			json.NewEncoder(w).Encode(platform.Row{"items": []platform.Row{artifact}})
		} else {
			json.NewEncoder(w).Encode(artifact)
		}
	}))
	a := platform.New(db, platform.Config{MasterKey: make([]byte, 32), NodeAllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	token, e := a.Encrypt([]byte(strings.Repeat("A", 43)))
	if e != nil {
		t.Fatal(e)
	}
	pin := sha256.Sum256(server.Certificate().Raw)
	if e = db.Exec("INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'image-admin','test-unused','admin','active')").Error; e != nil {
		t.Fatal(e)
	}
	if e = db.Exec("INSERT INTO policy_versions(id,kind,code,version,config,status,effective_at,created_by_user_id) VALUES(1,'metrics','image-test','v1',JSON_OBJECT(),'active','2000-01-01',1)").Error; e != nil {
		t.Fatal(e)
	}
	for id := 1; id <= 2; id++ {
		endpoint := server.URL
		if id == 2 {
			endpoint += "/"
		}
		e = db.Exec("INSERT INTO nodes(id,name,agent_endpoint,token_ciphertext,key_version,server_cert_sha256,default_storage_pool,default_project_name,allocatable_vcpus,allocatable_memory_bytes,allocatable_disk_bytes,resident_vcpu_budget,resident_memory_budget_bytes,resident_disk_budget_bytes,status,metrics_policy_id) VALUES(?,?,?,?,1,?,'default','default',4,1,1,4,1,1,'active',1)", id, fmt.Sprintf("image-node-%d", id), endpoint, token, pin[:]).Error
		if e != nil {
			t.Fatal(e)
		}
	}
	r := gin.New()
	r.Use(platform.RequestMiddleware())
	user := r.Group("/api/v1", func(c *gin.Context) { c.Set("user", platform.User{ID: 1, Role: "admin"}); c.Next() })
	admin := user.Group("/admin", platform.Admin(), a.Mutations())
	registerProducts(a, user, admin)
	registerImages(a, admin)
	var serial int
	call := func(method, path, body, etag string) *httptest.ResponseRecorder {
		serial++
		req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("image-api-%d", serial))
		req.Header.Set("If-Match", etag)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	data := func(w *httptest.ResponseRecorder) platform.Row {
		t.Helper()
		var envelope struct {
			Data platform.Row `json:"data"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &envelope); e != nil {
			t.Fatal(e)
		}
		return envelope.Data
	}
	w := call("GET", "/admin/nodes/1/remote-images?source_id=images&architecture=x86_64", "", "")
	if w.Code != 200 || discoveryCalls.Load() != 1 {
		t.Fatalf("discovery: %d %s", w.Code, w.Body.String())
	}
	if w = call("GET", "/admin/nodes/1/remote-images?source_id=https://untrusted.invalid", "", ""); w.Code != 422 {
		t.Fatalf("arbitrary source accepted: %s", w.Body.String())
	}
	nodes := []ImageNodeInput{{NodeID: "1", ProjectName: "default", Architecture: "x86_64", Fingerprint: fp, SourceID: "images", Variant: "default"}, {NodeID: "2", ProjectName: "tenant-project", Architecture: "aarch64", Fingerprint: armFP, SourceID: "images", Variant: "cloud", CloudInit: true, GPUReady: true}}
	payload := imageInput{InstanceType: "container", SourceType: "remote", Properties: map[string]any{"os_name": "alpine", "os_release": "3.22", "gpu_ready": false}, Nodes: nodes}
	wrong := payload
	wrong.Nodes = append([]ImageNodeInput(nil), nodes...)
	wrong.Nodes[1].CloudInit = false
	badJSON, _ := json.Marshal(wrong)
	if w = call("POST", "/admin/images", string(badJSON), ""); w.Code != 422 {
		t.Fatalf("unverified remote capability accepted: %d %s", w.Code, w.Body.String())
	}
	var rejectedRows int64
	if e := db.Table("images").Count(&rejectedRows).Error; e != nil {
		t.Fatal(e)
	}
	if rejectedRows != 0 {
		t.Fatal("partial multi-node registration persisted before metadata verification")
	}
	encoded, _ := json.Marshal(payload)
	w = call("POST", "/admin/images", string(encoded), "")
	if w.Code != 201 {
		t.Fatalf("multi-node registration: %d %s", w.Code, w.Body.String())
	}
	created := data(w)
	id := platform.Text(created["id"])
	configs, ok := created["nodes"].([]any)
	if !ok || len(configs) != 2 {
		t.Fatalf("missing node task DTOs: %+v", created)
	}
	for _, raw := range configs {
		n := raw.(map[string]any)
		if n["status"] != "pending" || platform.Text(n["task_id"]) == "" {
			t.Fatalf("registration claimed readiness or lost task: %+v", n)
		}
	}
	w = call("GET", "/catalog/images?node_id=1", "", "")
	if w.Code != 200 || len(data(w)["items"].([]any)) != 0 {
		t.Fatalf("pending image visible: %s", w.Body.String())
	}
	if e := db.Table("image_nodes").Where("image_id=? AND node_id=2", id).Updates(platform.Row{"status": "ready", "last_verified_at": a.Time()}).Error; e != nil {
		t.Fatal(e)
	}
	w = call("GET", "/catalog/images?node_id=2&project_name=tenant-project", "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	items := data(w)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("ready project image missing: %s", w.Body.String())
	}
	effective := items[0].(map[string]any)
	if effective["fingerprint"] != armFP || effective["architecture"] != "aarch64" || effective["gpu_ready"] != true || effective["cloud_init"] != true {
		t.Fatalf("global metadata leaked into effective DTO: %+v", effective)
	}
	w = call("GET", "/catalog/images?node_id=2", "", "")
	if len(data(w)["items"].([]any)) != 0 {
		t.Fatalf("wrong project image visible: %s", w.Body.String())
	}
	w = call("GET", "/admin/images/"+id, "", "")
	etag := w.Header().Get("ETag")
	manage, _ := json.Marshal(platform.Row{"nodes": nodes[:1]})
	if w = call("POST", "/admin/images/"+id+"/nodes", string(manage), ""); w.Code != 428 {
		t.Fatalf("missing If-Match accepted: %d %s", w.Code, w.Body.String())
	}
	if w = call("POST", "/admin/images/"+id+"/nodes", string(manage), etag); w.Code != 409 {
		t.Fatalf("active download requeued: %d %s", w.Code, w.Body.String())
	}
	if e := db.Exec("UPDATE image_nodes SET status='unknown' WHERE image_id=? AND node_id=1", id).Error; e != nil {
		t.Fatal(e)
	}
	if w = call("POST", "/admin/images/"+id+"/nodes", string(manage), etag); w.Code != 409 {
		t.Fatalf("ambiguous task redispatched: %d %s", w.Code, w.Body.String())
	}
	if e := db.Exec("UPDATE tasks SET status='failed',finished_at=? WHERE id=(SELECT task_id FROM image_nodes WHERE image_id=? AND node_id=1)", a.Time(), id).Error; e != nil {
		t.Fatal(e)
	}
	if e := db.Exec("UPDATE image_nodes SET status='failed' WHERE image_id=? AND node_id=1", id).Error; e != nil {
		t.Fatal(e)
	}
	if w = call("POST", "/admin/images/"+id+"/nodes", string(manage), etag); w.Code != 202 {
		t.Fatalf("known failed retry: %d %s", w.Code, w.Body.String())
	}
	if len(data(w)["tasks"].([]any)) != 1 {
		t.Fatal("manage response lost per-node task")
	}
	// A resolver result is a clone, and unavailable projects never inherit base identity.
	base, e := platform.Get(db, "images", platform.Uint(id), false)
	if e != nil {
		t.Fatal(e)
	}
	resolved, e := ResolveNodeImage(ctx, db, base, 2, "tenant-project")
	if e != nil {
		t.Fatal(e)
	}
	if imageFingerprint(base["fingerprint"]) != fp || imageFingerprint(resolved["fingerprint"]) != armFP {
		t.Fatal("resolver mutated global identity")
	}
	if _, e = ResolveNodeImage(ctx, db, base, 2, "default"); e == nil {
		t.Fatal("resolver fell back to global fingerprint")
	}
	// Payload shape is a clean cutover: historical top-level fingerprint is rejected.
	w = call("POST", "/admin/images", `{"instance_type":"container","architecture":"x86_64","fingerprint":"`+fp+`","properties":{"os_name":"alpine","os_release":"3.22","gpu_ready":false}}`, "")
	if w.Code != 422 {
		t.Fatalf("legacy payload accepted: %d %s", w.Code, w.Body.String())
	}
	// Migration retains legacy image properties on each node; they must remain
	// round-trippable through local management without triggering remote discovery.
	beforeLocal := discoveryCalls.Load()
	localNodes := []ImageNodeInput{{NodeID: "1", ProjectName: "default", Architecture: "x86_64", Fingerprint: fp, CloudInit: true, Properties: map[string]any{"os_name": "alpine", "os_release": "3.22", "gpu_ready": false, "cloud_init": true}}}
	localJSON, _ := json.Marshal(imageInput{Code: "legacy-local", SourceType: "local", InstanceType: "container", Properties: payload.Properties, Nodes: localNodes})
	w = call("POST", "/admin/images", string(localJSON), "")
	if w.Code != 201 {
		t.Fatalf("legacy local properties rejected: %d %s", w.Code, w.Body.String())
	}
	localID, localETag := platform.Text(data(w)["id"]), w.Header().Get("ETag")
	var localNode platform.Row
	if e := db.Table("image_nodes").Where("image_id=?", localID).Take(&localNode).Error; e != nil {
		t.Fatal(e)
	}
	if e := db.Table("tasks").Where("id=?", localNode["task_id"]).Updates(platform.Row{"status": "succeeded", "finished_at": a.Time()}).Error; e != nil {
		t.Fatal(e)
	}
	if e := db.Table("image_nodes").Where("id=?", localNode["id"]).Updates(platform.Row{"status": "ready", "last_verified_at": a.Time()}).Error; e != nil {
		t.Fatal(e)
	}
	localManage, _ := json.Marshal(platform.Row{"nodes": localNodes})
	w = call("POST", "/admin/images/"+localID+"/nodes", string(localManage), localETag)
	if w.Code != 202 {
		t.Fatalf("local management lost migrated properties: %d %s", w.Code, w.Body.String())
	}
	if discoveryCalls.Load() != beforeLocal {
		t.Fatal("local preparation queried a remote source")
	}
}
