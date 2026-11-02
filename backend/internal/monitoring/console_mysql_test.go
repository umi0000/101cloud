package monitoring

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

// Connection-local tables exercise the real consumers without changing persistent fixtures.
func consoleTestService(t *testing.T) (*consoleService, consoleSession) {
	t.Helper()
	dsn := os.Getenv("CLOUD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_DSN for console consumer regressions")
	}
	db, err := platform.OpenDB(platform.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	definitions := map[string]string{
		"users":            "id BIGINT PRIMARY KEY,status VARCHAR(24),role VARCHAR(24),revision BIGINT DEFAULT 1",
		"user_sessions":    "id BIGINT PRIMARY KEY,user_id BIGINT,revoked_at DATETIME(6),expires_at DATETIME(6),created_at DATETIME(6)",
		"instances":        "id BIGINT PRIMARY KEY,user_id BIGINT,node_id BIGINT,observed_state VARCHAR(24),provisioning_state VARCHAR(24),deleted_at DATETIME(6),product_type VARCHAR(24),allowed_user_actions JSON,image_type VARCHAR(24),revision BIGINT DEFAULT 1",
		"console_sessions": "id CHAR(43) PRIMARY KEY,user_id BIGINT,auth_session_id BIGINT,instance_id BIGINT,node_id BIGINT,instance_owner_id BIGINT,is_admin BOOLEAN,kind VARCHAR(16),status VARCHAR(16),cols INT,`rows` INT,operation_path VARCHAR(255),secrets_ciphertext BLOB,prepare_task_id BIGINT,prepare_started_at DATETIME(6),connected_at DATETIME(6),closed_at DATETIME(6),revision BIGINT,created_at DATETIME(6),updated_at DATETIME(6),KEY ix_console_user_status(user_id,status)",
		"tasks":            "id BIGINT AUTO_INCREMENT PRIMARY KEY,task_type VARCHAR(64),idempotency_scope VARCHAR(64),idempotency_key VARCHAR(64),user_id BIGINT,node_id BIGINT,instance_id BIGINT,is_instance_mutation BOOLEAN,status VARCHAR(24),priority INT,parameters_redacted JSON",
		"audit_logs":       "id BIGINT AUTO_INCREMENT PRIMARY KEY,event_type VARCHAR(128),resource_type VARCHAR(64),resource_id BIGINT,actor_user_id BIGINT,task_id BIGINT,result VARCHAR(24),result_redacted JSON,occurred_at DATETIME(6)",
	}
	for table, definition := range definitions {
		if err = tx.Exec("CREATE TEMPORARY TABLE `" + table + "` (" + definition + ") ENGINE=InnoDB").Error; err != nil {
			t.Fatal(err)
		}
	}
	a := platform.New(tx, platform.Config{Origin: "https://cloud.example", SessionTTL: 12 * time.Hour, ConsoleMaxConcurrent: 4})
	now := a.Time()
	for table, row := range map[string]platform.Row{
		"users":         {"id": 1, "status": "active", "role": "user"},
		"user_sessions": {"id": 2, "user_id": 1, "created_at": now.Add(-time.Hour), "expires_at": now.Add(11 * time.Hour)},
		"instances":     {"id": 3, "user_id": 1, "node_id": 4, "observed_state": "running", "provisioning_state": "ready", "product_type": "custom", "allowed_user_actions": `["console"]`, "image_type": "container"},
	} {
		if err = tx.Table(table).Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	row := consoleSession{ID: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), UserID: 1, AuthSessionID: 2, InstanceID: 3, NodeID: 4, InstanceOwnerID: 1, Kind: "terminal", Status: "ready", Cols: 80, Rows: 24, Revision: 1, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	if err = tx.Table("console_sessions").Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return &consoleService{a: a, bridges: make(map[string]*consoleBridge)}, row
}

func consoleTestContext(row consoleSession, method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "session_id", Value: row.ID}, {Key: "id", Value: fmt.Sprint(row.InstanceID)}}
	c.Set("user", platform.User{ID: row.UserID, SessionID: row.AuthSessionID})
	return c, recorder
}

