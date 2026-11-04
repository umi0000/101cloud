package billing

import (
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
)

func TestMySQLStaleBillingObservationRequiresNativeIdentityAndStableOwnership(t *testing.T) {
	for _, scenario := range []string{"confirmed_stop", "foreign_uuid", "offline", "disconnected", "concurrent_change", "queued_stop", "unknown_state"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAdjustmentMySQLFixture(t)
			old := now().Add(-2 * time.Hour)
			uuid := "11111111-1111-1111-1111-111111111111"
			var calls atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/1.0/instances/adjustment-instance" || r.URL.Query().Get("project") != "default" {
					t.Errorf("unexpected observation request %s %s", r.Method, r.URL)
				}
				if scenario == "disconnected" {
					http.Error(w, "unavailable", 503)
					return
				}
				if scenario == "concurrent_change" {
					if err := f.db.Exec("UPDATE instances SET revision=revision+1 WHERE id=1").Error; err != nil {
						t.Error(err)
					}
				}
				actual, state := uuid, "Stopped"
				if scenario == "foreign_uuid" {
					actual = "22222222-2222-2222-2222-222222222222"
				}
				if scenario == "unknown_state" {
					state = "Error"
				}
				_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"name": "adjustment-instance", "status": state, "config": platform.Row{"volatile.uuid": actual}}})
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
			if err = f.db.Table("instances").Where("id=1").Updates(row{"incus_uuid": uuid, "observed_state": "running", "observed_at": old}).Error; err != nil {
				t.Fatal(err)
			}
			if scenario == "offline" {
				if err = f.db.Table("nodes").Where("id=1").Update("health", "offline").Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "queued_stop" {
				if _, err = platform.Enqueue(f.db, "instance.stop", 2, 1, 1, true, row{}); err != nil {
					t.Fatal(err)
				}
			}
			if err = refreshUsageObservation(f.ctx, a, 1); err != nil {
				t.Fatal(err)
			}
			current := debtRow(t, f.db, "instances", 1)
			if scenario == "confirmed_stop" {
				if platform.Text(current["observed_state"]) != "stopped" || !freshUsageObservation(current, now()) {
					t.Fatal("confirmed native STOP did not refresh billing evidence")
				}
			} else if platform.Text(current["observed_state"]) != "running" || !stamp(current["observed_at"]).Equal(old) {
				t.Fatal("untrusted or superseded observation changed billing evidence")
			}
			if scenario == "offline" && calls.Load() != 0 {
				t.Fatal("offline node was contacted for metering proof")
			}
		})
	}
}
