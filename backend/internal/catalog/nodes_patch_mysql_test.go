package catalog

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

func TestMySQLNodePatchPreservesMetricsPolicy(t *testing.T) {
	ctx, db := openCatalogMySQL(t, "ci_node_patch_")
	for _, statement := range []string{
		"INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'node-admin','unused','admin','active')",
		"INSERT INTO policy_versions(id,kind,code,version,config,status,effective_at,created_by_user_id) VALUES(1,'metrics','original','v1',JSON_OBJECT(),'active','2000-01-01',1),(2,'metrics','replacement','v1',JSON_OBJECT(),'active','2000-01-01',1),(3,'metrics','draft','v1',JSON_OBJECT(),'draft','2000-01-01',1),(4,'metrics','future','v1',JSON_OBJECT(),'active','2099-01-01',1),(5,'pricing','wrong-kind','v1',JSON_OBJECT(),'active','2000-01-01',1)",
		"INSERT INTO nodes(id,name,agent_endpoint,token_ciphertext,key_version,server_cert_sha256,default_storage_pool,default_project_name,allocatable_vcpus,allocatable_memory_bytes,allocatable_disk_bytes,resident_vcpu_budget,resident_memory_budget_bytes,resident_disk_budget_bytes,status,administrative_state,health,metrics_policy_id,connection_verified_at,resources_sampled_at,resources) VALUES(1,'node','https://127.0.0.1:8443','unused',1,REPEAT('a',32),'default','default',4,1024,1024,4,1024,1024,'draining','draining','online',1,NOW(),NOW(),JSON_OBJECT())",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	a := platform.New(db, platform.Config{})
	r := gin.New()
	r.Use(platform.RequestMiddleware())
	admin := r.Group("/api/v1/admin", func(c *gin.Context) { c.Set("user", platform.User{ID: 1, Role: "admin"}); c.Next() }, platform.Admin(), a.Mutations())
	registerNodes(a, admin)
	call := func(method, body, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/admin/nodes/1", strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", etag)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	get := func() (string, platform.Row) {
		t.Helper()
		w := call("GET", "", "")
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		var result struct {
			Data platform.Row `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return w.Header().Get("ETag"), result.Data
	}
	initial, _ := get()
	etag := initial
	for _, body := range []string{`{"administrative_state":"active"}`, `{"administrative_state":"maintenance"}`, `{"administrative_state":"disabled"}`, `{"name":"renamed"}`} {
		w := call("PATCH", body, etag)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
		var node platform.Row
		etag, node = get()
		if platform.Text(node["metrics_policy_id"]) != "1" {
			t.Fatal("omitted policy changed binding")
		}
		var requested platform.Row
		json.Unmarshal([]byte(body), &requested)
		for key, value := range requested {
			if node[key] != value {
				t.Fatalf("%s not applied: %v", key, node)
			}
		}
	}
	for _, body := range []string{`{}`, `{"metrics_policy_id":null}`, `{"metrics_policy_id":""}`, `{"metrics_policy_id":2}`, `{"metrics_policy_id":"3"}`, `{"metrics_policy_id":"4"}`, `{"metrics_policy_id":"5"}`} {
		w := call("PATCH", body, etag)
		if w.Code != 422 {
			t.Fatalf("invalid patch %s: %d %s", body, w.Code, w.Body.String())
		}
		current, node := get()
		if current != etag || platform.Text(node["metrics_policy_id"]) != "1" {
			t.Fatal("rejected patch mutated node")
		}
	}
	if w := call("PATCH", `{"metrics_policy_id":"2"}`, etag); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	etag, node := get()
	if platform.Text(node["metrics_policy_id"]) != "2" {
		t.Fatal("replacement not persisted")
	}
	if w := call("PATCH", `{"name":"stale"}`, initial); w.Code != 412 {
		t.Fatalf("stale revision: %d %s", w.Code, w.Body.String())
	}
	if err := db.Exec("UPDATE nodes SET health='offline' WHERE id=1").Error; err != nil {
		t.Fatal(err)
	}
	if w := call("PATCH", `{"administrative_state":"active"}`, etag); w.Code != 409 {
		t.Fatalf("offline activation: %d %s", w.Code, w.Body.String())
	}
}
