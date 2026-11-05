package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
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

func TestEmbeddedFrontendAndAPIRoutesRemainSeparated(t *testing.T) {
	router := Router(platform.New(nil, platform.Config{Origin: "https://cloud.example"}))
	get := func(method, target string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(method, target, nil))
		return response
	}
	root := get(http.MethodGet, "/")
	if root.Code != http.StatusOK || !strings.HasPrefix(root.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("frontend unavailable: status=%d content-type=%q", root.Code, root.Header().Get("Content-Type"))
	}
	for _, target := range []string{"/instances/42", "/account/password", "/admin/dashboard", "/auth/login?redirect=%2Finstances%2F42"} {
		response := get(http.MethodGet, target)
		if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), root.Body.Bytes()) {
			t.Fatalf("deep link did not serve the application: %s status=%d", target, response.Code)
		}
	}
	for _, target := range []string{"/api", "/api/unknown", "/api/v1/unknown", "/api/v2/unknown"} {
		response := get(http.MethodGet, target)
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != http.StatusNotFound || body.Error.Code != "not_found" {
			t.Fatalf("API miss became an HTML page: %s status=%d", target, response.Code)
		}
	}
	response := get(http.MethodHead, "/api/v1/health/live")
	if response.Code != http.StatusMethodNotAllowed || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("API unsupported method changed: status=%d", response.Code)
	}
	if response := get(http.MethodGet, "/js/deleted-bundle.js"); response.Code != http.StatusNotFound || bytes.Equal(response.Body.Bytes(), root.Body.Bytes()) {
		t.Fatal("missing asset fell back to the application")
	}
}
