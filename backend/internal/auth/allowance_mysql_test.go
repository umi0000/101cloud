package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

func TestMySQLAdminUserCreationRequiresExplicitPublishedAllowance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := isolatedMySQL(t, ctx)
	a := platform.New(db, platform.Config{})
	if initial, err := EnsureAdministrator(ctx, a); err != nil || initial == nil {
		t.Fatalf("unconfigured bootstrap failed: %v", err)
	}
	for _, statement := range []string{
		`INSERT INTO credit_policy_versions(id,code,version,daily_allowance_pt,bonus_cap_pt,timezone,reset_time,billing_window_seconds,status,effective_at,created_by_user_id,daily_validity_seconds,shutdown_grace_seconds,grace_charge_mode,restore_action,storage_charge_mode,offline_charge_mode) VALUES(1,'selected-allowance','1',7,0,'Asia/Shanghai','06:30:00',60,'active','2000-01-01',1,86400,600,'waived','cancel_pending_stop','running_only','defer')`,
		`INSERT INTO policy_versions(id,kind,code,version,source_id,config,status,effective_at,created_by_user_id) VALUES(101,'credit','selected-allowance','1',1,JSON_OBJECT('daily_allowance_pt','7.000000','timezone','Asia/Shanghai','grant_time','06:30:00','daily_validity_seconds',86400,'billing_window_seconds',60,'shutdown_grace_seconds',600,'grace_charge_mode','waived','restore_action','cancel_pending_stop','storage_charge_mode','running_only','offline_charge_mode','defer'),'active','2000-01-01',1)`,
		`INSERT INTO policy_versions(id,kind,code,version,config,status,effective_at,created_by_user_id) VALUES(102,'metrics','not-an-allowance','1',JSON_OBJECT('raw_retention_days',7,'rollup_retention_days',30,'bucket_seconds',60,'collection_interval_seconds',5),'active','2000-01-01',1)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	router := gin.New()
	router.Use(platform.RequestMiddleware())
	admin := router.Group("/api/v1/admin", func(c *gin.Context) {
		c.Set("user", platform.User{ID: 1, Role: "admin"})
		c.Next()
	}, platform.Admin(), a.Mutations())
	registerAdmin(a, admin)
	call := func(policy any, key string) *httptest.ResponseRecorder {
		body := map[string]any{"username": "explicit-user", "role": "user", "initial_password": "explicit-user-password"}
		if policy != nil {
			body["credit_policy_version_id"] = policy
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", strings.NewReader(string(encoded))).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	for _, tc := range []struct {
		name   string
		policy any
		status int
	}{
		{"missing", nil, 422}, {"source_not_public_id", "1", 422}, {"wrong_kind", "102", 422},
	} {
		response := call(tc.policy, tc.name)
		if response.Code != tc.status {
			t.Fatalf("%s: status=%d body=%s", tc.name, response.Code, response.Body.String())
		}
		var users, limits, wallets int64
		if err := db.Table("users").Count(&users).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Table("user_limits").Count(&limits).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Table("credit_accounts").Count(&wallets).Error; err != nil {
			t.Fatal(err)
		}
		if users != 1 || limits != 1 || wallets != 0 {
			t.Fatalf("rejected allowance left partial user/limits/wallet: %d/%d/%d", users, limits, wallets)
		}
	}
	for _, status := range []string{"draft", "retired", "active"} {
		effective := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		if status == "active" {
			effective = a.Time().Add(24 * time.Hour)
		}
		if err := db.Table("policy_versions").Where("id=101").Updates(platform.Row{"status": status, "effective_at": effective}).Error; err != nil {
			t.Fatal(err)
		}
		if response := call("101", "not-published-"+status); response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s allowance accepted: %d %s", status, response.Code, response.Body.String())
		}
	}
	if err := db.Exec("UPDATE policy_versions SET status='active',effective_at='2000-01-01' WHERE id=101").Error; err != nil {
		t.Fatal(err)
	}
	response := call("101", "create-explicit-user")
	if response.Code != http.StatusCreated || response.Header().Get("ETag") == "" || response.Header().Get("Location") == "" {
		t.Fatalf("explicit create: %d %s", response.Code, response.Body.String())
	}
	var user platform.Row
	if err := db.Table("users").Where("username='explicit-user'").Take(&user).Error; err != nil {
		t.Fatal(err)
	}
	var wallets []platform.Row
	if err := db.Table("credit_accounts").Find(&wallets).Error; err != nil {
		t.Fatal(err)
	}
	if len(wallets) != 1 || platform.Uint(wallets[0]["user_id"]) != platform.Uint(user["id"]) || platform.Uint(wallets[0]["credit_policy_version_id"]) != 1 {
		t.Fatalf("wallet must use selected public allowance's source, bootstrap stays unconfigured: %#v", wallets)
	}
	for _, table := range []string{"credit_buckets", "credit_daily_grants", "credit_ledger"} {
		var count int64
		if err := db.Table(table).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("user create granted %s: count=%d error=%v", table, count, err)
		}
	}
	replay := call("101", "create-explicit-user")
	var originalData, replayData struct {
		Data struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &originalData); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayData); err != nil {
		t.Fatal(err)
	}
	if replay.Code != response.Code || replayData.Data != originalData.Data || replayData.Data.ID != platform.Text(user["id"]) {
		t.Fatalf("user create replay changed: %d %s", replay.Code, replay.Body.String())
	}
}
