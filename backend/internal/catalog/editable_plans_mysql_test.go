package catalog

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestMySQLEditablePlansIndependentAndAtomic(t *testing.T) {
	ctx, root := openCatalogMySQL(t, "ci_editable_api_")
	if err := root.Connection(func(db *gorm.DB) error {
		exec := func(s string) {
			t.Helper()
			if err := db.Exec(s).Error; err != nil {
				t.Fatal(err)
			}
		}
		exec(`INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'plan-admin','unused','admin','active')`)
		for _, table := range []string{"instance_plans", "plan_nodes", "plan_images", "plan_network_options"} {
			exec("CREATE TEMPORARY TABLE fixture_" + table + " LIKE " + table)
			exec("ALTER TABLE fixture_" + table + " RENAME TO " + table)
		}
		for table, definition := range map[string]string{
			"policy_versions":       "id BIGINT PRIMARY KEY,kind VARCHAR(32),status VARCHAR(24),effective_at DATETIME,source_id BIGINT,config JSON",
			"nodes":                 "id BIGINT PRIMARY KEY,administrative_state VARCHAR(24),health VARCHAR(24),connection_verified_at DATETIME(6),resources JSON",
			"images":                "id BIGINT PRIMARY KEY,status VARCHAR(24),image_type VARCHAR(24),properties JSON",
			"image_nodes":           "id BIGINT PRIMARY KEY,image_id BIGINT,node_id BIGINT,project_name VARCHAR(64),architecture VARCHAR(24),fingerprint BINARY(32),gpu_ready BOOL,cloud_init BOOL,properties JSON,status VARCHAR(24),last_verified_at DATETIME(6)",
			"node_network_bindings": "id BIGINT PRIMARY KEY,node_id BIGINT,project_name VARCHAR(64),network_pool_id BIGINT,status VARCHAR(24),validation_status VARCHAR(24),mode VARCHAR(24)",
			"network_pools":         "id BIGINT PRIMARY KEY,status VARCHAR(24)",
		} {
			exec("CREATE TEMPORARY TABLE " + table + " (" + definition + ") ENGINE=InnoDB")
		}
		exec(`INSERT INTO policy_versions VALUES(1,'pricing','active','2000-01-01',1,JSON_OBJECT()),(2,'credit','active','2000-01-01',1,JSON_OBJECT()),(3,'gpu-idle','active','2000-01-01',1,'{"enabled":false,"min_timeout_seconds":60,"max_timeout_seconds":3600,"default_timeout_seconds":600,"allowed_actions":["detach_gpu"]}'),(4,'gpu-scheduling','active','2000-01-01',1,JSON_OBJECT())`)
		exec(`INSERT INTO policy_versions VALUES(5,'metrics','active','2000-01-01',NULL,JSON_OBJECT())`)
		exec(`INSERT INTO nodes VALUES(1,'active','online',NOW(6),'{"architecture":"x86_64"}')`)
		exec(`INSERT INTO images VALUES(1,'active','container',JSON_OBJECT())`)
		exec(`INSERT INTO network_pools VALUES(1,'active')`)
		exec(`INSERT INTO node_network_bindings VALUES(1,1,'default',1,'active','verified','bridge')`)
		exec(`INSERT INTO image_nodes VALUES(1,1,1,'default','x86_64',UNHEX(REPEAT('aa',32)),FALSE,TRUE,JSON_OBJECT(),'ready',NOW(6))`)
		a := platform.New(db, platform.Config{})
		r := gin.New()
		r.Use(platform.RequestMiddleware())
		user := r.Group("/api/v1", func(c *gin.Context) { c.Set("user", platform.User{ID: 1, Role: "admin"}); c.Next() })
		registerProducts(a, user, user.Group("/admin", platform.Admin(), a.Mutations()))
		serial := 0
		call := func(method, path, body, etag string, want int) *httptest.ResponseRecorder {
			t.Helper()
			serial++
			req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", etag)
			req.Header.Set("Idempotency-Key", fmt.Sprintf("editable-%d", serial))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != want {
				t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
			}
			return w
		}
		data := func(w *httptest.ResponseRecorder) platform.Row {
			t.Helper()
			var e struct {
				Data platform.Row `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
				t.Fatal(err)
			}
			return e.Data
		}
		payload := `{"code":"%s","version":"1","name":"Original","instance_type":"container","resources":{"vcpus":2,"memory_bytes":"4294967296","disk_bytes":"21474836480"},"gpu":{"count":0,"model":null,"access_mode":"exclusive","max_hourly_pt":"0"},"node_ids":["1"],"image_ids":["1"],"network_option_ids":["1"],"allowed_overrides":{},"policy_versions":{"pricing":"1","credit":"2","gpu-idle":"3","gpu-scheduling":"4","metrics":"5"}}`
		paths := []string{}
		tags := []string{}
		for _, code := range []string{"first", "second"} {
			w := call("POST", "/admin/plans", fmt.Sprintf(payload, code), "", 201)
			d := data(w)
			if d["status"] != "disabled" {
				t.Fatal(d)
			}
			p := "/admin/plans/" + platform.Text(d["id"])
			w = call("POST", p+"/enable", "{}", fmt.Sprintf(`"rev-%s"`, platform.Text(d["revision"])), 200)
			paths = append(paths, p)
			tags = append(tags, w.Header().Get("ETag"))
		}
		second := data(call("GET", paths[1], "", "", 200))
		w := call("PATCH", paths[0], `{"code":"renamed","version":"2","name":"Edited"}`, tags[0], 200)
		fresh := w.Header().Get("ETag")
		call("PATCH", paths[0], `{"name":"stale"}`, tags[0], 412)
		call("PATCH", paths[0], `{"name":"bad","node_ids":[]}`, fresh, 422)
		got := data(call("GET", paths[0], "", "", 200))
		if got["name"] != "Edited" || got["status"] != "active" || got["revision"] != data(w)["revision"] {
			t.Fatal(got)
		}
		exec(`UPDATE image_nodes SET status='pending' WHERE id=1`)
		call("PATCH", paths[0], `{"name":"not-ready"}`, fresh, 409)
		exec(`UPDATE image_nodes SET status='ready' WHERE id=1`)
		w = call("POST", paths[0]+"/disable", "{}", fresh, 200)
		w = call("PATCH", paths[0], `{"name":"Disabled edit"}`, w.Header().Get("ETag"), 200)
		call("POST", paths[0]+"/enable", "{}", w.Header().Get("ETag"), 200)
		after := data(call("GET", paths[1], "", "", 200))
		if after["status"] != "active" || after["revision"] != second["revision"] || after["name"] != second["name"] {
			t.Fatalf("second changed: %v", after)
		}
		for _, suffix := range []string{"publish", "retire"} {
			call("POST", paths[0]+"/"+suffix, "{}", "", 404)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
