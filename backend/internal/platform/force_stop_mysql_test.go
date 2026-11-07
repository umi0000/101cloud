package platform

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

type forceStopTransport func(*http.Request) (*http.Response, error)

func (f forceStopTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func forceStopPlatformDB(t *testing.T) (*App, Job) {
	t.Helper()
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated force-stop regressions")
	}
	cfg, err := mysqlDriver.ParseDSN(dsn)
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
	name := "ci_platform_stop_" + hex.EncodeToString(nonce[:])
	if _, err = owner.ExecContext(t.Context(), "CREATE DATABASE `"+name+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Exec("DROP DATABASE `" + name + "`") })
	cfg.DBName = name
	db, err := OpenDB(Config{DSN: cfg.FormatDSN()})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	for table, definition := range map[string]string{
		"tasks":      "id BIGINT AUTO_INCREMENT PRIMARY KEY,task_type VARCHAR(64),user_id BIGINT,node_id BIGINT,instance_id BIGINT,status VARCHAR(24),phase VARCHAR(128),parameters_redacted JSON,result_redacted JSON,error_code VARCHAR(64),started_at DATETIME(6),finished_at DATETIME(6),lease_owner VARCHAR(128),lease_generation BIGINT DEFAULT 0,lease_until DATETIME(6),revision BIGINT DEFAULT 1,idempotency_scope VARCHAR(64),idempotency_key VARCHAR(128),is_instance_mutation BOOL,priority INT",
		"task_steps": "id BIGINT AUTO_INCREMENT PRIMARY KEY,task_id BIGINT,step_key VARCHAR(128),attempt BIGINT,status VARCHAR(24),error_code VARCHAR(64),operation_id VARCHAR(255),node_id BIGINT,project_name VARCHAR(64),parameters_redacted JSON,result_redacted JSON,started_at DATETIME(6),finished_at DATETIME(6),UNIQUE(task_id,step_key,attempt)",
		"nodes":      "id BIGINT PRIMARY KEY,agent_endpoint VARCHAR(255),token_ciphertext BLOB,server_cert_sha256 BLOB,default_project_name VARCHAR(64)",
		"audit_logs": "id BIGINT AUTO_INCREMENT PRIMARY KEY,event_type VARCHAR(64),resource_type VARCHAR(64),resource_id BIGINT,actor_user_id BIGINT,task_id BIGINT,result VARCHAR(24),result_redacted JSON,occurred_at DATETIME(6)",
	} {
		if err = db.Exec("CREATE TABLE `" + table + "` (" + definition + ") ENGINE=InnoDB").Error; err != nil {
			t.Fatal(err)
		}
	}
	a := New(db, Config{MasterKey: []byte("0123456789abcdef0123456789abcdef"), NodeAllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	token, err := a.Encrypt([]byte("test-token"))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Table("nodes").Create(Row{"id": 1, "agent_endpoint": "https://127.0.0.1", "token_ciphertext": token, "default_project_name": "default"}).Error; err != nil {
		t.Fatal(err)
	}
	queued, err := Enqueue(db, "regression.force-stop", 0, 1, 0, false, Row{})
	if err != nil {
		t.Fatal(err)
	}
	j := Job{ID: Uint(queued["task_id"]), Type: "regression.force-stop", NodeID: 1, LeaseGeneration: 1}
	if err = db.Table("tasks").Where("id=?", j.ID).Updates(Row{"status": "running", "lease_owner": "worker", "lease_generation": 1, "lease_until": a.Time().Add(time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	return a, j
}

func forceStopCacheTransport(a *App, transport forceStopTransport) {
	key := "https://127.0.0.1::" + hex.EncodeToString(a.sign([]byte("test-token")))
	a.agentClients.Store(key, &http.Client{Transport: transport})
}

func forceStopRevoke(t *testing.T, a *App, j Job) {
	t.Helper()
	if err := a.DB.Table("tasks").Where("id=?", j.ID).Updates(Row{"status": "cancelled", "error_code": "task_force_stopped", "finished_at": a.Time(), "lease_owner": nil, "lease_until": nil, "lease_generation": 2, "result_redacted": `{"force_stop":{"orchestration_stopped":true,"external_state":"requires_reconciliation","requires_reconciliation":true}}`}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestMySQLForceStopBeforeExecuteAndLateWrites(t *testing.T) {
	a, j := forceStopPlatformDB(t)
	if err := a.Step(t.Context(), j, "native", "/1.0/operations/existing?project=default", "waiting"); err != nil {
		t.Fatal(err)
	}
	a.RegisterJob(j.Type, func(context.Context, *App, Job) error {
		t.Error("revoked execution entered handler")
		return nil
	})
	forceStopRevoke(t, a, j)
	a.CancelTaskExecutions(j.ID)
	a.execute(t.Context(), j, "worker")
	if err := a.Step(t.Context(), j, "native", "", "succeeded"); err == nil {
		t.Fatal("late step completion was accepted")
	}
	if err := a.TaskResult(t.Context(), j, Row{"late": true}); err == nil {
		t.Fatal("late task result was accepted")
	}
	if err := a.finishTask(t.Context(), j, "worker", "succeeded", a.Time(), nil, nil); err == nil {
		t.Fatal("late task finish was accepted")
	}
	stored, err := Get(a.DB, "tasks", j.ID, false)
	if err != nil || Text(stored["status"]) != "cancelled" || !strings.Contains(Text(stored["result_redacted"]), "force_stop") {
		t.Fatalf("late worker changed cancellation: %+v %v", stored, err)
	}
	var step Row
	if err = a.DB.Table("task_steps").Where("task_id=?", j.ID).Take(&step).Error; err != nil || Text(step["operation_id"]) != "/1.0/operations/existing?project=default" {
		t.Fatalf("operation evidence was lost: %+v %v", step, err)
	}
}

func TestMySQLForceStopUnblocksImplicitNativeDispatch(t *testing.T) {
	a, j := forceStopPlatformDB(t)
	entered := make(chan struct{})
	forceStopCacheTransport(a, func(r *http.Request) (*http.Response, error) {
		if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	a.RegisterJob(j.Type, func(ctx context.Context, a *App, j Job) error {
		_, err := a.Agent(ctx, j.NodeID, http.MethodPost, "/1.0/instances?project=default", Row{})
		return err
	})
	done := make(chan struct{})
	go func() { a.execute(t.Context(), j, "worker"); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("implicit native mutation did not dispatch")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var id uint64
		return tx.Raw("SELECT id FROM tasks WHERE id=? FOR UPDATE", j.ID).Scan(&id).Error
	}); err != nil {
		t.Fatalf("submitted HTTP retained its task lock while waiting for response: %v", err)
	}
	forceStopRevoke(t, a, j)
	a.CancelTaskExecutions(j.ID)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("execution ignored force-stop")
	}
	stored, err := Get(a.DB, "tasks", j.ID, false)
	if err != nil || Text(stored["status"]) != "cancelled" || Uint(stored["lease_generation"]) != 2 {
		t.Fatalf("postcommit cancellation changed revoked task: %+v %v", stored, err)
	}
	var step Row
	if err = a.DB.Table("task_steps").Where("task_id=?", j.ID).Take(&step).Error; err != nil || !strings.Contains(Text(step["result_redacted"]), "native_dispatch_started") {
		t.Fatalf("implicit mutation omitted durable intent: %+v %v", step, err)
	}
	stale, _, unregister := a.registerTaskExecution(t.Context(), j)
	defer unregister()
	if _, err = a.Agent(stale, j.NodeID, http.MethodPut, "/1.0/instances/test/state", Row{}); err == nil {
		t.Fatal("implicit mutation dispatched after lease revocation")
	}
}

func TestMySQLForceStopPreservesOperationReturnedDuringCancellation(t *testing.T) {
	a, j := forceStopPlatformDB(t)
	ctx, _, unregister := a.registerTaskExecution(t.Context(), j)
	defer unregister()
	if err := a.Step(ctx, j, "native.named", "", "running"); err != nil {
		t.Fatal(err)
	}
	submitted, response := make(chan struct{}), make(chan struct{})
	forceStopCacheTransport(a, func(r *http.Request) (*http.Response, error) {
		if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		close(submitted)
		<-response
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"operation":"/1.0/operations/received"}`)), Request: r}, nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := a.Agent(WithTaskDispatch(ctx, j, "native.named"), j.NodeID, http.MethodPost, "/1.0/instances?project=default", Row{})
		done <- err
	}()
	select {
	case <-submitted:
	case <-time.After(5 * time.Second):
		t.Fatal("request not submitted")
	}
	forceStopRevoke(t, a, j)
	a.CancelTaskExecutions(j.ID)
	close(response)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late response did not finish")
	}
	var step Row
	if err := a.DB.Table("task_steps").Where("task_id=? AND step_key='native.named'", j.ID).Take(&step).Error; err != nil || Text(step["operation_id"]) != "/1.0/operations/received?project=default" || !strings.Contains(Text(step["result_redacted"]), "native_dispatch_started") {
		t.Fatalf("cancellation lost returned operation/intent: %+v %v", step, err)
	}
	task, err := Get(a.DB, "tasks", j.ID, false)
	if err != nil || Text(task["status"]) != "cancelled" || Uint(task["lease_generation"]) != 2 {
		t.Fatalf("late response revived task: %+v %v", task, err)
	}
}

func TestMySQLTaskInitiatorAndAfterCommitHooks(t *testing.T) {
	a, _ := forceStopPlatformDB(t)
	for _, reject := range []bool{false, true} {
		called := false
		var id uint64
		router := gin.New()
		router.Use(func(c *gin.Context) { c.Set("user", User{ID: 42}); c.Next() }, a.Mutations())
		router.PUT("/mutation", func(c *gin.Context) {
			result, err := Enqueue(DB(c, a), "regression.initiator", 7, 0, 0, false, Row{"initiated_by_user_id": "7"})
			if err != nil {
				t.Error(err)
				Fail(c, err)
				return
			}
			id = Uint(result["task_id"])
			c.Set("after_commit", []func(){func() {
				called = true
				stored, err := Get(a.DB, "tasks", id, false)
				var params Row
				if err != nil || json.Unmarshal([]byte(Text(stored["parameters_redacted"])), &params) != nil || params["initiated_by_user_id"] != "42" {
					t.Errorf("hook ran before commit or untrusted initiator persisted: %+v %v", stored, err)
				}
			}})
			if reject {
				Fail(c, E(409, "invalid_state", "reject"))
				return
			}
			c.JSON(200, Row{"data": result})
		})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/mutation", strings.NewReader(`{}`)))
		if called == reject || (!reject && response.Code != 200) || (reject && response.Code != 409) {
			t.Fatalf("commit hook outcome: rejected=%t called=%t status=%d", reject, called, response.Code)
		}
		if reject {
			if _, err := Get(a.DB, "tasks", id, false); err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("rejected mutation persisted: %v", err)
			}
		}
	}
}
