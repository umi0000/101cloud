package webui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func testHandler() http.Handler {
	return &handler{files: fstest.MapFS{
		"index.html":                  {Data: []byte("<!doctype html><html><body>cloud</body></html>")},
		"_app.config.js":              {Data: []byte("window.config={}")},
		"js/app-AbCd_123.js":          {Data: []byte("export const version=1;")},
		"jse/index-index-ABCdef12.js": {Data: []byte("import '/js/app-AbCd_123.js';")},
		"css/app-AbCd_123.css":        {Data: []byte("body{color:inherit}")},
		"images/logo.svg":             {Data: []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>")},
		"images/logo-AbCd_123.svg":    {Data: []byte("<svg/>")},
		".env":                        {Data: []byte("private")},
		"js/app.js.map":               {Data: []byte("private")},
	}}
}
func request(h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSPARoutesAndResourceCaching(t *testing.T) {
	h := testHandler()
	for _, tc := range []struct{ path, contentType, cache string }{
		{"/", "text/html; charset=utf-8", "no-store"},
		{"/index.html", "text/html; charset=utf-8", "no-store"},
		{"/instances/42", "text/html; charset=utf-8", "no-store"},
		{"/account/password?next=%2Finstances", "text/html; charset=utf-8", "no-store"},
		{"/admin/dashboard", "text/html; charset=utf-8", "no-store"},
		{"/_app.config.js?v=123", "text/javascript; charset=utf-8", "no-store"},
		{"/js/app-AbCd_123.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable"},
		{"/jse/index-index-ABCdef12.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable"},
		{"/css/app-AbCd_123.css", "text/css; charset=utf-8", "public, max-age=31536000, immutable"},
		{"/images/logo-AbCd_123.svg", "image/svg+xml", "no-cache"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			get := request(h, http.MethodGet, tc.path, nil)
			if get.Code != 200 || get.Header().Get("Content-Type") != tc.contentType || get.Header().Get("Cache-Control") != tc.cache || get.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("GET status=%d headers=%v", get.Code, get.Header())
			}
			head := request(h, http.MethodHead, tc.path, nil)
			if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) || head.Header().Get("Content-Type") != tc.contentType || head.Header().Get("Cache-Control") != tc.cache {
				t.Fatalf("HEAD status=%d headers=%v body=%d", head.Code, head.Header(), head.Body.Len())
			}
		})
	}
	root := request(h, http.MethodGet, "/", nil)
	deep := request(h, http.MethodGet, "/account/password", map[string]string{"Accept": "text/html,application/xhtml+xml;q=0.9"})
	if !bytes.Equal(root.Body.Bytes(), deep.Body.Bytes()) {
		t.Fatal("deep link did not serve the application entry")
	}
}
func TestMissesUnsafePathsAndWrongMethodsNeverServeSPA(t *testing.T) {
	h := testHandler()
	for _, target := range []string{
		"/api", "/api/", "/api/v1/missing", "/api/v2/missing",
		"/js", "/js/", "/js/missing", "/jse/missing", "/css/missing", "/images/missing", "/assets/missing",
		"/deleted.js", "/deleted.png", "/deleted.json", "/deleted.wasm", "/deleted.js/nested",
		"/../index.html", "/account/../index.html", "/%2e%2e/index.html", "/account/%2e/password",
		"//index.html", "/account//password", "/%2findex.html", "/account%2Fpassword", "/account%5cpassword", "/account%255cpassword",
		"/%252e%252e/index.html", "/index.html%00", "/.env", "/.git/config", "/js/.hidden", "/js/app.js.map", "/account/%09password",
	} {
		w := request(h, http.MethodGet, target, map[string]string{"Accept": "text/html"})
		if w.Code != 404 || w.Header().Get("Location") != "" || strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("path=%s status=%d headers=%v", target, w.Code, w.Header())
		}
	}
	w := request(h, http.MethodGet, "/account/password", map[string]string{"Accept": "application/json"})
	if w.Code != 404 {
		t.Fatalf("non-navigation miss status=%d", w.Code)
	}
	w = request(h, http.MethodPost, "/account/password", nil)
	if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("wrong method status=%d allow=%q", w.Code, w.Header().Get("Allow"))
	}
}
func TestRangeRequestsUseReadOnlyAsset(t *testing.T) {
	h := testHandler()
	get := request(h, http.MethodGet, "/js/app-AbCd_123.js", nil)
	partial := request(h, http.MethodGet, "/js/app-AbCd_123.js", map[string]string{"Range": "bytes=2-9"})
	if partial.Code != 206 || !bytes.Equal(partial.Body.Bytes(), get.Body.Bytes()[2:10]) || partial.Header().Get("Content-Range") != "bytes 2-9/"+strconv.Itoa(get.Body.Len()) {
		t.Fatalf("partial status=%d headers=%v", partial.Code, partial.Header())
	}
	invalid := request(h, http.MethodGet, "/js/app-AbCd_123.js", map[string]string{"Range": "bytes=" + strconv.Itoa(get.Body.Len()) + "-"})
	if invalid.Code != 416 {
		t.Fatalf("out-of-range status=%d", invalid.Code)
	}
}
