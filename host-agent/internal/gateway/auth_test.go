package gateway

import (
	"crypto/tls"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

var testNodeToken = base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func TestLoadToken(t *testing.T) {
	for _, suffix := range []string{"", "\n", "\r\n"} {
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte(testNodeToken+suffix), 0600); err != nil {
			t.Fatal(err)
		}
		if token, err := LoadToken(path); err != nil || token != testNodeToken {
			t.Fatalf("token file with suffix %q: token = %q, err = %v", suffix, token, err)
		}
	}
	for _, value := range []string{"", "short", testNodeToken + "=", " " + testNodeToken, testNodeToken + "\r", testNodeToken + "\n\n", testNodeToken + "\n\r\n", strings.Repeat("/", 43), strings.Repeat("_", 43), strings.Repeat("A", 100)} {
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadToken(path); err == nil {
			t.Errorf("accepted malformed token %q", value)
		}
	}
	for _, mode := range []os.FileMode{0400, 0640, 0644, 0666} {
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte(testNodeToken), mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadToken(path); err == nil {
			t.Errorf("accepted token file mode %04o", mode)
		}
	}
	if _, err := LoadToken(t.TempDir()); err == nil {
		t.Fatal("accepted directory as token")
	}
}

func TestTokenAuthOverRealHTTPS(t *testing.T) {
	server, client := authenticatedTestServer(t, TokenAuth(testNodeToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values("Authorization")) != 0 {
			t.Error("Authorization reached upstream")
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	for _, tc := range []struct {
		name   string
		values []string
		query  string
		cookie string
		want   int
	}{
		{name: "valid", values: []string{"Bearer " + testNodeToken}, want: http.StatusNoContent},
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong", values: []string{"Bearer " + strings.Repeat("A", 43)}, want: http.StatusUnauthorized},
		{name: "duplicate", values: []string{"Bearer " + testNodeToken, "Bearer " + testNodeToken}, want: http.StatusUnauthorized},
		{name: "combined", values: []string{"Bearer " + testNodeToken + ", Bearer " + testNodeToken}, want: http.StatusUnauthorized},
		{name: "wrong scheme", values: []string{"Basic " + testNodeToken}, want: http.StatusUnauthorized},
		{name: "lowercase scheme", values: []string{"bearer " + testNodeToken}, want: http.StatusUnauthorized},
		{name: "double space", values: []string{"Bearer  " + testNodeToken}, want: http.StatusUnauthorized},
		{name: "query only", query: "?token=" + testNodeToken + "&access_token=" + testNodeToken, want: http.StatusUnauthorized},
		{name: "cookie only", cookie: "token=" + testNodeToken + "; access_token=" + testNodeToken, want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, server.URL+tc.query, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range tc.values {
				request.Header.Add("Authorization", value)
			}
			request.Header.Set("Cookie", tc.cookie)
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", response.StatusCode, tc.want)
			}
			if tc.want == http.StatusUnauthorized && response.Header.Get("WWW-Authenticate") == "" {
				t.Error("missing authentication challenge")
			}
			if strings.Contains(string(body), testNodeToken) {
				t.Error("error disclosed token")
			}
		})
	}
}

func TestTokenAuthRejectsPlaintextAndCaseDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tls       bool
		duplicate bool
		token     string
	}{
		{name: "plaintext", token: testNodeToken},
		{name: "case duplicate", tls: true, duplicate: true, token: testNodeToken},
		{name: "invalid configured token", tls: true, token: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://node.test/", nil)
			request.Header.Set("Authorization", "Bearer "+testNodeToken)
			if tc.tls {
				request.TLS = &tls.ConnectionState{}
			}
			if tc.duplicate {
				request.Header["authorization"] = []string{"Bearer " + testNodeToken}
			}
			response := httptest.NewRecorder()
			TokenAuth(tc.token, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unauthenticated handler reached") })).ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d", response.Code)
			}
		})
	}
}

func TestWebSocketBearerBoundary(t *testing.T) {
	server, client := authenticatedTestServer(t, TokenAuth(testNodeToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("WebSocket upgrade leaked Authorization upstream")
		}
		connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		kind, data, err := connection.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		if err := connection.WriteMessage(kind, data); err != nil {
			t.Error(err)
		}
	})))
	dialer := websocket.Dialer{TLSClientConfig: client.Transport.(*http.Transport).TLSClientConfig}
	for _, tc := range []struct {
		name     string
		header   http.Header
		query    string
		accepted bool
	}{
		{name: "Bearer", header: http.Header{"Authorization": {"Bearer " + testNodeToken}}, accepted: true},
		{name: "missing"},
		{name: "wrong", header: http.Header{"Authorization": {"Bearer " + strings.Repeat("A", 43)}}},
		{name: "duplicate", header: http.Header{"Authorization": {"Bearer " + testNodeToken, "Bearer " + testNodeToken}}},
		{name: "query", query: "?token=" + testNodeToken},
		{name: "cookie", header: http.Header{"Cookie": {"token=" + testNodeToken}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connection, response, err := dialer.Dial("wss"+strings.TrimPrefix(server.URL, "https")+tc.query, tc.header)
			if !tc.accepted {
				if connection != nil {
					connection.Close()
				}
				if response != nil {
					defer response.Body.Close()
				}
				if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized || response.Header.Get("WWW-Authenticate") == "" {
					t.Fatalf("unauthorized WebSocket: response = %v, err = %v", response, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if err := connection.WriteMessage(websocket.BinaryMessage, []byte("operation stream")); err != nil {
				t.Fatal(err)
			}
			_, message, err := connection.ReadMessage()
			if err != nil || string(message) != "operation stream" {
				t.Fatalf("WebSocket echo = %q, err = %v", message, err)
			}
		})
	}
}
