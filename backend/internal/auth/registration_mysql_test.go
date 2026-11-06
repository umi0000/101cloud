package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type registrationClient struct {
	h       *registrationHarness
	cookies []*http.Cookie
	csrf    string
}

type registrationHarness struct {
	t     *testing.T
	ctx   context.Context
	db    *gorm.DB
	a     *platform.App
	r     *gin.Engine
	admin *registrationClient
	guest *registrationClient
}

func newRegistrationHarness(t *testing.T) *registrationHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	db := isolatedMySQL(t, ctx)
	a := platform.New(db, platform.Config{Origin: "https://registration.example", MasterKey: bytes.Repeat([]byte{13}, 32), SessionTTL: time.Hour})
	initial, err := EnsureAdministrator(ctx, a)
	if err != nil || initial == nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, sql := range []string{
		`INSERT INTO credit_policy_versions(id,code,version,daily_allowance_pt,bonus_cap_pt,timezone,reset_time,billing_window_seconds,status,effective_at,created_by_user_id,daily_validity_seconds,shutdown_grace_seconds,grace_charge_mode,restore_action,storage_charge_mode,offline_charge_mode) VALUES(1,'registration-allowance','1',7,0,'Asia/Shanghai','06:30:00',60,'active','2000-01-01',1,86400,600,'waived','cancel_pending_stop','running_only','defer')`,
		`INSERT INTO policy_versions(id,kind,code,version,source_id,config,status,effective_at,created_by_user_id) VALUES(101,'credit','registration-allowance','1',1,JSON_OBJECT('daily_allowance_pt','7.000000','timezone','Asia/Shanghai','grant_time','06:30:00','daily_validity_seconds',86400,'billing_window_seconds',60,'shutdown_grace_seconds',600,'grace_charge_mode','waived','restore_action','cancel_pending_stop','storage_charge_mode','running_only','offline_charge_mode','defer'),'active','2000-01-01',1)`,
		`INSERT INTO policy_versions(id,kind,code,version,config,status,effective_at,created_by_user_id) VALUES(102,'metrics','not-credit','1',JSON_OBJECT('raw_retention_days',7,'rollup_retention_days',30,'bucket_seconds',60,'collection_interval_seconds',5),'active','2000-01-01',1)`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	r := gin.New()
	if err := r.SetTrustedProxies(nil); err != nil {
		t.Fatal(err)
	}
	api := r.Group("/api/v1", platform.RequestMiddleware(), CSRF(a))
	user := api.Group("", Middleware(a))
	admin := user.Group("/admin", platform.Admin(), a.Mutations())
	user.Use(a.Mutations())
	Register(a, api, user, admin)
	h := &registrationHarness{t: t, ctx: ctx, db: db, a: a, r: r}
	h.guest = h.client()
	h.admin = h.client()
	body, _ := json.Marshal(map[string]string{"username": initial.Username, "password": initial.Password})
	response := h.admin.call(http.MethodPost, "/auth/login", string(body), nil)
	h.expect(response, http.StatusOK, "")
	h.admin.cookies = response.Result().Cookies()
	var login struct {
		Data struct {
			CSRF string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	h.admin.csrf = login.Data.CSRF
	return h
}

func (h *registrationHarness) client() *registrationClient {
	client := &registrationClient{h: h}
	response := client.call(http.MethodGet, "/auth/csrf", "", nil)
	h.expect(response, http.StatusOK, "")
	client.cookies = response.Result().Cookies()
	var body struct {
		Data struct {
			CSRF string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		h.t.Fatal(err)
	}
	client.csrf = body.Data.CSRF
	return client
}

// Calls do not mutate client state, so concurrent requests can share a pre-login context.
func (client *registrationClient) call(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	h := client.h
	req := httptest.NewRequest(method, h.a.Config.Origin+"/api/v1"+path, strings.NewReader(body)).WithContext(h.ctx)
	req.RemoteAddr = "192.0.2.17:1234"
	req.Header.Set("Origin", h.a.Config.Origin)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", client.csrf)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	for _, cookie := range client.cookies {
		req.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	h.r.ServeHTTP(response, req)
	return response
}

func (h *registrationHarness) expect(response *httptest.ResponseRecorder, status int, code string) {
	h.t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		h.t.Fatalf("invalid response: %v: %s", err, response.Body.String())
	}
	if response.Code != status || body.Error.Code != code {
		h.t.Fatalf("status/code = %d/%s, want %d/%s: %s", response.Code, body.Error.Code, status, code, response.Body.String())
	}
}

func registrationSettingsBody(enabled bool, policy any) map[string]any {
	return map[string]any{"enabled": enabled, "credit_policy_version_id": policy, "limits": map[string]any{
		"max_instances": 3, "max_vcpus": 7, "max_memory_bytes": "9007199254740993", "max_disk_bytes": "18446744073709551615", "max_gpus": 2, "max_resident_instances": 1,
	}}
}

func (h *registrationHarness) settings(body map[string]any, etag string) *httptest.ResponseRecorder {
	h.t.Helper()
	if etag == "" {
		response := h.admin.call(http.MethodGet, "/admin/registration-settings", "", nil)
		h.expect(response, http.StatusOK, "")
		etag = response.Header().Get("ETag")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	// PUT intentionally uses revisions, not POST/DELETE idempotency semantics.
	return h.admin.call(http.MethodPut, "/admin/registration-settings", string(encoded), map[string]string{"If-Match": etag})
}

func (h *registrationHarness) count(table string) int64 {
	h.t.Helper()
	var count int64
	if err := h.db.Table(table).Count(&count).Error; err != nil {
		h.t.Fatal(err)
	}
	return count
}

func (h *registrationHarness) signup(name string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"username": name, "password": "registration-password"})
	return h.guest.call(http.MethodPost, "/auth/register", string(body), nil)
}

func TestMySQLRegistrationSettingsValidationAndAuthorization(t *testing.T) {
	h := newRegistrationHarness(t)
	response := h.guest.call(http.MethodGet, "/auth/registration", "", nil)
	h.expect(response, http.StatusOK, "")
	var availability struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &availability); err != nil {
		t.Fatal(err)
	}
	if len(availability.Data) != 1 || availability.Data["enabled"] != false || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("public defaults leak configuration: %s", response.Body.String())
	}
	h.expect(h.signup("disabled-user"), http.StatusForbidden, "registration_disabled")
	h.expect(h.guest.call(http.MethodGet, "/admin/registration-settings", "", nil), http.StatusUnauthorized, "unauthenticated")
	h.expect(h.guest.call(http.MethodPut, "/admin/registration-settings", `{}`, nil), http.StatusUnauthorized, "unauthenticated")
	response = h.admin.call(http.MethodGet, "/admin/registration-settings", "", nil)
	h.expect(response, http.StatusOK, "")
	initialETag := response.Header().Get("ETag")
	var defaults struct {
		Data struct {
			ID      string         `json:"id"`
			Enabled bool           `json:"enabled"`
			Policy  *string        `json:"credit_policy_version_id"`
			Limits  map[string]any `json:"limits"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &defaults); err != nil {
		t.Fatal(err)
	}
	if initialETag == "" || defaults.Data.ID != "1" || defaults.Data.Enabled || defaults.Data.Policy != nil || len(defaults.Data.Limits) != 6 {
		t.Fatalf("unsafe installation defaults: %s", response.Body.String())
	}
	for name, value := range defaults.Data.Limits {
		if strings.HasSuffix(name, "_bytes") {
			if value != "0" {
				t.Fatalf("nonzero default %s: %v", name, value)
			}
		} else if value != float64(0) {
			t.Fatalf("nonzero default %s: %v", name, value)
		}
	}
	if err := h.db.Exec("UPDATE users SET role='user' WHERE id=1").Error; err != nil {
		t.Fatal(err)
	}
	h.expect(h.admin.call(http.MethodGet, "/admin/registration-settings", "", nil), http.StatusForbidden, "forbidden")
	h.expect(h.admin.call(http.MethodPut, "/admin/registration-settings", `{}`, nil), http.StatusForbidden, "forbidden")
	if err := h.db.Exec("UPDATE users SET role='admin' WHERE id=1").Error; err != nil {
		t.Fatal(err)
	}
	for _, policy := range []any{nil, "1", "102", "999999", 101} {
		h.expect(h.settings(registrationSettingsBody(true, policy), ""), http.StatusUnprocessableEntity, "validation_failed")
	}
	for _, change := range []func(map[string]any){
		func(b map[string]any) { delete(b, "enabled") },
		func(b map[string]any) { delete(b, "credit_policy_version_id") },
		func(b map[string]any) { b["role"] = "admin" },
		func(b map[string]any) { b["limits"].(map[string]any)["max_gpus"] = -1 },
		func(b map[string]any) { b["limits"].(map[string]any)["max_vcpus"] = uint64(4294967296) },
		func(b map[string]any) { b["limits"].(map[string]any)["max_resident_instances"] = 4 },
		func(b map[string]any) { b["limits"].(map[string]any)["max_memory_bytes"] = "01" },
		func(b map[string]any) { b["limits"].(map[string]any)["max_disk_bytes"] = "18446744073709551616" },
		func(b map[string]any) { b["limits"].(map[string]any)["max_disk_bytes"] = 1 },
		func(b map[string]any) { delete(b["limits"].(map[string]any), "max_gpus") },
		func(b map[string]any) { b["limits"].(map[string]any)["unexpected"] = 1 },
	} {
		body := registrationSettingsBody(false, nil)
		change(body)
		h.expect(h.settings(body, ""), http.StatusUnprocessableEntity, "validation_failed")
	}
	body, _ := json.Marshal(registrationSettingsBody(true, "101"))
	h.expect(h.admin.call(http.MethodPut, "/admin/registration-settings", string(body), nil), http.StatusPreconditionRequired, "precondition_required")
	response = h.settings(registrationSettingsBody(true, "101"), initialETag)
	h.expect(response, http.StatusOK, "")
	if response.Header().Get("ETag") == initialETag {
		t.Fatal("settings revision did not advance")
	}
	h.expect(h.settings(registrationSettingsBody(false, nil), initialETag), http.StatusPreconditionFailed, "revision_conflict")
	h.expect(h.settings(registrationSettingsBody(false, nil), ""), http.StatusOK, "")
}

func TestMySQLRegistrationCopiesDefaultsAndDailyGrantWithoutPrivilege(t *testing.T) {
	h := newRegistrationHarness(t)
	h.expect(h.settings(registrationSettingsBody(true, "101"), ""), http.StatusOK, "")
	for _, field := range []string{`"role":"admin"`, `"status":"active"`, `"limits":{}`, `"credit_policy_version_id":"101"`} {
		h.expect(h.guest.call(http.MethodPost, "/auth/register", `{"username":"injected","password":"registration-password",`+field+`}`, nil), http.StatusUnprocessableEntity, "validation_failed")
	}
	for _, body := range []string{
		`{"username":"","password":"registration-password"}`,
		`{"username":"bad-password","password":"short"}`,
		`{"username":"bad-email","password":"registration-password","email":"not-an-email"}`,
	} {
		h.expect(h.guest.call(http.MethodPost, "/auth/register", body, nil), http.StatusUnprocessableEntity, "validation_failed")
	}
	response := h.guest.call(http.MethodPost, "/auth/register", `{"username":"self-user","password":"registration-password","email":"self@example.test"}`, nil)
	h.expect(response, http.StatusCreated, "")
	if len(response.Result().Cookies()) != 0 || strings.Contains(response.Body.String(), "password") {
		t.Fatalf("registration leaked credentials or logged in: %s", response.Body.String())
	}
	var result struct {
		Data struct {
			ID     string `json:"id"`
			Role   string `json:"role"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Role != "user" || result.Data.Status != "active" || result.Data.ID == "" {
		t.Fatalf("incorrect account: %s", response.Body.String())
	}
	userID, _ := strconv.ParseUint(result.Data.ID, 10, 64)
	limits, err := getLimits(h.db, userID, false)
	if err != nil {
		t.Fatal(err)
	}
	for name, expected := range map[string]uint64{"max_instances": 3, "max_vcpus": 7, "max_memory_bytes": 9007199254740993, "max_disk_bytes": 18446744073709551615, "max_gpu_allocations": 2, "max_resident_instances": 1} {
		if platform.Uint(limits[name]) != expected {
			t.Fatalf("copied %s=%v, want %d", name, limits[name], expected)
		}
	}
	var account platform.Row
	if err := h.db.Table("credit_accounts").Where("user_id=?", userID).Take(&account).Error; err != nil {
		t.Fatal(err)
	}
	if platform.Uint(account["credit_policy_version_id"]) != 1 || platform.Text(account["status"]) != "active" {
		t.Fatalf("account did not resolve public policy to source: %#v", account)
	}
	for _, table := range []string{"credit_buckets", "credit_ledger", "credit_daily_grants"} {
		if count := h.count(table); count != 1 {
			t.Fatalf("registration must grant once in %s: %d", table, count)
		}
	}
	var granted int64
	if err := h.db.Table("credit_buckets b").
		Joins("JOIN credit_daily_grants g ON g.bucket_id=b.id AND g.account_id=b.account_id").
		Joins("JOIN credit_ledger l ON l.bucket_id=b.id AND l.account_id=b.account_id").
		Joins("JOIN credit_accounts a ON a.id=b.account_id").
		Joins("JOIN users u ON u.id=a.user_id").
		Where("a.user_id=? AND b.source_type='daily' AND b.business_key=CONCAT('daily:',g.business_date) AND l.business_key=b.business_key AND l.entry_type='grant' AND b.granted_pt=7 AND b.remaining_pt=7 AND b.reserved_pt=0 AND l.amount_delta_pt=7 AND l.remaining_after_pt=7 AND g.policy_version_id=1 AND b.valid_from>=u.created_at AND b.valid_from=g.valid_from AND b.expires_at=g.expires_at AND b.expires_at=DATE_ADD(b.valid_from, INTERVAL 86400 SECOND) AND a.grant_cursor_at<=b.valid_from", userID).
		Count(&granted).Error; err != nil || granted != 1 {
		t.Fatalf("registration daily bucket/ledger/cycle mismatch: count=%d error=%v", granted, err)
	}
	var audit platform.Row
	if err := h.db.Table("audit_logs").Where("event_type='auth.register'").Take(&audit).Error; err != nil {
		t.Fatal(err)
	}
	if platform.Uint(audit["actor_user_id"]) != userID || strings.Contains(platform.Text(audit["result_redacted"]), "password") {
		t.Fatalf("unsafe registration audit: %#v", audit)
	}
	h.expect(h.guest.call(http.MethodPost, "/auth/register", `{"username":"email-conflict","password":"registration-password","email":"self@example.test"}`, nil), http.StatusConflict, "configuration_conflict")
	if h.count("users") != 2 || h.count("user_limits") != 2 || h.count("credit_accounts") != 1 {
		t.Fatal("email uniqueness failure left partial rows")
	}
	// Registration does not authenticate the existing pre-login browser context.
	h.expect(h.guest.call(http.MethodGet, "/me", "", nil), http.StatusUnauthorized, "unauthenticated")
	h.expect(h.guest.call(http.MethodPost, "/auth/login", `{"username":"self-user","password":"registration-password"}`, nil), http.StatusOK, "")
	changed := registrationSettingsBody(true, "101")
	changed["limits"].(map[string]any)["max_vcpus"] = 19
	h.expect(h.settings(changed, ""), http.StatusOK, "")
	h.expect(h.signup("later-user"), http.StatusCreated, "")
	limits, err = getLimits(h.db, userID, false)
	if err != nil || platform.Uint(limits["max_vcpus"]) != 7 {
		t.Fatalf("settings retroactively changed an account: %#v %v", limits, err)
	}
	var later struct {
		MaxVCPUs uint64 `gorm:"column:max_vcpus"`
	}
	if err := h.db.Table("user_limits l").Select("l.max_vcpus").Joins("JOIN users u ON u.id=l.user_id").Where("u.username='later-user'").Take(&later).Error; err != nil || later.MaxVCPUs != 19 {
		t.Fatalf("new registration missed changed defaults: %#v %v", later, err)
	}
	h.expect(h.admin.call(http.MethodPost, "/admin/users", `{"username":"admin-created","initial_password":"registration-password","role":"user","credit_policy_version_id":"101"}`, map[string]string{"Idempotency-Key": "admin-create-unchanged"}), http.StatusCreated, "")
	var adminLimits platform.Row
	if err := h.db.Table("user_limits l").Select("l.*").Joins("JOIN users u ON u.id=l.user_id").Where("u.username='admin-created'").Take(&adminLimits).Error; err != nil {
		t.Fatal(err)
	}
	for _, key := range registrationLimitColumns {
		if platform.Uint(adminLimits[key]) != 0 {
			t.Fatalf("registration defaults changed admin creation: %s=%v", key, adminLimits[key])
		}
	}
	for _, table := range []string{"credit_buckets", "credit_ledger", "credit_daily_grants"} {
		if count := h.count(table); count != 2 {
			t.Fatalf("administrator creation changed daily funds in %s: %d", table, count)
		}
	}
}

func TestMySQLRegistrationConcurrentDuplicateAndRollback(t *testing.T) {
	h := newRegistrationHarness(t)
	h.expect(h.settings(registrationSettingsBody(true, "101"), ""), http.StatusOK, "")
	var responses [2]*httptest.ResponseRecorder
	var group sync.WaitGroup
	for i := range responses {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			responses[i] = h.signup("concurrent-user")
		}(i)
	}
	group.Wait()
	if responses[0].Code != http.StatusCreated {
		responses[0], responses[1] = responses[1], responses[0]
	}
	h.expect(responses[0], http.StatusCreated, "")
	h.expect(responses[1], http.StatusConflict, "username_taken")
	h.expect(h.signup("CONCURRENT-USER"), http.StatusConflict, "username_taken")
	if h.count("users") != 2 || h.count("user_limits") != 2 || h.count("credit_accounts") != 1 {
		t.Fatal("duplicate registration left partial rows")
	}
	for _, table := range []string{"credit_buckets", "credit_ledger", "credit_daily_grants"} {
		if count := h.count(table); count != 1 {
			t.Fatalf("duplicate registration changed %s: %d", table, count)
		}
	}
	beforeAudits := h.count("audit_logs")
	if err := h.db.Exec(`CREATE TRIGGER reject_registration_audit BEFORE INSERT ON audit_logs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='registration audit failure'`).Error; err != nil {
		t.Fatal(err)
	}
	response := h.signup("rollback-user")
	if err := h.db.Exec("DROP TRIGGER reject_registration_audit").Error; err != nil {
		t.Fatal(err)
	}
	h.expect(response, http.StatusInternalServerError, "internal_error")
	if h.count("users") != 2 || h.count("user_limits") != 2 || h.count("credit_accounts") != 1 || h.count("audit_logs") != beforeAudits {
		t.Fatal("audit failure did not roll back user, limits, account and audit together")
	}
	for _, table := range []string{"credit_buckets", "credit_ledger", "credit_daily_grants"} {
		if count := h.count(table); count != 1 {
			t.Fatalf("audit failure did not roll back %s: %d", table, count)
		}
	}
	var attempts uint64
	if err := h.db.Table("auth_rate_limits").Select("attempts").Where("bucket_key=?", keyed(h.a, "registration-rate:username:rollback-user")).Scan(&attempts).Error; err != nil || attempts != 1 {
		t.Fatalf("failed domain transaction rolled back rate counter: %d %v", attempts, err)
	}
	h.expect(h.signup("rollback-user"), http.StatusCreated, "")
}

