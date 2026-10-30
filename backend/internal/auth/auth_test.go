package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

func TestPasswordHashVerification(t *testing.T) {
	password := "correct horse battery staple"
	encoded, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("unexpected hash format: %q", encoded)
	}
	if !verifyPassword(encoded, password) {
		t.Fatal("correct password was rejected")
	}
	if verifyPassword(encoded, "incorrect horse battery staple") {
		t.Fatal("wrong password was accepted")
	}
	other, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hash password again: %v", err)
	}
	if encoded == other {
		t.Fatal("password hashes reused their salt")
	}
}

func TestPasswordBounds(t *testing.T) {
	for _, tc := range []struct {
		name, password string
		valid          bool
	}{
		{"empty", "", false},
		{"too short", strings.Repeat("a", 11), false},
		{"minimum", strings.Repeat("a", 12), true},
		{"maximum", strings.Repeat("a", 128), true},
		{"too long", strings.Repeat("a", 129), false},
		{"unicode minimum", strings.Repeat("密", 12), true},
		{"unicode maximum", strings.Repeat("密", 128), true},
		{"unicode too short", strings.Repeat("密", 11), false},
		{"invalid UTF8", strings.Repeat("a", 12) + string([]byte{0xff}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validPassword(tc.password); got != tc.valid {
				t.Fatalf("validPassword=%v, want %v", got, tc.valid)
			}
			if !tc.valid {
				if _, err := hashPassword(tc.password); err == nil {
					t.Fatal("invalid password was hashed")
				}
			}
		})
	}
}

func TestMalformedPasswordHashesAreRejected(t *testing.T) {
	salt := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16))
	key := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	prefix := "$argon2id$v=19$m=65536,t=3,p=2$"
	for _, tc := range []struct{ name, encoded, password string }{
		{"empty", "", "valid password"},
		{"plaintext", "valid password", "valid password"},
		{"wrong algorithm", strings.Replace(dummyPasswordHash, "argon2id", "argon2i", 1), "valid password"},
		{"wrong version", strings.Replace(dummyPasswordHash, "v=19", "v=16", 1), "valid password"},
		{"unbounded memory", strings.Replace(dummyPasswordHash, "m=65536", "m=4294967295", 1), "valid password"},
		{"unbounded iterations", strings.Replace(dummyPasswordHash, "t=3", "t=4294967295", 1), "valid password"},
		{"unbounded parallelism", strings.Replace(dummyPasswordHash, "p=2", "p=255", 1), "valid password"},
		{"extra field", dummyPasswordHash + "$extra", "valid password"},
		{"invalid salt", prefix + "!$" + key, "valid password"},
		{"invalid key", prefix + salt + "$!", "valid password"},
		{"short salt", prefix + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 15)) + "$" + key, "valid password"},
		{"oversized salt", prefix + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 65)) + "$" + key, "valid password"},
		{"short key", prefix + salt + "$" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 31)), "valid password"},
		{"oversized key", prefix + salt + "$" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 33)), "valid password"},
		{"oversized password", dummyPasswordHash, strings.Repeat("a", 513)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if verifyPassword(tc.encoded, tc.password) {
				t.Fatal("malformed password hash or input was accepted")
			}
		})
	}
}

func testAuthApp() *platform.App {
	return platform.New(nil, platform.Config{Origin: "https://cloud.example", MasterKey: bytes.Repeat([]byte{7}, 32), SessionTTL: 8 * time.Hour})
}

func testCSRFRouter(a *platform.App) *gin.Engine {
	r := gin.New()
	api := r.Group("/api/v1", CSRF(a))
	Register(a, api, api.Group(""), api.Group("/admin"))
	api.POST("/probe", func(c *gin.Context) { noContent(c) })
	return r
}

