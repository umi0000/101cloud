package instances

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

func TestMySQLInstanceSourceImageMetadata(t *testing.T) {
	db, a := admissionMySQLFixture(t)
	for _, statement := range []string{
		"ALTER TABLE instances ADD logical_name VARCHAR(128), ADD billing_mode VARCHAR(20), ADD image_type VARCHAR(20), ADD image_id BIGINT, ADD instance_plan_id BIGINT, ADD plan_snapshot JSON, ADD policy_versions JSON, ADD observed_at DATETIME(6), ADD allowed_user_actions JSON, ADD created_at DATETIME(6), ADD updated_at DATETIME(6), ADD creation_failed_at DATETIME(6)",
		"CREATE TABLE images (id BIGINT PRIMARY KEY,name VARCHAR(128),os_name VARCHAR(100),os_release VARCHAR(100),status VARCHAR(20),source_config JSON)",
		"CREATE TABLE tasks (id BIGINT PRIMARY KEY,instance_id BIGINT,task_type VARCHAR(64),is_instance_mutation BOOLEAN,status VARCHAR(20),error_code VARCHAR(96),result_redacted JSON)",
		"CREATE TABLE credit_enforcements (id BIGINT PRIMARY KEY,status VARCHAR(20),shutdown_deadline DATETIME(6))",
		"CREATE TABLE credit_enforcement_instances (enforcement_id BIGINT,instance_id BIGINT)",
		"INSERT INTO images VALUES (10,'not-the-os','Ubuntu','24.04','active',JSON_OBJECT('secret','private-image-secret')),(20,'Ubuntu-looking-name','Debian','12','retired',JSON_OBJECT('secret','private-image-secret')),(30,'Ubuntu','Fedora','41','disabled',JSON_OBJECT('secret','private-image-secret'))",
		"UPDATE instances SET logical_name='CentOS misleading name',billing_mode='credit',image_type='container',image_id=id*10,observed_at=NOW(6),created_at='2026-11-01 00:00:00',updated_at='2026-11-01 00:00:00',incus_config_snapshot=JSON_OBJECT('private','private-instance-secret'),revision=7",
		"INSERT INTO instances (id,user_id,node_id,logical_name,product_type,billing_mode,image_type,image_id,created_at,updated_at,revision) VALUES (3,2,1,'Ubuntu misleading name','elastic','credit','container',30,'2026-11-01 00:00:00','2026-11-01 00:00:00',7),(4,1,1,'Ubuntu without metadata','elastic','credit','container',999,'2026-11-01 00:00:00','2026-11-01 00:00:00',7)",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func(table string) []platform.Row {
		t.Helper()
		var rows []platform.Row
		if err := db.Table(table).Order("id").Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		return rows
	}
	beforeInstances, beforeImages := snapshot("instances"), snapshot("images")
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user", platform.User{ID: 1})
		c.Set("admin", strings.HasPrefix(c.Request.URL.Path, "/api/v1/admin/"))
	})
	Register(a, router.Group("/api/v1"), router.Group("/api/v1/admin"))
	request := func(path string, status int) (map[string]any, http.Header) {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != status {
			t.Fatalf("%s: status %d: %s", path, response.Code, response.Body.String())
		}
		for _, secret := range []string{"private-image-secret", "private-instance-secret", "source_config", "incus_config_snapshot", "source_image_id"} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("%s leaked %s: %s", path, secret, response.Body.String())
			}
		}
		var body struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Data, response.Header()
	}
	wantOS := map[string][2]string{"1": {"Ubuntu", "24.04"}, "2": {"Debian", "12"}, "3": {"Fedora", "41"}, "4": {"", ""}}
	checkOS := func(item map[string]any) {
		t.Helper()
		id, _ := item["id"].(string)
		want, ok := wantOS[id]
		if !ok || item["image_os_name"] != want[0] || item["image_os_release"] != want[1] {
			t.Fatalf("incorrect source image metadata: %#v", item)
		}
	}
	// Exercise the real list pagination, including the joined projection's cursor
	// columns, not a DTO assembled with already-copied OS fields.
	seen := map[string]bool{}
	path := "/api/v1/instances?limit=1"
	for {
		data, _ := request(path, http.StatusOK)
		for _, raw := range data["items"].([]any) {
			item := raw.(map[string]any)
			checkOS(item)
			id := item["id"].(string)
			if id == "3" || seen[id] {
				t.Fatalf("list crossed ownership or repeated cursor item: %#v", item)
			}
			seen[id] = true
		}
		cursor, _ := data["next_cursor"].(string)
		if cursor == "" {
			break
		}
		path = "/api/v1/instances?limit=1&cursor=" + url.QueryEscape(cursor)
	}
	if len(seen) != 3 {
		t.Fatalf("owner list contains %v", seen)
	}
	adminList, _ := request("/api/v1/admin/instances", http.StatusOK)
	if len(adminList["items"].([]any)) != 4 {
		t.Fatalf("admin list omitted instances: %#v", adminList)
	}
	for _, raw := range adminList["items"].([]any) {
		checkOS(raw.(map[string]any))
	}
	filtered, _ := request("/api/v1/admin/instances?owner_user_id=2", http.StatusOK)
	if items := filtered["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != "3" {
		t.Fatalf("admin owner filter changed: %#v", filtered)
	}
	for _, id := range []string{"1", "2", "4"} {
		item, headers := request("/api/v1/instances/"+id, http.StatusOK)
		checkOS(item)
		if headers.Get("ETag") != `"rev-7"` {
			t.Fatalf("detail ETag changed: %v", headers)
		}
	}
	request("/api/v1/instances/3", http.StatusNotFound)
	item, _ := request("/api/v1/admin/instances/3", http.StatusOK)
	checkOS(item)
	if !reflect.DeepEqual(beforeInstances, snapshot("instances")) || !reflect.DeepEqual(beforeImages, snapshot("images")) {
		t.Fatal("reading image metadata mutated source images or instance configuration/revisions")
	}
}