func TestMySQLRegistrationDailyGrantFailuresRollback(t *testing.T) {
	h := newRegistrationHarness(t)
	h.expect(h.settings(registrationSettingsBody(true, "101"), ""), http.StatusOK, "")
	beforeAudits := h.count("audit_logs")
	for _, table := range []string{"credit_buckets", "credit_ledger", "credit_daily_grants"} {
		t.Run(table, func(t *testing.T) {
			if err := h.db.Exec("CREATE TRIGGER reject_registration_grant BEFORE INSERT ON " + table + " FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='registration grant failure'").Error; err != nil {
				t.Fatal(err)
			}
			response := h.signup("grant-failure-" + table)
			if err := h.db.Exec("DROP TRIGGER reject_registration_grant").Error; err != nil {
				t.Fatal(err)
			}
			h.expect(response, http.StatusInternalServerError, "internal_error")
			for _, empty := range []string{"credit_accounts", "credit_buckets", "credit_ledger", "credit_daily_grants"} {
				if count := h.count(empty); count != 0 {
					t.Fatalf("%s failure left %d rows in %s", table, count, empty)
				}
			}
			if h.count("users") != 1 || h.count("user_limits") != 1 || h.count("audit_logs") != beforeAudits {
				t.Fatal("grant failure did not roll back registration")
			}
		})
	}
	h.expect(h.signup("grant-recovered"), http.StatusCreated, "")
}

