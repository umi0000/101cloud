package platform

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestDecodeRejectsAmbiguousRequests(t *testing.T) {
	for _, body := range []string{`{"name":"a","name":"b"}`, `{"nested":{"x":1,"x":2}}`, `{"name":"a"} {"name":"b"}`, `{"unexpected":true}`} {
		t.Run(body, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/", strings.NewReader(body))
			var req struct {
				Name   string         `json:"name"`
				Nested map[string]any `json:"nested"`
			}
			if Decode(c, &req) == nil {
				t.Fatal("ambiguous or unknown request accepted")
			}
		})
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"ok","nested":{"x":1}}`))
	var req struct {
		Name   string         `json:"name"`
		Nested map[string]any `json:"nested"`
	}
	if e := Decode(c, &req); e != nil || req.Name != "ok" {
		t.Fatalf("valid nested object: %v", e)
	}
}
func TestEncryptedCredentialsRejectTamperingAndOtherKey(t *testing.T) {
	a := New(nil, Config{MasterKey: bytes.Repeat([]byte{1}, 32)})
	secret := []byte("node-secret-token")
	sealed, e := a.Encrypt(secret)
	if e != nil {
		t.Fatal(e)
	}
	plain, e := a.Decrypt(sealed)
	if e != nil || !bytes.Equal(plain, secret) {
		t.Fatal("credential round trip failed")
	}
	sealed[len(sealed)-1] ^= 1
	if _, e = a.Decrypt(sealed); e == nil {
		t.Fatal("tampered credential accepted")
	}
	b := New(nil, Config{MasterKey: bytes.Repeat([]byte{2}, 32)})
	valid, _ := a.Encrypt(secret)
	if _, e = b.Decrypt(valid); e == nil {
		t.Fatal("credential accepted with different key")
	}
}
func TestNodeAddressAllowlist(t *testing.T) {
	a := New(nil, Config{NodeAllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	for _, endpoint := range []string{"http://127.0.0.1:8443", "https://192.168.0.1", "https://name:password@127.0.0.1", "https://127.0.0.1/path", "https://127.0.0.1?query=1"} {
		if a.CheckEndpoint(endpoint) == nil {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	if e := a.CheckEndpoint("https://127.0.0.1:8443"); e != nil {
		t.Fatal(e)
	}
}
func TestPublicPreservesLargeIdentifiers(t *testing.T) {
	out := Public(Row{"id": uint64(18446744073709551614), "memory_bytes": uint64(18446744073709551613), "used": nil})
	b, e := json.Marshal(out)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(b, []byte(`"18446744073709551614"`)) || !bytes.Contains(b, []byte(`"18446744073709551613"`)) || out["used"] != nil {
		t.Fatalf("public integer precision/unknown lost: %s", b)
	}
}

// CLOUD_ORIGIN is compared byte for byte with the browser Origin header, so only
// the serialized form a browser sends may be configured.
func TestOriginMustBeCanonicalHTTPOrHTTPS(t *testing.T) {
	for _, origin := range []string{"https://cloud.example", "https://cloud.example:8443", "http://localhost:5555", "http://192.168.0.10", "https://[::1]:8443"} {
		if e := validOrigin(origin); e != nil {
			t.Fatalf("canonical origin %q rejected: %v", origin, e)
		}
	}
	for _, origin := range []string{"", "ftp://cloud.example", "https://cloud.example/", "https://cloud.example/app", "https://cloud.example?x=1", "https://cloud.example#x", "https://user@cloud.example", "https://Cloud.example", "HTTPS://cloud.example", "https://cloud.example:443", "http://cloud.example:80", "https://cloud.example:", "cloud.example", "https://"} {
		if validOrigin(origin) == nil {
			t.Fatalf("non-canonical origin %q accepted", origin)
		}
	}
	if !(Config{Origin: "https://cloud.example"}).HTTPS() || (Config{Origin: "http://localhost:5555"}).HTTPS() {
		t.Fatal("origin scheme misdetected")
	}
}

func TestTrustedProxiesParsing(t *testing.T) {
	if p, e := trustedProxies(" "); e != nil || p != nil {
		t.Fatalf("empty list must trust nothing: %v %v", p, e)
	}
	p, e := trustedProxies("127.0.0.1/32, ::1/128,10.0.0.7,fd00::1,192.168.1.9/24")
	if e != nil || len(p) != 5 {
		t.Fatal(p, e)
	}
	want := []string{"127.0.0.1/32", "::1/128", "10.0.0.7/32", "fd00::1/128", "192.168.1.0/24"}
	for i, prefix := range p {
		if prefix.String() != want[i] {
			t.Fatalf("entry %d = %s, want %s", i, prefix, want[i])
		}
	}
	for _, list := range []string{"127.0.0.1,", "localhost", "10.0.0.0/33", "fe80::1%eth0", "::ffff:127.0.0.1", "1.2.3"} {
		if _, e := trustedProxies(list); e == nil {
			t.Fatalf("invalid proxy list %q accepted", list)
		}
	}
}

func TestConfigPreservesLoginExpiryAndConsoleConcurrency(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUD_ORIGIN", "https://cloud.example")
	t.Setenv("CLOUD_MYSQL_DSN", "unused-config-test")
	t.Setenv("CLOUD_MASTER_KEY_FILE", keyPath)
	t.Setenv("CLOUD_SESSION_TTL", "")
	t.Setenv("CLOUD_CONSOLE_MAX_CONCURRENT", "")
	t.Setenv("CLOUD_NODE_ALLOW_CIDRS", "127.0.0.1/32")
	t.Setenv("CLOUD_TRUSTED_PROXIES", "")
	cfg, err := LoadConfig()
	if err != nil || cfg.SessionTTL != 12*time.Hour || cfg.ConsoleMaxConcurrent != 4 {
		t.Fatalf("login expiry or console concurrency default changed: cfg=%+v error=%v", cfg, err)
	}
	t.Setenv("CLOUD_SESSION_TTL", "2h")
	cfg, err = LoadConfig()
	if err != nil || cfg.SessionTTL != 2*time.Hour {
		t.Fatalf("explicit login expiry no longer honored: duration=%v error=%v", cfg.SessionTTL, err)
	}
	for _, value := range []string{"0s", "-1h", "invalid"} {
		t.Setenv("CLOUD_SESSION_TTL", value)
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("invalid login duration %q accepted", value)
		}
	}
}
