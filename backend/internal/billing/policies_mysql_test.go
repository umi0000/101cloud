package billing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	migrations "101cloud/backend/sql"
	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// openBillingMySQL migrates an isolated database for billing consumer-boundary
// regressions; the database is dropped when the test ends.
func openBillingMySQL(t *testing.T, prefix string) (context.Context, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated billing regression")
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

// Policy profiles are created active, edited in place, switched through the
// platform bindings and deleted only when nothing references them.
func TestMySQLPolicyProfilesEditSwitchDelete(t *testing.T) {
	ctx, db := openBillingMySQL(t, "ci_policy_profiles_")
	if err := db.Exec("INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'policy-admin','unused-test-hash','admin','active')").Error; err != nil {
		t.Fatal(err)
	}
	a := platform.New(db, platform.Config{})
	r := gin.New()
	r.Use(platform.RequestMiddleware())
	admin := r.Group("/api/v1/admin", func(c *gin.Context) { c.Set("user", platform.User{ID: 1, Role: "admin"}); c.Next() }, platform.Admin(), a.Mutations())
	registerPolicies(a, admin)
	requests := 0
	call := func(method, path, body, etag string) *httptest.ResponseRecorder {
		requests++
		req := httptest.NewRequest(method, "/api/v1/admin"+path, strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		if etag != "" {
			req.Header.Set("If-Match", etag)
		}
		req.Header.Set("Idempotency-Key", "policy-profile-"+strconv.Itoa(requests))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	type profile struct {
		ID     string         `json:"id"`
		Status string         `json:"status"`
		Config map[string]any `json:"config"`
	}
	decode := func(w *httptest.ResponseRecorder) profile {
		t.Helper()
		var envelope struct {
			Data profile `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%v: %s", err, w.Body.String())
		}
		return envelope.Data
	}
	failure := func(w *httptest.ResponseRecorder) (string, string) {
		t.Helper()
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%v: %s", err, w.Body.String())
		}
		return envelope.Error.Code, envelope.Error.Message
	}
	configs := map[string]any{
		"pricing":        map[string]any{"cpu_hourly_pt": "1.000000", "memory_gib_hourly_pt": "0.500000", "disk_gib_hourly_pt": "0.010000"},
		"gpu-pricing":    map[string]any{"gpu_model": "NVIDIA A100", "access_mode": "shared", "gpu_hourly_pt": "3.000000"},
		"credit":         validCreditPolicy(),
		"gpu-scheduling": map[string]any{"poll_interval_seconds": 10, "metrics_max_age_seconds": 60, "sm_weight": 0.5, "memory_weight": 0.5, "source_high_load_pct": 90, "target_low_load_pct": 30, "evaluation_window_seconds": 300, "min_improvement_pct": 10, "cooldown_seconds": 600, "rebindings_window_seconds": 3600, "max_rebindings_per_window": 2, "max_concurrent_per_node": 1, "allow_price_increase": false},
		"gpu-idle":       map[string]any{"enabled": true, "default_timeout_seconds": 1800, "min_timeout_seconds": 300, "max_timeout_seconds": 7200, "allowed_actions": []string{"release_gpu", "stop_instance"}, "gpu_utilization_threshold_pct": 5, "gpu_memory_threshold_pct": 5, "graceful_stop_timeout_seconds": 60, "allow_force_stop": false},
		"metrics":        map[string]any{"raw_retention_days": 7, "rollup_retention_days": 30, "bucket_seconds": 60, "collection_interval_seconds": 5},
	}
	create := func(kind, code string) profile {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"code": code, "version": "1", "config": configs[kind]})
		w := call(http.MethodPost, "/policies/"+kind+"/versions", string(body), "")
		if w.Code != 201 {
			t.Fatalf("create %s: %d %s", kind, w.Code, w.Body.String())
		}
		p := decode(w)
		if p.Status != "active" {
			t.Fatalf("created %s profile is not active: %s", kind, w.Body.String())
		}
		return p
	}
	etag := func(kind, id string) string {
		t.Helper()
		w := call(http.MethodGet, "/policies/"+kind+"/versions/"+id, "", "")
		if w.Code != 200 {
			t.Fatalf("detail %s/%s: %d %s", kind, id, w.Code, w.Body.String())
		}
		return w.Header().Get("ETag")
	}
	sourceOf := func(id string) uint64 {
		t.Helper()
		var source []uint64
		if err := db.Table("policy_versions").Where("id = ?", id).Pluck("source_id", &source).Error; err != nil || len(source) != 1 {
			t.Fatalf("policy %s source: %v %v", id, source, err)
		}
		return source[0]
	}
	count := func(table, where string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := db.Table(table).Where(where, args...).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	bind := func(bindings map[string]string) {
		t.Helper()
		w := call(http.MethodGet, "/platform-policy-bindings", "", "")
		body, _ := json.Marshal(bindings)
		if w = call(http.MethodPut, "/platform-policy-bindings", string(body), w.Header().Get("ETag")); w.Code != 200 {
			t.Fatalf("bind: %d %s", w.Code, w.Body.String())
		}
	}

	// Create: every kind is active and effective immediately, hence bindable.
	defaults := map[string]string{}
	for _, kind := range requiredPolicyKinds {
		defaults[kind] = create(kind, "default-"+kind).ID
	}
	var sourceStatus []string
	if err := db.Table("credit_policy_versions").Where("id = ?", sourceOf(defaults["credit"])).Pluck("status", &sourceStatus).Error; err != nil || len(sourceStatus) != 1 || sourceStatus[0] != "active" {
		t.Fatalf("credit source row not active: %v %v", sourceStatus, err)
	}
	bind(defaults)

	// Edit in place: the bound, active profile and its source row both change.
	creditID := defaults["credit"]
	edited := validCreditPolicy()
	edited["daily_allowance_pt"] = "12.500000"
	body, _ := json.Marshal(map[string]any{"config": edited})
	if w := call(http.MethodPatch, "/policies/credit/versions/"+creditID, string(body), ""); w.Code != 428 {
		t.Fatalf("edit without If-Match: %d %s", w.Code, w.Body.String())
	}
	before := etag("credit", creditID)
	w := call(http.MethodPatch, "/policies/credit/versions/"+creditID, string(body), before)
	if w.Code != 200 {
		t.Fatalf("edit active profile: %d %s", w.Code, w.Body.String())
	}
	if p := decode(w); p.Status != "active" || p.Config["daily_allowance_pt"] != "12.500000" {
		t.Fatalf("edit response: %s", w.Body.String())
	}
	var allowance []string
	if err := db.Table("credit_policy_versions").Where("id = ?", sourceOf(creditID)).Pluck("CAST(daily_allowance_pt AS CHAR)", &allowance).Error; err != nil || len(allowance) != 1 || allowance[0] != "12.500000" {
		t.Fatalf("source row not updated: %v %v", allowance, err)
	}
	if w = call(http.MethodPatch, "/policies/credit/versions/"+creditID, string(body), before); w.Code != 412 {
		t.Fatalf("stale edit accepted: %d %s", w.Code, w.Body.String())
	}

	// Delete the platform default: rejected, rows kept.
	if w = call(http.MethodDelete, "/policies/credit/versions/"+creditID, "", ""); w.Code != 428 {
		t.Fatalf("delete without If-Match: %d %s", w.Code, w.Body.String())
	}
	w = call(http.MethodDelete, "/policies/credit/versions/"+creditID, "", etag("credit", creditID))
	if code, message := failure(w); w.Code != 409 || code != "configuration_conflict" || !strings.Contains(message, "默认") {
		t.Fatalf("bound profile delete: %d %s", w.Code, w.Body.String())
	}
	if count("policy_versions", "id = ?", creditID) != 1 || count("credit_policy_versions", "id = ?", sourceOf(creditID)) != 1 {
		t.Fatal("rejected delete removed the bound profile")
	}

	// Delete a profile referenced by a credit account (source FK): rejected.
	accountCredit := create("credit", "account-credit")
	if err := db.Exec("INSERT INTO credit_accounts(id,user_id,credit_policy_version_id,status,version) VALUES(1,1,?,'active',0)", sourceOf(accountCredit.ID)).Error; err != nil {
		t.Fatal(err)
	}
	w = call(http.MethodDelete, "/policies/credit/versions/"+accountCredit.ID, "", etag("credit", accountCredit.ID))
	if code, message := failure(w); w.Code != 409 || code != "configuration_conflict" || !strings.Contains(message, "1 个额度账户") {
		t.Fatalf("account-referenced delete: %d %s", w.Code, w.Body.String())
	}
	if count("policy_versions", "id = ?", accountCredit.ID) != 1 || count("credit_policy_versions", "id = ?", sourceOf(accountCredit.ID)) != 1 {
		t.Fatal("rejected delete removed the account profile")
	}

	// Delete a non-economic profile referenced only by plan policy_versions JSON: rejected.
	planScheduling := create("gpu-scheduling", "plan-scheduling")
	versions, _ := json.Marshal(map[string]string{"pricing": defaults["pricing"], "credit": defaults["credit"], "gpu-idle": defaults["gpu-idle"], "gpu-scheduling": planScheduling.ID})
	if err := db.Exec("INSERT INTO instance_plans(id,code,version,name,image_type,vcpus,memory_bytes,disk_bytes,gpu_count,pricing_version_id,credit_policy_version_id,idle_policy_version_id,init_parameters,allow_residency,status,allowed_overrides,policy_versions) VALUES(1,'plan','1','Plan','container',1,1073741824,10737418240,0,?,?,?,JSON_OBJECT(),FALSE,'draft',JSON_OBJECT(),?)",
		sourceOf(defaults["pricing"]), sourceOf(defaults["credit"]), sourceOf(defaults["gpu-idle"]), string(versions)).Error; err != nil {
		t.Fatal(err)
	}
	w = call(http.MethodDelete, "/policies/gpu-scheduling/versions/"+planScheduling.ID, "", etag("gpu-scheduling", planScheduling.ID))
	if code, message := failure(w); w.Code != 409 || code != "configuration_conflict" || !strings.Contains(message, "1 个套餐") {
		t.Fatalf("plan-referenced delete: %d %s", w.Code, w.Body.String())
	}
	if count("policy_versions", "id = ?", planScheduling.ID) != 1 {
		t.Fatal("rejected delete removed the plan profile")
	}

	// Switch the platform default, then delete the previous default: both rows go.
	replacement := create("credit", "replacement-credit")
	switched := map[string]string{}
	for k, v := range defaults {
		switched[k] = v
	}
	switched["credit"] = replacement.ID
	bind(switched)
	var bound []uint64
	if err := db.Table("platform_policy_bindings").Where("kind = 'credit'").Pluck("policy_id", &bound).Error; err != nil || len(bound) != 1 || strconv.FormatUint(bound[0], 10) != replacement.ID {
		t.Fatalf("default not switched: %v %v", bound, err)
	}
	// The original default is still referenced by the plan's credit source FK.
	w = call(http.MethodDelete, "/policies/credit/versions/"+creditID, "", etag("credit", creditID))
	if code, message := failure(w); w.Code != 409 || code != "configuration_conflict" || !strings.Contains(message, "1 个套餐") {
		t.Fatalf("plan source-referenced delete: %d %s", w.Code, w.Body.String())
	}
	// An unreferenced, unbound profile is deleted together with its source row.
	unused := create("credit", "unused-credit")
	unusedSource := sourceOf(unused.ID)
	if w = call(http.MethodDelete, "/policies/credit/versions/"+unused.ID, "", `"rev-99"`); w.Code != 412 {
		t.Fatalf("stale delete: %d %s", w.Code, w.Body.String())
	}
	if w = call(http.MethodDelete, "/policies/credit/versions/"+unused.ID, "", etag("credit", unused.ID)); w.Code != 204 {
		t.Fatalf("unreferenced delete: %d %s", w.Code, w.Body.String())
	}
	if count("policy_versions", "id = ?", unused.ID) != 0 || count("credit_policy_versions", "id = ?", unusedSource) != 0 {
		t.Fatal("delete left policy or source row behind")
	}
	if w = call(http.MethodGet, "/policies/credit/versions/"+unused.ID, "", ""); w.Code != 404 {
		t.Fatalf("deleted profile still readable: %d", w.Code)
	}
	// Removing the plan releases the JSON-only reference as well.
	if err := db.Exec("DELETE FROM instance_plans WHERE id = 1").Error; err != nil {
		t.Fatal(err)
	}
	if w = call(http.MethodDelete, "/policies/gpu-scheduling/versions/"+planScheduling.ID, "", etag("gpu-scheduling", planScheduling.ID)); w.Code != 204 {
		t.Fatalf("released profile delete: %d %s", w.Code, w.Body.String())
	}

	// The draft workflow endpoints no longer exist.
	for _, action := range []string{"publish", "retire"} {
		if w = call(http.MethodPost, "/policies/credit/versions/"+replacement.ID+"/"+action, `{}`, etag("credit", replacement.ID)); w.Code != 404 {
			t.Fatalf("%s route still served: %d %s", action, w.Code, w.Body.String())
		}
	}
}