func TestMySQLRegistrationZeroDailyAllowance(t *testing.T) {
	h := newRegistrationHarness(t)
	if err := h.db.Exec("UPDATE credit_policy_versions SET daily_allowance_pt=0 WHERE id=1").Error; err != nil {
		t.Fatal(err)
	}
	if err := h.db.Exec(`UPDATE policy_versions SET config=JSON_SET(config,'$.daily_allowance_pt','0.000000') WHERE id=101`).Error; err != nil {
		t.Fatal(err)
	}
	h.expect(h.settings(registrationSettingsBody(true, "101"), ""), http.StatusOK, "")
	h.expect(h.signup("zero-allowance"), http.StatusCreated, "")
	if h.count("credit_accounts") != 1 || h.count("credit_buckets") != 1 || h.count("credit_daily_grants") != 1 || h.count("credit_ledger") != 0 {
		t.Fatal("zero allowance must record the daily cycle without a zero-valued ledger entry")
	}
	var bucket platform.Row
	if err := h.db.Table("credit_buckets").Take(&bucket).Error; err != nil {
		t.Fatal(err)
	}
	if platform.Text(bucket["granted_pt"]) != "0.000000" || platform.Text(bucket["remaining_pt"]) != "0.000000" || platform.Text(bucket["source_type"]) != "daily" {
		t.Fatalf("unsafe zero allowance: %#v", bucket)
	}
}

