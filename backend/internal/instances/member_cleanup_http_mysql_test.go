package instances

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"101cloud/backend/internal/auth"
	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Exercise the production session, CSRF and transactional mutation middleware
// over loopback HTTP, rather than injecting a trusted user into each request.
type memberCleanupHTTP struct {
	f      *planConfigurationFixture
	server *httptest.Server
	client *http.Client
	csrf   string
}

func newMemberCleanupHTTP(t *testing.T, f *planConfigurationFixture) *memberCleanupHTTP {
	t.Helper()
	// The admission fixture already registered its own workers. The real HTTP
	// surface needs a separate application registry over the same isolated DB.
	f.a = platform.New(f.db, f.a.Config)
	r := gin.New()
	r.Use(platform.RequestMiddleware())
	api := r.Group("/api/v1", auth.CSRF(f.a))
	user := api.Group("", auth.Middleware(f.a))
	admin := user.Group("/admin", platform.Admin(), f.a.Mutations())
	user.Use(f.a.Mutations())
	Register(f.a, user, admin)
	auth.Register(f.a, api, user, admin)
	s := httptest.NewServer(r)
	t.Cleanup(s.Close)
	f.a.Config.Origin = s.URL
	f.a.Config.SessionTTL = time.Hour
	token := strings.Repeat("M", 43)
	hash := sha256.Sum256([]byte(token))
	f.exec("INSERT INTO user_sessions(user_id,token_hash,expires_at,created_at) VALUES(2,?,?,NOW(6))", hash[:], f.a.Time().Add(time.Hour))
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "101cloud_session", Value: token, Path: "/"}})
	h := &memberCleanupHTTP{f: f, server: s, client: &http.Client{Jar: jar}}
	response, err := h.client.Get(s.URL + "/api/v1/auth/csrf")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope platform.Row
	if err = json.NewDecoder(response.Body).Decode(&envelope); err != nil || response.StatusCode != 200 {
		t.Fatalf("CSRF setup: %v %+v", err, envelope)
	}
	h.csrf = platform.Text(object(envelope["data"])["csrf_token"])
	return h
}

