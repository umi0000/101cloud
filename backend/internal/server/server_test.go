package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

// ClientIP feeds the login rate-limit ip bucket; forwarded headers must only be
// believed when the TCP peer is a configured proxy.
func TestClientIPHonorsForwardedForOnlyFromTrustedProxies(t *testing.T) {
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	for _, tc := range []struct {
		name    string
		proxies []netip.Prefix
		remote  string
		headers map[string]string
		want    string
	}{
		{"trusted proxy forwards client", loopback, "127.0.0.1:40000", map[string]string{"X-Forwarded-For": "203.0.113.9"}, "203.0.113.9"},
		{"client-supplied prefix ignored", loopback, "127.0.0.1:40000", map[string]string{"X-Forwarded-For": "198.51.100.1, 203.0.113.9"}, "203.0.113.9"},
		{"X-Real-IP ignored", loopback, "127.0.0.1:40000", map[string]string{"X-Real-IP": "203.0.113.9"}, "127.0.0.1"},
		{"untrusted peer spoofing", loopback, "192.0.2.5:40000", map[string]string{"X-Forwarded-For": "203.0.113.9"}, "192.0.2.5"},
		{"no proxies configured", nil, "127.0.0.1:40000", map[string]string{"X-Forwarded-For": "203.0.113.9"}, "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Router(platform.New(nil, platform.Config{Origin: "https://cloud.example", TrustedProxies: tc.proxies}))
			r.GET("/client-ip", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })
			req := httptest.NewRequest(http.MethodGet, "/client-ip", nil)
			req.RemoteAddr = tc.remote
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			response := httptest.NewRecorder()
			r.ServeHTTP(response, req)
			if response.Code != http.StatusOK || response.Body.String() != tc.want {
				t.Fatalf("status=%d client ip=%q want %q", response.Code, response.Body.String(), tc.want)
			}
		})
	}
}
