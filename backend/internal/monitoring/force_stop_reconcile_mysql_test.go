package monitoring

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

const consoleRecoveryOperation = "/1.0/operations/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func consoleRecoveryFixture(t *testing.T, handler http.HandlerFunc) (*consoleService, platform.Job, consoleSession) {
	t.Helper()
	s, session := consoleTestService(t)
	for _, statement := range []string{
		"ALTER TABLE tasks ADD error_code VARCHAR(64), ADD revision BIGINT DEFAULT 1, ADD result_redacted JSON",
		"ALTER TABLE instances ADD project_name VARCHAR(64) DEFAULT 'default'",
		"CREATE TEMPORARY TABLE task_steps (id BIGINT AUTO_INCREMENT PRIMARY KEY,task_id BIGINT,step_key VARCHAR(64),attempt BIGINT,status VARCHAR(24),operation_id VARCHAR(255),node_id BIGINT,project_name VARCHAR(64),result_redacted JSON) ENGINE=InnoDB",
		"CREATE TEMPORARY TABLE nodes (id BIGINT PRIMARY KEY,agent_endpoint VARCHAR(255),token_ciphertext BLOB,server_cert_sha256 BINARY(32)) ENGINE=InnoDB",
	} {
		if err := s.a.DB.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	s.a = platform.New(s.a.DB, platform.Config{MasterKey: make([]byte, 32), NodeAllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	pin := sha256.Sum256(server.Certificate().Raw)
	token, err := s.a.Encrypt([]byte(strings.Repeat("A", 43)))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.a.DB.Table("nodes").Create(platform.Row{"id": session.NodeID, "agent_endpoint": server.URL, "token_ciphertext": token, "server_cert_sha256": pin[:]}).Error; err != nil {
		t.Fatal(err)
	}
	for _, task := range []platform.Row{
		{"id": 10, "task_type": "monitoring.console.prepare", "user_id": session.UserID, "node_id": session.NodeID, "instance_id": session.InstanceID, "status": "cancelled", "error_code": "task_force_stopped", "lease_generation": 8, "parameters_redacted": fmt.Sprintf(`{"session_id":%q}`, session.ID), "result_redacted": `{"force_stop":{"requires_reconciliation":true,"external_state":"requires_reconciliation","orchestration_stopped":true}}`},
		{"id": 11, "task_type": "instance.reconcile", "user_id": session.UserID, "node_id": session.NodeID, "instance_id": session.InstanceID, "status": "running", "lease_generation": 1, "lease_until": s.a.Time().Add(time.Minute), "result_redacted": `{}`},
	} {
		if err = s.a.DB.Table("tasks").Create(task).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err = s.a.DB.Table("console_sessions").Where("id=?", session.ID).Updates(platform.Row{"prepare_task_id": 10, "status": "closed", "closed_at": s.a.Time()}).Error; err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	RegisterConsole(s.a, router.Group("/user"), router.Group("/admin"))
	t.Cleanup(func() { consoleServices.Delete(s.a) })
	return s, platform.Job{ID: 11, Type: "monitoring.console.prepare", UserID: session.UserID, NodeID: session.NodeID, InstanceID: session.InstanceID, LeaseGeneration: 1, Parameters: json.RawMessage(`{"source_task_id":"10"}`)}, session
}

func TestMySQLForceStoppedConsoleReadOnlyReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name                                           string
		status                                         int
		step, session, dispatch, undispatched, missing bool
		settled                                        bool
	}{
		{name: "session terminal", status: 200, session: true, settled: true},
		{name: "step failed terminal", status: 400, step: true, dispatch: true, settled: true},
		{name: "cancelled terminal", status: 401, step: true, session: true, dispatch: true, settled: true},
		{name: "still running", status: 103, session: true},
		{name: "operation gone", session: true, missing: true},
		{name: "dispatch identity lost", dispatch: true},
		{name: "undispatched", undispatched: true, settled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads, writes atomic.Int32
			s, job, session := consoleRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				reads.Add(1)
				if r.URL.Path != consoleRecoveryOperation || r.URL.Query().Get("project") != "default" {
					t.Errorf("unexpected recovery observation: %s", r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.missing {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"type":"error","error_code":404}`))
					return
				}
				_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"id": strings.TrimPrefix(consoleRecoveryOperation, "/1.0/operations/"), "status_code": tc.status}})
			})
			operation := ""
			if tc.step {
				operation = consoleRecoveryOperation + "?project=default"
			}
			if err := s.a.DB.Table("task_steps").Create(platform.Row{"task_id": 10, "step_key": "prepare_console", "attempt": 7, "status": "cancelled", "operation_id": operation, "node_id": session.NodeID, "project_name": "default", "result_redacted": fmt.Sprintf(`{"native_dispatch_started":%t}`, tc.dispatch)}).Error; err != nil {
				t.Fatal(err)
			}
			if tc.session {
				if err := s.a.DB.Table("console_sessions").Where("id=?", session.ID).Update("operation_path", consoleRecoveryOperation).Error; err != nil {
					t.Fatal(err)
				}
			}
			if tc.undispatched {
				if err := s.a.DB.Table("tasks").Where("id=10").Update("result_redacted", `{"force_stop":{"requires_reconciliation":false,"external_state":"not_dispatched","orchestration_stopped":true}}`).Error; err != nil {
					t.Fatal(err)
				}
			}
			err := s.a.ReconcileJob(context.Background(), job)
			if (err == nil) != tc.settled {
				t.Fatalf("unexpected console recovery settlement: %v", err)
			}
			source, err := platform.Get(s.a.DB, "tasks", 10, false)
			if err != nil {
				t.Fatal(err)
			}
			var evidence struct {
				ForceStop struct {
					ReconciliationCompleted bool `json:"reconciliation_completed"`
				} `json:"force_stop"`
			}
			if err = json.Unmarshal([]byte(platform.Text(source["result_redacted"])), &evidence); err != nil {
				t.Fatal(err)
			}
			if platform.Text(source["status"]) != "cancelled" || platform.Text(source["error_code"]) != "task_force_stopped" || platform.Uint(source["lease_generation"]) != 8 || evidence.ForceStop.ReconciliationCompleted != tc.settled {
				t.Fatalf("recovery rewrote cancelled history or lost completion evidence: %v", source)
			}
			var step platform.Row
			if err = s.a.DB.Table("task_steps").Where("task_id=10").Take(&step).Error; err != nil || platform.Text(step["status"]) != "cancelled" || platform.Text(step["operation_id"]) != operation {
				t.Fatalf("recovery changed the source journal: %v %v", step, err)
			}
			if writes.Load() != 0 || (tc.step || tc.session) && reads.Load() == 0 {
				t.Fatal("console recovery mutated native state or omitted operation observation")
			}
		})
	}
}

func TestMySQLForceStoppedConsoleReconciliationFencesLateIO(t *testing.T) {
	for _, revoke := range []string{"wrapper", "source", "session"} {
		t.Run(revoke, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var unblock sync.Once
			defer unblock.Do(func() { close(release) })
			s, job, session := consoleRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"metadata":{"status_code":200}}`))
			})
			if err := s.a.DB.Table("console_sessions").Where("id=?", session.ID).Update("operation_path", consoleRecoveryOperation).Error; err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- s.a.ReconcileJob(context.Background(), job) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("console recovery did not issue observation")
			}
			switch revoke {
			case "wrapper":
				if err := s.a.DB.Table("tasks").Where("id=11").Updates(platform.Row{"status": "cancelled", "lease_generation": 2}).Error; err != nil {
					t.Fatal(err)
				}
			case "source":
				if err := s.a.DB.Table("tasks").Where("id=10").Update("revision", 2).Error; err != nil {
					t.Fatal(err)
				}
			case "session":
				if err := s.a.DB.Table("console_sessions").Where("id=?", session.ID).Update("revision", 2).Error; err != nil {
					t.Fatal(err)
				}
			}
			unblock.Do(func() { close(release) })
			if err := <-done; err == nil {
				t.Fatal("late terminal observation survived reconciliation fencing")
			}
			source, err := platform.Get(s.a.DB, "tasks", 10, false)
			if err != nil || strings.Contains(platform.Text(source["result_redacted"]), "reconciliation_completed") {
				t.Fatalf("late observation published source completion evidence: %v %v", source, err)
			}
		})
	}
}
