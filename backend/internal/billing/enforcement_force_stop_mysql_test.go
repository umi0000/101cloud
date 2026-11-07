package billing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

func TestMySQLForceStoppedCreditReconciliationFencesObservationAndContinuation(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	f.addDebt(t, []string{"3.000003"})
	debtBefore := debtRow(t, f.db, "usage_charges", 1)
	uuid := "11111111-1111-1111-1111-111111111111"
	if err := f.db.Exec(`INSERT INTO instance_plans(id,code,version,name,image_type,vcpus,memory_bytes,disk_bytes,gpu_count,pricing_version_id,credit_policy_version_id,idle_policy_version_id,init_parameters,allow_residency,status,allowed_overrides,policy_versions,gpu_policy,allowed_user_actions,gpu_user_permissions) VALUES(1,'force-stop-plan','1','Force stop plan','container',1,1073741824,1073741824,0,1,1,1,JSON_OBJECT(),FALSE,'active',JSON_OBJECT(),JSON_OBJECT('credit','1','metrics','100'),JSON_OBJECT(),JSON_ARRAY(),JSON_OBJECT())`).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("instances").Where("id=1").Updates(row{"product_type": "elastic", "instance_plan_id": 1, "plan_snapshot": jsonValue(row{"revision": "1", "vcpus": 1, "memory_bytes": "1073741824", "disk_bytes": "1073741824", "allowed_overrides": row{}}), "incus_uuid": uuid, "observed_state": "running", "observed_at": now().Add(-time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	instanceBefore := debtRow(t, f.db, "instances", 1)
	policy := validCreditPolicy()
	event, err := insert(f.db, "credit_enforcements", row{"account_id": 1, "user_id": 2, "policy_version_id": 1, "status": "unknown", "exhausted_at": now().Add(-time.Hour), "shutdown_deadline": now().Add(-time.Minute), "policy_snapshot": jsonValue(policy)})
	if err != nil {
		t.Fatal(err)
	}
	member, err := insert(f.db, "credit_enforcement_instances", row{"enforcement_id": event["id"], "instance_id": 1, "status": "pending"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := platform.Enqueue(f.db, "credit_enforcement_reconcile", 2, 0, 0, false, row{"enforcement_id": platform.Uint(event["id"])})
	if err != nil {
		t.Fatal(err)
	}
	j := platform.Job{ID: platform.Uint(task["task_id"]), Type: "credit_enforcement_reconcile", UserID: 2, LeaseGeneration: 1}
	if err = f.db.Table("tasks").Where("id=?", j.ID).Updates(row{"status": "running", "lease_generation": 1, "lease_owner": "test", "lease_until": now().Add(time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	var calls, mutations atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			mutations.Add(1)
			http.Error(w, "unexpected mutation", 500)
			return
		}
		// Native GET was already in flight when the administrator revoked the job.
		if err := f.db.Table("tasks").Where("id=?", j.ID).Updates(row{"status": "cancelled", "error_code": "task_force_stopped", "finished_at": now(), "lease_generation": gorm.Expr("lease_generation+1"), "lease_owner": nil, "lease_until": nil}).Error; err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status": "Stopped", "config": platform.Row{"volatile.uuid": uuid}}})
	})
	server := httptest.NewUnstartedServer(handler)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	defer server.Close()
	a := platform.New(f.db, platform.Config{MasterKey: make([]byte, 32), NodeAllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	token, err := a.Encrypt([]byte(strings.Repeat("A", 43)))
	if err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(server.Certificate().Raw)
	if err = f.db.Table("nodes").Where("id=1").Updates(row{"health": "online", "agent_endpoint": server.URL, "token_ciphertext": token, "server_cert_sha256": pin[:]}).Error; err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(f.ctx, enforcementJobKey{}, j)
	if err = reconcileEvent(ctx, a, platform.Uint(event["id"])); err == nil {
		t.Fatal("late native observation committed after force-stop")
	}
	if err = advanceEvent(ctx, a, platform.Uint(event["id"])); err == nil {
		t.Fatal("revoked reconciliation continued credit enforcement")
	}
	current := debtRow(t, f.db, "instances", 1)
	if platform.Text(current["revision"]) != platform.Text(instanceBefore["revision"]) || platform.Text(current["observed_state"]) != "running" {
		t.Fatal("late stopped observation changed instance/usage evidence")
	}
	if current := debtRow(t, f.db, "credit_enforcements", platform.Uint(event["id"])); platform.Text(current["status"]) != "unknown" || platform.Text(current["revision"]) != platform.Text(event["revision"]) {
		t.Fatal("force-stop completed or cancelled the financial enforcement")
	}
	if current := debtRow(t, f.db, "credit_enforcement_instances", platform.Uint(member["id"])); platform.Text(current["status"]) != "pending" || current["task_id"] != nil {
		t.Fatal("force-stopped continuation queued a shutdown")
	}
	assertDebtUnchanged(t, f.db, debtBefore)
	if f.count(t, "credit_ledger", "1=1") != 0 || f.count(t, "usage_sessions", "status='active'") != 1 || calls.Load() != 1 || mutations.Load() != 0 {
		t.Fatal("force-stop changed charges, sessions, ledger, or dispatched native work")
	}
}

func TestCreditReconciliationCancelledContextCannotAuthorizeWrites(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Cancellation must fail before requiring a database or attempting a lock.
	if err := enforcementWriteLease(ctx, nil, nil); err != context.Canceled {
		t.Fatalf("cancelled reconciliation context accepted: %v", err)
	}
}
