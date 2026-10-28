package server

import (
	"bytes"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuditDoesNotRecordOperationWebsocketSecret(t *testing.T) {
	var logs bytes.Buffer
	handler := audit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("secret") != "sensitive-operation-credential" {
			t.Fatal("operation credential was altered")
		}
		w.WriteHeader(http.StatusNoContent)
	}), slog.New(slog.NewJSONHandler(&logs, nil)))
	request := httptest.NewRequest("GET", "https://node/1.0/operations/task/websocket?secret=sensitive-operation-credential", nil)
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Authorization", "Bearer sensitive-node-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 204 {
		t.Fatalf("status %d", response.Code)
	}
	if strings.Contains(logs.String(), "sensitive-operation-credential") || strings.Contains(logs.String(), "sensitive-node-token") {
		t.Fatal("WebSocket secret leaked to audit log")
	}
}

func TestAuditRejectsPlaintextRequests(t *testing.T) {
	called := false
	handler := audit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	r := httptest.NewRequest("GET", "http://node/1.0", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 401 || called {
		t.Fatal("unauthenticated request reached privileged handler")
	}
}