func getCSRF(t *testing.T, r http.Handler, session string) (string, *http.Cookie) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "https://cloud.example/api/v1/auth/csrf", nil)
	if session != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	}
	response := httptest.NewRecorder()
	r.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("csrf initialization status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("CSRF initialization was cacheable")
	}
	var envelope struct {
		Data struct {
			Token string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode CSRF response: %v", err)
	}
	if envelope.Data.Token == "" {
		t.Fatal("missing CSRF token")
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == csrfCookie {
			if !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.Domain != "" || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != int(csrfTTL.Seconds()) {
				t.Fatalf("unsafe CSRF context cookie: %+v", cookie)
			}
			return envelope.Data.Token, cookie
		}
	}
	t.Fatal("missing browser-context cookie")
	return "", nil
}

func TestCSRFBrowserContextSessionBindingAndOrigin(t *testing.T) {
	a := testAuthApp()
	r := testCSRFRouter(a)
	session := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	otherSession := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	token, cookie := getCSRF(t, r, session)
	anonymousToken, anonymousCookie := getCSRF(t, r, "")
	expiredBody := strings.Repeat("A", 43) + "." + strconv.FormatInt(a.Time().Add(-time.Minute).Unix(), 10)
	expired := expiredBody + "." + base64.RawURLEncoding.EncodeToString(keyed(a, "csrf-context:"+expiredBody))
	for _, tc := range []struct {
		name, origin, token, context, session, fetchSite string
		status                                           int
	}{
		{"same origin session", a.Config.Origin, token, cookie.Value, session, "same-origin", 204},
		{"same origin anonymous login context", a.Config.Origin, anonymousToken, anonymousCookie.Value, "", "", 204},
		{"missing origin", "", token, cookie.Value, session, "", 403},
		{"different origin", "https://attacker.example", token, cookie.Value, session, "", 403},
		{"origin suffix", a.Config.Origin + ".attacker.example", token, cookie.Value, session, "", 403},
		{"origin trailing slash", a.Config.Origin + "/", token, cookie.Value, session, "", 403},
		{"null origin", "null", token, cookie.Value, session, "", 403},
		{"cross site fetch", a.Config.Origin, token, cookie.Value, session, "cross-site", 403},
		{"missing token", a.Config.Origin, "", cookie.Value, session, "", 403},
		{"modified token", a.Config.Origin, token + "x", cookie.Value, session, "", 403},
		{"missing browser context", a.Config.Origin, token, "", session, "", 403},
		{"modified browser context", a.Config.Origin, token, cookie.Value + "x", session, "", 403},
		{"expired signed context", a.Config.Origin, csrfFor(a, expired, session), expired, session, "", 403},
		{"different session", a.Config.Origin, token, cookie.Value, otherSession, "", 403},
		{"session removed", a.Config.Origin, token, cookie.Value, "", "", 403},
		{"prelogin token after login", a.Config.Origin, anonymousToken, anonymousCookie.Value, session, "", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://cloud.example/api/v1/probe", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.token != "" {
				req.Header.Set("X-CSRF-Token", tc.token)
			}
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			if tc.context != "" {
				req.AddCookie(&http.Cookie{Name: csrfCookie, Value: tc.context})
			}
			if tc.session != "" {
				req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tc.session})
			}
			response := httptest.NewRecorder()
			r.ServeHTTP(response, req)
			if response.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.status, response.Body.String())
			}
			if tc.status == 403 && !strings.Contains(response.Body.String(), `"code":"csrf_failed"`) {
				t.Fatalf("wrong rejection: %s", response.Body.String())
			}
		})
	}
}

func TestCSRFInitializationRejectsCrossOrigin(t *testing.T) {
	a := testAuthApp()
	r := testCSRFRouter(a)
	for _, tc := range []struct{ name, origin, fetchSite string }{
		{"foreign origin", "https://attacker.example", ""},
		{"cross site request", "", "cross-site"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "https://cloud.example/api/v1/auth/csrf", nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			response := httptest.NewRecorder()
			r.ServeHTTP(response, req)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if len(response.Result().Cookies()) != 0 {
				t.Fatal("cross-origin initialization set a browser context")
			}
		})
	}
}

func TestNoContentSurvivesEndpointWrapper(t *testing.T) {
	r := gin.New()
	r.POST("/empty", platform.Handle(func(c *gin.Context) (any, error) { noContent(c); return nil, nil }))
	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/empty", nil))
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}
