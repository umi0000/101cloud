package platform

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

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
