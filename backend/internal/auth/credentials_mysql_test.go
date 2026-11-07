package auth

import (
	"bytes"
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

// isolatedMySQL creates and migrates a throwaway database from
// CLOUD_TEST_MYSQL_ADMIN_DSN and drops it when the test ends.
func isolatedMySQL(t *testing.T, ctx context.Context) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated MySQL regressions")
	}
	admin, e := driver.ParseDSN(dsn)
	if e != nil {
		t.Fatal(e)
	}
	admin.DBName = ""
	owner, e := sql.Open("mysql", admin.FormatDSN())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { owner.Close() })
	var nonce [8]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		t.Fatal(e)
	}
	name := "ci_auth_" + hex.EncodeToString(nonce[:])
	if _, e = owner.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { owner.Exec("DROP DATABASE `" + name + "`") })
	admin.DBName = name
	db, e := platform.OpenDB(platform.Config{DSN: admin.FormatDSN()})
	if e != nil {
		t.Fatal(e)
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { pool.Close() })
	if e = migrations.Up(ctx, db); e != nil {
		t.Fatal(e)
	}
	return db
}

// A wrong password is a credential error, not a lost session: the frontend must
// be able to show "wrong password" without treating the user as logged out.
func TestMySQLCredentialErrorsKeepSessionAndAdminResetNeedsNoReason(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := isolatedMySQL(t, ctx)
	a := platform.New(db, platform.Config{Origin: "https://cloud.example", MasterKey: bytes.Repeat([]byte{7}, 32), SessionTTL: time.Hour})
	initial, e := EnsureAdministrator(ctx, a)
	if e != nil || initial == nil {
		t.Fatalf("create administrator: %v", e)
	}
	r := gin.New()
	api := r.Group("/api/v1", platform.RequestMiddleware(), CSRF(a))
	user := api.Group("", Middleware(a))
	admin := user.Group("/admin", platform.Admin(), a.Mutations())
	user.Use(a.Mutations())
	Register(a, api, user, admin)

	jar := map[string]string{}
	var csrf string
	do := func(method, path, body string, header map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, a.Config.Origin+"/api/v1"+path, strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Origin", a.Config.Origin)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		for k, v := range jar {
			req.AddCookie(&http.Cookie{Name: k, Value: v})
		}
		response := httptest.NewRecorder()
		r.ServeHTTP(response, req)
		for _, cookie := range response.Result().Cookies() {
			if cookie.MaxAge < 0 {
				delete(jar, cookie.Name)
			} else {
				jar[cookie.Name] = cookie.Value
			}
		}
		return response
	}
	expect := func(response *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		var envelope struct {
			Data struct {
				Token string `json:"csrf_token"`
			} `json:"data"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &envelope)
		if response.Code != status || envelope.Error.Code != code {
			t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
		}
		if envelope.Data.Token != "" {
			csrf = envelope.Data.Token
		}
	}
	login := func(username, password string) string {
		body, _ := json.Marshal(map[string]string{"username": username, "password": password})
		return string(body)
	}

	expect(do(http.MethodGet, "/auth/csrf", "", nil), 200, "")
	expect(do(http.MethodPost, "/auth/login", login(initial.Username, "definitely-not-the-password"), nil), 401, "invalid_credentials")
	expect(do(http.MethodPost, "/auth/login", login("no-such-user", "definitely-not-the-password"), nil), 401, "invalid_credentials")
	expect(do(http.MethodPost, "/auth/login", login(initial.Username, initial.Password), nil), 200, "")
	session := jar[sessionCookie(a)]
	if session == "" {
		t.Fatal("login did not set the session cookie")
	}
	me := do(http.MethodGet, "/me", "", nil)
	expect(me, 200, "")
	wrong := do(http.MethodPost, "/me/password", `{"current_password":"definitely-not-the-password","new_password":"A-brand-new-password1"}`, map[string]string{"If-Match": me.Header().Get("ETag"), "Idempotency-Key": "wrong-current-password"})
	expect(wrong, 401, "invalid_credentials")
	if jar[sessionCookie(a)] != session {
		t.Fatal("wrong current password cleared the session cookie")
	}
	expect(do(http.MethodGet, "/me", "", nil), 200, "")
	var identity struct {
		Data struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	if e = json.Unmarshal(me.Body.Bytes(), &identity); e != nil || identity.Data.User.ID == "" {
		t.Fatalf("read administrator identity: %v, %s", e, me.Body.String())
	}
	resetPath := "/admin/users/" + identity.Data.User.ID + "/reset-password"
	resetHeaders := map[string]string{"If-Match": me.Header().Get("ETag"), "Idempotency-Key": "admin-reset-without-reason"}
	expect(do(http.MethodPost, resetPath, `{"new_password":"A-brand-new-password1","reason":"legacy operation reason"}`, resetHeaders), 422, "validation_failed")
	expect(do(http.MethodPost, resetPath, `{"new_password":"A-brand-new-password1"}`, resetHeaders), 204, "")
	var audits []platform.Row
	if e = db.Table("audit_logs").Where("event_type = ?", "auth.reset-password").Find(&audits).Error; e != nil {
		t.Fatal(e)
	}
	if len(audits) != 1 || platform.Text(audits[0]["actor_user_id"]) != identity.Data.User.ID || platform.Stamp(audits[0]["occurred_at"]).IsZero() {
		t.Fatalf("reset did not retain basic audit: %v", audits)
	}
	var details map[string]json.RawMessage
	if e = json.Unmarshal([]byte(platform.Text(audits[0]["result_redacted"])), &details); e != nil {
		t.Fatal(e)
	}
	if _, exists := details["reason"]; exists {
		t.Fatalf("administrator operation reason was recorded: %s", audits[0]["result_redacted"])
	}
	expect(do(http.MethodGet, "/me", "", nil), 401, "unauthenticated")
	expect(do(http.MethodGet, "/auth/csrf", "", nil), 200, "")
	expect(do(http.MethodPost, "/auth/login", login(initial.Username, "A-brand-new-password1"), nil), 200, "")
}
