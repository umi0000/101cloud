package billing_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"101cloud/backend/internal/auth"
	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/platform"
	migrations "101cloud/backend/sql"
	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
)

// Exercise production cookie admission, not an injected Current user. This is
// independent of internal aggregation fixtures so auth's billing dependency
// cannot create an import cycle.
func TestMySQLBillingReportRealSessionAdmission(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated reporting admission")
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
	name := "ci_report_admission_" + hex.EncodeToString(nonce[:])
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
	for _, statement := range []string{
		`INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'report-admin','unused-hash','admin','active'),(2,'report-owner','unused-hash','user','active'),(3,'report-no-wallet','unused-hash','user','active')`,
		`INSERT INTO credit_policy_versions(id,code,version,daily_allowance_pt,bonus_cap_pt,timezone,reset_time,billing_window_seconds,status,effective_at,created_by_user_id,daily_validity_seconds,shutdown_grace_seconds,grace_charge_mode,restore_action,storage_charge_mode,offline_charge_mode) VALUES(1,'report-credit','1',0,0,'Asia/Shanghai','06:30:00',60,'active','2000-01-01',1,86400,600,'waived','cancel_pending_stop','running_only','defer')`,
		`INSERT INTO credit_accounts(id,user_id,credit_policy_version_id,status) VALUES(1,2,1,'active')`,
	} {
		if err = db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	tokens := make(map[uint64]string)
	for _, id := range []uint64{2, 3} {
		var secret [32]byte
		if _, err = rand.Read(secret[:]); err != nil {
			t.Fatal(err)
		}
		token := base64.RawURLEncoding.EncodeToString(secret[:])
		tokens[id] = token
		hash := sha256.Sum256([]byte(token))
		if err = db.Exec("INSERT INTO user_sessions(user_id,token_hash,expires_at) VALUES(?,?,?)", id, hash[:], time.Now().Add(time.Hour)).Error; err != nil {
			t.Fatal(err)
		}
	}
	a := platform.New(db, platform.Config{Origin: "http://report.local"})
	r := gin.New()
	r.Use(platform.RequestMiddleware())
	member := r.Group("/api/v1", auth.Middleware(a), auth.CSRF(a))
	billing.Register(a, member, r.Group("/api/v1/admin", platform.Admin()))
	call := func(path, token string, want int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil).WithContext(ctx)
		if token != "" {
			req.AddCookie(&http.Cookie{Name: "101cloud_session", Value: token})
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s status=%d want=%d body=%s", path, w.Code, want, w.Body.String())
		}
		if want == 200 && w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("member report cache policy missing")
		}
	}
	for _, path := range []string{"/billing/overview", "/billing/trend?period=year", "/billing/instances?period=year"} {
		call(path, "", 401)
		call(path, "malformed", 401)
		call(path, tokens[2], 200)
		call(path, tokens[3], 409)
	}
	call("/billing/overview?user_id=3", tokens[2], 422)
	call("/billing/trend?period=year&instance_id=1", tokens[2], 404)
	if err = db.Exec("UPDATE user_sessions SET revoked_at=? WHERE user_id=2", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	call("/billing/overview", tokens[2], 401)
	if err = db.Exec("UPDATE users SET status='disabled' WHERE id=3").Error; err != nil {
		t.Fatal(err)
	}
	call("/billing/overview", tokens[3], 401)
}