func TestMySQLConsoleOlderThanTenMinutesRemainsAuthorized(t *testing.T) {
	s, row := consoleTestService(t)
	if err := s.authorized(context.Background(), row, true); err != nil {
		t.Fatalf("old console with valid login rejected: %v", err)
	}
	c, _ := consoleTestContext(row, http.MethodGet, "/console-sessions/"+row.ID, "")
	value, err := s.status(c)
	if err != nil {
		t.Fatal(err)
	}
	response := value.(map[string]any)
	if response["status"] != "ready" {
		t.Fatalf("old ready console changed status: %v", response)
	}
	if _, exists := response["expires_at"]; exists {
		t.Fatal("public console response still carries a fixed expiry")
	}
	if err := s.revoke(context.Background(), s.a); err != nil {
		t.Fatal(err)
	}
	var saved consoleSession
	if err := s.a.DB.Table("console_sessions").Where("id=?", row.ID).Take(&saved).Error; err != nil || saved.Status != "ready" {
		t.Fatalf("auth sweep closed a valid old console: status=%s error=%v", saved.Status, err)
	}
}

func TestMySQLConsoleAdmissionCountsOldOpenSessions(t *testing.T) {
	s, row := consoleTestService(t)
	for i, status := range []string{"preparing", "active", "ready", "expired", "closed", "failed"} {
		copy := row
		copy.ID = base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%032d", i+1)))
		copy.Status = status
		if err := s.a.DB.Table("console_sessions").Create(&copy).Error; err != nil {
			t.Fatal(err)
		}
	}
	c, _ := consoleTestContext(row, http.MethodPost, "/instances/3/console-sessions", `{"kind":"terminal","cols":80,"rows":24}`)
	_, err := s.create(c)
	api, ok := err.(*platform.HTTPError)
	if !ok || api.Code != "console_limit" {
		t.Fatalf("four old open sessions did not enforce quota: %v", err)
	}
	c, _ = consoleTestContext(row, http.MethodDelete, "/console-sessions/"+row.ID, "")
	c.Request.Header.Set("If-Match", `"rev-1"`)
	if _, err := s.remove(c); err != nil {
		t.Fatal(err)
	}
	c, recorder := consoleTestContext(row, http.MethodPost, "/instances/3/console-sessions", `{"kind":"terminal","cols":80,"rows":24}`)
	if _, err := s.create(c); err != nil || recorder.Code != http.StatusCreated {
		t.Fatalf("closed and historical rows consumed quota: status=%d error=%v", recorder.Code, err)
	}
}

func consoleTestBridge() (*consoleBridge, func()) {
	data, dataPeer := net.Pipe()
	control, controlPeer := net.Pipe()
	_, cancel := context.WithCancel(context.Background())
	bridge := &consoleBridge{data: &consoleWire{body: data}, control: &consoleWire{body: control}, done: make(chan struct{}), cancel: cancel}
	return bridge, func() { bridge.close(1000, "test complete"); dataPeer.Close(); controlPeer.Close() }
}

func TestMySQLConsoleGuardClosesOnRevocation(t *testing.T) {
	for _, reason := range []string{"logout", "login_expired", "user_disabled", "permission_revoked", "instance_stopped", "admin_demoted"} {
		t.Run(reason, func(t *testing.T) {
			s, row := consoleTestService(t)
			var result *gorm.DB
			switch reason {
			case "logout":
				result = s.a.DB.Table("user_sessions").Where("id=2").Update("revoked_at", s.a.Time())
			case "login_expired":
				result = s.a.DB.Table("user_sessions").Where("id=2").Update("expires_at", s.a.Time().Add(-time.Second))
			case "user_disabled":
				result = s.a.DB.Table("users").Where("id=1").Update("status", "disabled")
			case "permission_revoked":
				result = s.a.DB.Table("instances").Where("id=3").Update("allowed_user_actions", `[]`)
			case "instance_stopped":
				result = s.a.DB.Table("instances").Where("id=3").Update("observed_state", "stopped")
			case "admin_demoted":
				row.IsAdmin = true
				result = s.a.DB.Table("console_sessions").Where("id=?", row.ID).Update("is_admin", true)
			}
			if result.Error != nil {
				t.Fatal(result.Error)
			}
			bridge, cleanup := consoleTestBridge()
			defer cleanup()
			s.bridges[row.ID] = bridge
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); s.guard(ctx, row, bridge) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cancel()
				<-done
				t.Fatal("authorization guard did not close the console")
			}
			var saved consoleSession
			if err := s.a.DB.Table("console_sessions").Where("id=?", row.ID).Take(&saved).Error; err != nil || saved.Status != "closed" {
				t.Fatalf("revocation was not recorded as closed: status=%s error=%v", saved.Status, err)
			}
		})
	}
}

