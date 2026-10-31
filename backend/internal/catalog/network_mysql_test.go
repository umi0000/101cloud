package catalog

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	migrations "101cloud/backend/sql"
	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// openCatalogMySQL migrates an isolated database for catalog consumer-boundary
// regressions; the database is dropped when the test ends.
func openCatalogMySQL(t *testing.T, prefix string) (context.Context, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated catalog regression")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	owner, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := prefix + hex.EncodeToString(nonce[:])
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	if _, err = owner.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Exec("DROP DATABASE `" + name + "`") })
	cfg.DBName = name
	db, err := platform.OpenDB(platform.Config{DSN: cfg.FormatDSN()})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err = migrations.Up(ctx, db); err != nil {
		t.Fatal(err)
	}
	return ctx, db
}

// Range editing must preserve existing address inventory and use the resulting
// gateway/range together, rather than checking either against the old fields.
func TestMySQLPoolRangeEditing(t *testing.T) {
	ctx, db := openCatalogMySQL(t, "ci_pool_range_")
	for _, statement := range []string{
		"INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'range-admin','unused-test-hash','admin','active')",
		"INSERT INTO network_pools(id,name,address_family,network_address,prefix_length,range_start,range_end,gateway,dns_servers,status,revision) VALUES(1,'test',4,INET6_ATON('192.0.2.0'),24,INET6_ATON('192.0.2.116'),INET6_ATON('192.0.2.116'),INET6_ATON('192.0.2.2'),JSON_ARRAY(),'active',1)",
		"INSERT INTO ip_addresses(network_pool_id,address_family,address,status) VALUES(1,4,INET6_ATON('192.0.2.116'),'available')",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	a := platform.New(db, platform.Config{})
	r := gin.New()
	r.Use(platform.RequestMiddleware())
	admin := r.Group("/api/v1/admin", func(c *gin.Context) { c.Set("user", platform.User{ID: 1, Role: "admin"}); c.Next() }, platform.Admin(), a.Mutations())
	registerNetworks(a, r.Group("/api/v1"), admin)
	call := func(method, path, body, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/admin"+path, strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", etag)
		req.Header.Set("Idempotency-Key", path+body)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	get := func() (string, map[string]any) {
		t.Helper()
		w := call(http.MethodGet, "/network-pools/1", "", "")
		var data struct {
			Data map[string]any `json:"data"`
		}
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return w.Header().Get("ETag"), data.Data
	}
	initial, _ := get()
	w := call(http.MethodPatch, "/network-pools/1", `{"range_end":"192.0.2.130"}`, initial)
	if w.Code != 200 {
		t.Fatalf("expansion failed: %d %s", w.Code, w.Body.String())
	}
	etag, data := get()
	if data["range_start"] != "192.0.2.116" || data["range_end"] != "192.0.2.130" {
		t.Fatalf("incorrect resulting range: %v", data)
	}
	if w = call(http.MethodPatch, "/network-pools/1", `{"range_end":"192.0.2.140"}`, initial); w.Code != 412 {
		t.Fatalf("stale range write accepted: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"range_start":"192.0.2.117"}`, 409, "pool_range_in_use"},
		{`{"range_end":"192.0.2.115"}`, 422, "invalid_network"},
		{`{"range_end":"192.0.3.1"}`, 422, "invalid_network"},
		{`{"range_end":"::ffff:192.0.2.120"}`, 422, "invalid_network"},
		{`{"range_start":"192.0.2.0"}`, 422, "invalid_network"},
		{`{"range_end":"192.0.2.255"}`, 422, "invalid_network"},
		{`{"range_start":"192.0.2.1"}`, 422, "invalid_network"},
	} {
		w = call(http.MethodPatch, "/network-pools/1", tc.body, etag)
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if w.Code != tc.status || envelope.Error.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.body, w.Code, w.Body.String())
		}
		unchanged, snapshot := get()
		if unchanged != etag || snapshot["range_start"] != "192.0.2.116" || snapshot["range_end"] != "192.0.2.130" {
			t.Fatalf("rejected update changed pool: %v", snapshot)
		}
	}
	// The combined update may move the gateway out of the newly expanded range.
	w = call(http.MethodPatch, "/network-pools/1", `{"range_start":"192.0.2.2","gateway":"192.0.2.1"}`, etag)
	if w.Code != 200 {
		t.Fatalf("combined range/gateway rejected against stale range: %d %s", w.Code, w.Body.String())
	}
	etag, _ = get()
	w = call(http.MethodPatch, "/network-pools/1", `{"range_end":"192.0.2.116"}`, etag)
	if w.Code != 200 {
		t.Fatalf("safe shrink rejected: %d %s", w.Code, w.Body.String())
	}
	var inventory int64
	if err := db.Table("ip_addresses").Where("network_pool_id=1 AND address=INET6_ATON('192.0.2.116') AND status='available'").Count(&inventory).Error; err != nil || inventory != 1 {
		t.Fatalf("range editing changed inventory: %d %v", inventory, err)
	}
	etag, _ = get()
	for _, tc := range []struct {
		body, reason string
		status       int
	}{
		{`{"addresses":["192.0.2.50","192.0.2.116"]}`, "already_registered", 409},
		{`{"addresses":["192.0.2.50","192.0.2.50"]}`, "duplicate_in_batch", 422},
		{`{"addresses":["192.0.2.50","192.0.2.1"]}`, "gateway", 422},
		{`{"addresses":["192.0.2.50","192.0.2.0"]}`, "reserved_address", 422},
		{`{"addresses":["192.0.2.50","192.0.2.255"]}`, "reserved_address", 422},
		{`{"addresses":["192.0.2.50","192.0.2.117"]}`, "outside_range", 422},
		{`{"addresses":["192.0.2.50","192.0.3.5"]}`, "outside_subnet", 422},
		{`{"addresses":["192.0.2.50","invalid"]}`, "invalid_address", 422},
	} {
		w = call(http.MethodPost, "/network-pools/1/addresses", tc.body, etag)
		var failure struct {
			Error struct {
				Code    string `json:"code"`
				Details struct {
					Reason string `json:"reason"`
					Field  string `json:"field"`
				} `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil {
			t.Fatal(err)
		}
		if w.Code != tc.status || failure.Error.Details.Reason != tc.reason || failure.Error.Details.Field != "addresses[1]" {
			t.Fatalf("incorrect batch rejection: %d %s", w.Code, w.Body.String())
		}
		wantCode := "invalid_network"
		if tc.status == 409 {
			wantCode = "configuration_conflict"
		}
		if failure.Error.Code != wantCode {
			t.Fatalf("changed stable error code: %s", w.Body.String())
		}
		unchanged, _ := get()
		if unchanged != etag {
			t.Fatalf("rejected batch changed pool revision: %s -> %s", etag, unchanged)
		}
		var total int64
		if err := db.Table("ip_addresses").Where("network_pool_id=1").Count(&total).Error; err != nil || total != 1 {
			t.Fatalf("partial batch was persisted: %d %v", total, err)
		}
	}
}

// Without availability zones, a client narrows direct network options to the
// node it intends to use; every item identifies its node.
func TestMySQLNetworkOptionsFilterByNode(t *testing.T) {
	ctx, db := openCatalogMySQL(t, "ci_net_options_")
	cert := "UNHEX(SHA2('node-cert',256))"
	for _, statement := range []string{
		"INSERT INTO nodes(id,name,agent_endpoint,token_ciphertext,key_version,server_cert_sha256,default_storage_pool,default_project_name,allocatable_vcpus,allocatable_memory_bytes,allocatable_disk_bytes,resident_vcpu_budget,resident_memory_budget_bytes,resident_disk_budget_bytes,status) VALUES(1,'node-a','https://node-a.example:8443','cipher',1," + cert + ",'default','default',4,1,1,4,1,1,'active'),(2,'node-b','https://node-b.example:8443','cipher',1," + cert + ",'default','default',4,1,1,4,1,1,'active')",
		"INSERT INTO network_pools(id,name,address_family,network_address,prefix_length,range_start,range_end,gateway,dns_servers,status,revision) VALUES(1,'lan',4,INET6_ATON('192.0.2.0'),24,INET6_ATON('192.0.2.10'),INET6_ATON('192.0.2.20'),INET6_ATON('192.0.2.1'),JSON_ARRAY(),'active',1),(2,'lan-b',4,INET6_ATON('198.51.100.0'),24,INET6_ATON('198.51.100.10'),INET6_ATON('198.51.100.20'),INET6_ATON('198.51.100.1'),JSON_ARRAY(),'active',1)",
		"INSERT INTO node_network_bindings(id,node_id,network_pool_id,mode,parent_interface,project_name,status,validation_status) VALUES(1,1,1,'bridge','br0','default','active','verified'),(2,2,1,'bridge','br0','default','active','verified'),(3,2,2,'macvlan','eth0','default','active','verified')",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	a := platform.New(db, platform.Config{})
	r := gin.New()
	r.Use(platform.RequestMiddleware())
	user := r.Group("/api/v1", func(c *gin.Context) { c.Set("user", platform.User{ID: 1, Role: "user"}); c.Next() })
	registerNetworks(a, user, r.Group("/api/v1/admin"))
	options := func(query string) (int, []map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/catalog/network-options?"+query, nil).WithContext(ctx))
		var body struct {
			Data struct {
				Items []map[string]any `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return w.Code, body.Data.Items
	}
	for _, tc := range []struct {
		query string
		want  map[string]string
	}{
		{"instance_type=container", map[string]string{"1": "1", "2": "2", "3": "2"}},
		{"instance_type=container&node_id=2", map[string]string{"2": "2", "3": "2"}},
		{"instance_type=virtual-machine&node_id=2", map[string]string{"2": "2"}},
		{"instance_type=container&node_id=1", map[string]string{"1": "1"}},
	} {
		code, items := options(tc.query)
		got := map[string]string{}
		for _, item := range items {
			got[platform.Text(item["id"])] = platform.Text(item["node_id"])
		}
		if code != http.StatusOK || len(got) != len(tc.want) {
			t.Fatalf("%s: %d %v", tc.query, code, items)
		}
		for id, node := range tc.want {
			if got[id] != node {
				t.Fatalf("%s: option %s node %q, want %q (%v)", tc.query, id, got[id], node, items)
			}
		}
	}
	if code, _ := options("instance_type=container&zone_id=1"); code != http.StatusUnprocessableEntity {
		t.Fatalf("removed zone filter accepted: %d", code)
	}
}