func (h *memberCleanupHTTP) request(method, path, body, etag, key, csrf, origin string) (int, platform.Row) {
	h.f.t.Helper()
	req, err := http.NewRequest(method, h.server.URL+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		h.f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", etag)
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Origin", origin)
	response, err := h.client.Do(req)
	if err != nil {
		h.f.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		h.f.t.Fatal(err)
	}
	var envelope platform.Row
	if err = json.Unmarshal(raw, &envelope); err != nil {
		h.f.t.Fatalf("decode status=%d: %s", response.StatusCode, raw)
	}
	return response.StatusCode, envelope
}

func TestMySQLMemberCleanupHTTPRejectsWithoutTakeover(t *testing.T) {
	for _, scenario := range []string{"other_owner", "no_delete", "custom_no_delete", "missing_mode", "missing_risk", "missing_name", "missing_data", "wrong_name", "stale_etag", "missing_etag", "missing_csrf", "wrong_origin", "malformed_flag", "unknown_field", "retired", "deleted"} {
		t.Run(scenario, func(t *testing.T) {
			f, r, old := memberStuckDeleteFixture(t)
			f.exec("UPDATE tasks SET status='unknown',lease_generation=7 WHERE id=?", old["id"])
			h := newMemberCleanupHTTP(t, f)
			body := stuckDeleteBody(r)
			etag, csrf, origin := fmt.Sprintf(`"rev-%d"`, platform.Uint(r["revision"])), h.csrf, h.server.URL
			want, code := 422, "invalid_request"
			switch scenario {
			case "other_owner":
				f.exec("INSERT INTO users(id,username,password_hash,role,status) VALUES(3,'another-member','unused-test-hash','user','active')")
				f.exec("UPDATE user_sessions SET user_id=3 WHERE user_id=2")
				want, code = 404, "not_found"
			case "no_delete", "custom_no_delete":
				f.exec("UPDATE instances SET allowed_user_actions=JSON_ARRAY('stop') WHERE id=?", r["id"])
				if scenario == "custom_no_delete" {
					f.exec("UPDATE instances SET product_type='custom',instance_plan_id=NULL,plan_snapshot=NULL,billing_mode='admin_exempt' WHERE id=?", r["id"])
				}
				want, code = 403, "action_not_allowed"
			case "missing_mode":
				delete(body, "mode")
			case "missing_risk":
				delete(body, "confirm_data_loss_risk")
			case "missing_name":
				delete(body, "confirm_name")
			case "missing_data":
				delete(body, "delete_data")
			case "wrong_name":
				body["confirm_name"] = "not the exact name"
			case "stale_etag":
				etag, want, code = `"rev-999999"`, 412, "revision_conflict"
			case "missing_etag":
				etag, want, code = "", 428, "precondition_required"
			case "missing_csrf":
				csrf, want, code = "", 403, "csrf_failed"
			case "wrong_origin":
				origin, want, code = "http://elsewhere.invalid", 403, "csrf_failed"
			case "malformed_flag":
				body["cleanup_stuck"], code = "true", "validation_failed"
			case "unknown_field":
				body["extra"], code = true, "validation_failed"
			case "retired":
				f.exec("UPDATE instances SET creation_failed_at=NOW(6),creation_cleanup_state='unknown' WHERE id=?", r["id"])
				want, code = 404, "not_found"
			case "deleted":
				f.exec("UPDATE instances SET desired_state='deleted',observed_state='deleted',provisioning_state='deleted',deleted_at=NOW(6) WHERE id=?", r["id"])
				want, code = 404, "not_found"
			}
			before := f.instance(platform.Uint(r["id"]))
			requests := f.count("backend_requests")
			status, envelope := h.request(http.MethodDelete, fmt.Sprintf("/instances/%d", platform.Uint(r["id"])), jsonText(body), etag, "reject-cleanup", csrf, origin)
			if status != want || platform.Text(object(envelope["error"])["code"]) != code {
				t.Fatalf("status=%d want=%d: %+v", status, want, envelope)
			}
			current, err := platform.Get(f.db, "tasks", platform.Uint(old["id"]), false)
			if err != nil || platform.Text(current["status"]) != "unknown" || platform.Uint(current["lease_generation"]) != 7 || f.count("tasks") != 1 || f.count("backend_requests") != requests || platform.Text(f.instance(platform.Uint(r["id"]))["desired_state"]) != platform.Text(before["desired_state"]) {
				t.Fatalf("rejected request took over durable work: %+v %v", current, err)
			}
		})
	}
}

func TestMySQLMemberCleanupHTTPIdempotencyAndCapabilities(t *testing.T) {
	f, r, old := memberStuckDeleteFixture(t)
	f.exec("UPDATE tasks SET status='unknown' WHERE id=?", old["id"])
	f.exec("UPDATE instances SET provisioning_state='unknown',observed_state='running',observed_at=NOW(6)-INTERVAL 1 DAY WHERE id=?", r["id"])
	h := newMemberCleanupHTTP(t, f)
	path := fmt.Sprintf("/instances/%d", platform.Uint(r["id"]))
	status, detail := h.request(http.MethodGet, path, "", "", "", "", "")
	caps := object(object(detail["data"])["capabilities"])
	if status != 200 || caps["force_delete"] != true || caps["delete"] != false {
		t.Fatalf("stale/busy recovery capability: %+v", detail)
	}
	body, etag := jsonText(stuckDeleteBody(r)), fmt.Sprintf(`"rev-%d"`, platform.Uint(r["revision"]))
	status, first := h.request(http.MethodDelete, path, body, etag, "same-cleanup-intent", h.csrf, h.server.URL)
	if status != 202 {
		t.Fatalf("accept: %d %+v", status, first)
	}
	status, replay := h.request(http.MethodDelete, path, body, etag, "same-cleanup-intent", h.csrf, h.server.URL)
	if status != 202 || platform.Uint(object(first["data"])["task_id"]) != platform.Uint(object(replay["data"])["task_id"]) || f.count("tasks") != 2 {
		t.Fatalf("replay replaced original cleanup: %d %+v %+v", status, first, replay)
	}
	if f.instance(platform.Uint(r["id"]))["deleted_at"] != nil {
		t.Fatal("HTTP acceptance masqueraded as native deletion")
	}
	f.exec("UPDATE instances SET allowed_user_actions=JSON_ARRAY() WHERE id=?", r["id"])
	status, detail = h.request(http.MethodGet, path, "", "", "", "", "")
	if status != 200 || object(object(detail["data"])["capabilities"])["force_delete"] != false {
		t.Fatalf("revoked delete grant retained recovery capability: %+v", detail)
	}
}

func TestMySQLMemberCleanupLeavesOrdinaryDeleteStoppedOnly(t *testing.T) {
	for _, scenario := range []string{"running", "stale", "stopped"} {
		t.Run(scenario, func(t *testing.T) {
			f, r, old := memberStuckDeleteFixture(t)
			f.exec("UPDATE tasks SET status='succeeded',finished_at=NOW(6) WHERE id=?", old["id"])
			f.exec("UPDATE instances SET provisioning_state='ready',observed_state='stopped',observed_at=NOW(6) WHERE id=?", r["id"])
			want, code := 202, ""
			switch scenario {
			case "running":
				f.exec("UPDATE instances SET observed_state='running' WHERE id=?", r["id"])
				want, code = 409, "force_confirmation_required"
			case "stale":
				f.exec("UPDATE instances SET observed_at=NOW(6)-INTERVAL 1 DAY WHERE id=?", r["id"])
				want, code = 409, "invalid_state"
			}
			h := newMemberCleanupHTTP(t, f)
			path := fmt.Sprintf("/instances/%d", platform.Uint(r["id"]))
			body := jsonText(platform.Row{"confirm_name": r["logical_name"], "delete_data": true})
			status, envelope := h.request(http.MethodDelete, path, body, fmt.Sprintf(`"rev-%d"`, platform.Uint(r["revision"])), "ordinary-delete", h.csrf, h.server.URL)
			if status != want || (code != "" && platform.Text(object(envelope["error"])["code"]) != code) {
				t.Fatalf("ordinary deletion changed semantics: %d %+v", status, envelope)
			}
			if status == 202 {
				task, err := platform.Get(f.db, "tasks", platform.Uint(object(envelope["data"])["task_id"]), false)
				if err != nil || platform.Bool(object(task["parameters_redacted"])["cleanup_stuck"]) || platform.Bool(object(task["parameters_redacted"])["force"]) {
					t.Fatalf("ordinary deletion became force cleanup: %+v %v", task, err)
				}
			}
		})
	}
}

func TestMySQLMemberCleanupRechecksCurrentRowAfterAccountLock(t *testing.T) {
	// Ownership changes with retained tasks/usage are already prohibited by
	// composite FKs. Cross-owner HTTP denial is exercised using another session.
	for _, scenario := range []string{"permission", "retired", "deleted"} {
		t.Run(scenario, func(t *testing.T) {
			f, r, old := memberStuckDeleteFixture(t)
			f.exec("UPDATE tasks SET status='unknown' WHERE id=?", old["id"])
			h := newMemberCleanupHTTP(t, f)
			tx := f.db.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			if err := lockOwnerAccount(tx, 2); err != nil {
				t.Fatal(err)
			}
			read := make(chan struct{})
			var once sync.Once
			callback := "member-cleanup-current-row-recheck"
			if err := f.db.Callback().Query().After("gorm:query").Register(callback, func(db *gorm.DB) {
				if db.Statement.Table == "instances" {
					if _, locked := db.Statement.Clauses["FOR"]; !locked {
						once.Do(func() { close(read) })
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			defer f.db.Callback().Query().Remove(callback)
			type response struct {
				status   int
				envelope platform.Row
			}
			result := make(chan response, 1)
			go func() {
				status, envelope := h.request(http.MethodDelete, fmt.Sprintf("/instances/%d", platform.Uint(r["id"])), jsonText(stuckDeleteBody(r)), fmt.Sprintf(`"rev-%d"`, platform.Uint(r["revision"])), "current-row-race", h.csrf, h.server.URL)
				result <- response{status, envelope}
			}()
			select {
			case <-read:
			case <-time.After(10 * time.Second):
				t.Fatal("request did not read initial instance")
			}
			changes := platform.Row{}
			want, code := 404, "not_found"
			switch scenario {
			case "permission":
				changes["allowed_user_actions"] = jsonText([]string{})
				want, code = 403, "action_not_allowed"
			case "retired":
				changes["creation_failed_at"] = f.a.Time()
			case "deleted":
				changes["deleted_at"] = f.a.Time()
				changes["desired_state"] = "deleted"
				changes["observed_state"] = "deleted"
				changes["provisioning_state"] = "deleted"
			}
			if err := tx.Table("instances").Where("id=?", r["id"]).Updates(changes).Error; err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit().Error; err != nil {
				t.Fatal(err)
			}
			var got response
			select {
			case got = <-result:
			case <-time.After(10 * time.Second):
				t.Fatal("request did not resume after instance change")
			}
			if got.status != want || platform.Text(object(got.envelope["error"])["code"]) != code || f.count("tasks") != 1 {
				t.Fatalf("changed instance admitted cleanup: %+v", got)
			}
			current, err := platform.Get(f.db, "tasks", platform.Uint(old["id"]), false)
			if err != nil || platform.Text(current["status"]) != "unknown" {
				t.Fatalf("current-row rejection cancelled old task: %+v %v", current, err)
			}
		})
	}
}