func TestMySQLRegistrationPolicyInvalidationFailsClosed(t *testing.T) {
	h := newRegistrationHarness(t)
	h.expect(h.settings(registrationSettingsBody(true, "101"), ""), http.StatusOK, "")
	for _, change := range []platform.Row{
		{"status": "draft"},
		{"status": "retired"},
		{"effective_at": h.a.Time().Add(24 * time.Hour)},
		{"config": `{}`},
	} {
		var original platform.Row
		if err := h.db.Table("policy_versions").Where("id=101").Take(&original).Error; err != nil {
			t.Fatal(err)
		}
		if err := h.db.Table("policy_versions").Where("id=101").Updates(change).Error; err != nil {
			t.Fatal(err)
		}
		response := h.guest.call(http.MethodGet, "/auth/registration", "", nil)
		h.expect(response, http.StatusOK, "")
		var body struct {
			Data struct {
				Enabled bool `json:"enabled"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Data.Enabled {
			t.Fatalf("invalid policy advertised registration: %s %v", response.Body.String(), err)
		}
		h.expect(h.signup("policy-invalid-user"), http.StatusForbidden, "registration_disabled")
		h.expect(h.settings(registrationSettingsBody(true, "101"), ""), http.StatusUnprocessableEntity, "validation_failed")
		if h.count("users") != 1 || h.count("user_limits") != 1 || h.count("credit_accounts") != 0 {
			t.Fatal("invalid policy left partial registration")
		}
		restore := platform.Row{}
		for key := range change {
			restore[key] = original[key]
		}
		if err := h.db.Table("policy_versions").Where("id=101").Updates(restore).Error; err != nil {
			t.Fatal(err)
		}
	}
	h.expect(h.signup("policy-valid-user"), http.StatusCreated, "")
}

func TestMySQLRegistrationRevalidatesPolicyInsideTransaction(t *testing.T) {
	h := newRegistrationHarness(t)
	h.expect(h.settings(registrationSettingsBody(true, "101"), ""), http.StatusOK, "")
	// Invalidate the real policy after the public precheck and hashing, immediately
	// before the transaction obtains its settings snapshot. No response is mocked.
	var changed bool
	var updateError error
	const callback = "test:invalidate-registration-policy"
	if err := h.db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if !changed && tx.Statement.Table == "registration_settings" && tx.Statement.Clauses["FOR"].Expression != nil {
			changed = true
			updateError = h.db.Exec("UPDATE policy_versions SET status='retired' WHERE id=101").Error
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.db.Callback().Query().Remove(callback) })
	h.expect(h.signup("policy-race-user"), http.StatusForbidden, "registration_disabled")
	if !changed || updateError != nil {
		t.Fatalf("policy invalidation did not run: changed=%v error=%v", changed, updateError)
	}
	if h.count("users") != 1 || h.count("user_limits") != 1 || h.count("credit_accounts") != 0 {
		t.Fatal("transaction failed to revalidate the invalidated policy")
	}
}

func TestMySQLRegistrationCSRFAndPersistentRateLimits(t *testing.T) {
	h := newRegistrationHarness(t)
	if remaining := h.a.Time().Truncate(15 * time.Minute).Add(15 * time.Minute).Sub(h.a.Time()); remaining < 20*time.Second {
		t.Skip("fixed rate window is about to roll over")
	}
	body := `{"username":"limited-user","password":"registration-password"}`
	for _, headers := range []map[string]string{
		{"X-CSRF-Token": ""},
		{"Origin": "https://evil.example"},
		{"Sec-Fetch-Site": "cross-site"},
	} {
		h.expect(h.guest.call(http.MethodPost, "/auth/register", body, headers), http.StatusForbidden, "csrf_failed")
	}
	var registrationBuckets int64
	if err := h.db.Table("auth_rate_limits").Where("bucket_key IN ?", [][]byte{keyed(h.a, "registration-rate:username:limited-user"), keyed(h.a, "registration-rate:ip:192.0.2.17")}).Count(&registrationBuckets).Error; err != nil || registrationBuckets != 0 {
		t.Fatalf("CSRF rejection reached rate-limit/domain handler: %d %v", registrationBuckets, err)
	}
	for range 10 {
		h.expect(h.guest.call(http.MethodPost, "/auth/register", body, nil), http.StatusForbidden, "registration_disabled")
	}
	h.expect(h.settings(registrationSettingsBody(true, "101"), ""), http.StatusOK, "")
	response := h.guest.call(http.MethodPost, "/auth/register", body, nil)
	h.expect(response, http.StatusTooManyRequests, "rate_limited")
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("rate limit omitted Retry-After")
	}
	if h.count("users") != 1 || h.count("credit_accounts") != 0 {
		t.Fatal("rate-limited request created an account")
	}
	// Varying usernames cannot bypass the separately committed IP dimension.
	h.expect(h.settings(registrationSettingsBody(false, nil), ""), http.StatusOK, "")
	for i := 11; i < 50; i++ {
		h.expect(h.signup("ip-limited-"+strconv.Itoa(i)), http.StatusForbidden, "registration_disabled")
	}
	h.expect(h.signup("ip-limit-exceeded"), http.StatusTooManyRequests, "rate_limited")
}