func TestMySQLConsoleAuthSweepPreservesHistory(t *testing.T) {
	s, row := consoleTestService(t)
	history := row
	history.ID = base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("h", 32)))
	history.Status = "expired"
	if err := s.a.DB.Table("console_sessions").Create(&history).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.a.DB.Table("user_sessions").Where("id=2").Update("revoked_at", s.a.Time()).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.revoke(context.Background(), s.a); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{row.ID: "closed", history.ID: "expired"} {
		var saved consoleSession
		if err := s.a.DB.Table("console_sessions").Where("id=?", id).Take(&saved).Error; err != nil || saved.Status != want {
			t.Fatalf("unexpected sweep state: want=%s got=%s error=%v", want, saved.Status, err)
		}
	}
}

func TestMySQLConsoleShutdownClosesOnlyOwnedBridges(t *testing.T) {
	s, row := consoleTestService(t)
	other := row
	other.ID = base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("o", 32)))
	if err := s.a.DB.Table("console_sessions").Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	bridge, cleanup := consoleTestBridge()
	defer cleanup()
	s.bridges[row.ID] = bridge
	s.shutdown()
	for id, want := range map[string]string{row.ID: "closed", other.ID: "ready"} {
		var saved consoleSession
		if err := s.a.DB.Table("console_sessions").Where("id=?", id).Take(&saved).Error; err != nil || saved.Status != want {
			t.Fatalf("shutdown crossed process ownership: want=%s got=%s error=%v", want, saved.Status, err)
		}
	}
}

func TestMySQLConsoleOldReadySessionStreamsOnce(t *testing.T) {
	s, row := consoleTestService(t)
	bridge, cleanup := consoleTestBridge()
	defer cleanup()
	bridge.frames = make(chan consoleFrame)
	bridge.result = make(chan consoleResult, 1)
	s.bridges[row.ID] = bridge
	router := gin.New()
	finished := make(chan struct{}, 2)
	router.GET("/console-sessions/:session_id/stream", func(c *gin.Context) {
		defer func() { finished <- struct{}{} }()
		c.Set("user", platform.User{ID: row.UserID, SessionID: row.AuthSessionID})
		s.stream(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	dialer := websocket.Dialer{Subprotocols: []string{"101cloud.terminal.v1"}, HandshakeTimeout: 5 * time.Second}
	target := "ws" + strings.TrimPrefix(server.URL, "http") + "/console-sessions/" + row.ID + "/stream"
	header := http.Header{"Origin": []string{s.a.Config.Origin}}
	browser, response, err := dialer.Dial(target, header)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatalf("one-hour-old ready session could not stream: %v", err)
	}
	defer browser.Close()
	var saved consoleSession
	if err := s.a.DB.Table("console_sessions").Where("id=?", row.ID).Take(&saved).Error; err != nil || saved.Status != "active" || saved.ConnectedAt == nil {
		t.Fatalf("stream was not consumed: status=%s error=%v", saved.Status, err)
	}
	second, response, err := dialer.Dial(target, header)
	if second != nil {
		second.Close()
	}
	if response != nil {
		defer response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusConflict {
		t.Fatalf("single-use stream accepted a second connection: response=%v error=%v", response, err)
	}
	<-finished // Rejected second handshake has no live bridge.
	bridge.close(1000, "test complete")
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not stop when its bridge closed")
	}
}

func TestMySQLElasticConsoleAuthorizationAppliesToAdmissionAndExistingSession(t *testing.T) {
	s, row := consoleTestService(t)
	if err := s.a.DB.Table("instances").Where("id=3").Updates(platform.Row{"product_type": "elastic", "allowed_user_actions": `[]`}).Error; err != nil {
		t.Fatal(err)
	}
	c, _ := consoleTestContext(row, http.MethodPost, "/instances/3/console-sessions", `{"kind":"terminal","cols":80,"rows":24}`)
	if _, err := s.create(c); err == nil {
		t.Fatal("elastic instance without console permission opened a new session")
	} else if api, ok := err.(*platform.HTTPError); !ok || api.Status != 403 || api.Code != "action_not_allowed" {
		t.Fatalf("unexpected denial: %v", err)
	}
	if err := s.authorized(context.Background(), row, true); err == nil {
		t.Fatal("existing elastic session retained revoked console access")
	}
	if err := s.a.DB.Table("instances").Where("id=3").Update("allowed_user_actions", `["console"]`).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.authorized(context.Background(), row, true); err != nil {
		t.Fatalf("authorized elastic console denied: %v", err)
	}
	c, response := consoleTestContext(row, http.MethodPost, "/instances/3/console-sessions", `{"kind":"terminal","cols":80,"rows":24}`)
	if _, err := s.create(c); err != nil || response.Code != http.StatusCreated {
		t.Fatalf("authorized elastic console admission failed: status=%d error=%v", response.Code, err)
	}
}
