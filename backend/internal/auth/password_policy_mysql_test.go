package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
	"golang.org/x/crypto/argon2"
)

// This regression uses a real HTTP listener and an isolated, migrated MySQL
// database. Domain snapshots exclude only intentional auth rate-limit counters.
func TestMySQLPasswordPolicyHTTP(t *testing.T) {
	h := newRegistrationHarness(t)
	h.expect(h.settings(registrationSettingsBody(true, "101"), ""), http.StatusOK, "")
	server := httptest.NewTLSServer(h.r)
	t.Cleanup(server.Close)
	transport := server.Client()
	call := func(client *registrationClient, method, path string, body any, headers map[string]string, status int) (http.Header, []byte) {
		t.Helper()
		var payload []byte
		if body != nil {
			var err error
			payload, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequestWithContext(h.ctx, method, server.URL+"/api/v1"+path, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", h.a.Config.Origin)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", client.csrf)
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		for _, cookie := range client.cookies {
			req.AddCookie(cookie)
		}
		response, err := transport.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		result, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status {
			t.Fatalf("%s %s status=%d, want %d: %s", method, path, response.StatusCode, status, result)
		}
		if status == http.StatusUnprocessableEntity && !bytes.Contains(result, []byte(`"code":"validation_failed"`)) {
			t.Fatalf("wrong validation error: %s", result)
		}
		return response.Header, result
	}
	login := func(client *registrationClient, username, password string) {
		t.Helper()
		headers, body := call(client, http.MethodPost, "/auth/login", map[string]string{"username": username, "password": password}, nil, http.StatusOK)
		var envelope struct {
			Data struct {
				CSRF string `json:"csrf_token"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatal(err)
		}
		client.csrf = envelope.Data.CSRF
		client.cookies = (&http.Response{Header: headers}).Cookies()
	}
	snapshot := func() map[string][]platform.Row {
		t.Helper()
		state := make(map[string][]platform.Row)
		for _, table := range []string{"users", "user_limits", "user_sessions", "credit_accounts", "credit_buckets", "credit_daily_grants", "credit_ledger", "audit_logs", "backend_requests"} {
			var rows []platform.Row
			order := "id"
			if table == "backend_requests" {
				order = "scope,request_key"
			}
			if err := h.db.Table(table).Order(order).Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			state[table] = rows
		}
		return state
	}
	assertUnchanged := func(before map[string][]platform.Row) {
		t.Helper()
		if after := snapshot(); !reflect.DeepEqual(before, after) {
			for table, rows := range before {
				if !reflect.DeepEqual(rows, after[table]) {
					t.Errorf("rejected password changed %s", table)
				}
			}
			t.FailNow()
		}
	}
	minimum := "Aa123456"
	call(h.admin, http.MethodPost, "/admin/users", map[string]string{"username": "legacy-password-user", "initial_password": minimum, "role": "user", "credit_policy_version_id": "101"}, map[string]string{"Idempotency-Key": "password-legacy-fixture"}, http.StatusCreated)
	var target platform.Row
	if err := h.db.Table("users").Where("username = ?", "legacy-password-user").Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatUint(platform.Uint(target["id"]), 10)
	// Seed a historical Argon2id hash without passing through new-password policy.
	weak := "weak"
	salt := bytes.Repeat([]byte{17}, 16)
	key := argon2.IDKey([]byte(weak), salt, 3, 65536, 2, 32)
	legacyHash := fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
	if err := h.db.Table("users").Where("id = ?", id).Update("password_hash", []byte(legacyHash)).Error; err != nil {
		t.Fatal(err)
	}
	owner := h.client()
	login(owner, "legacy-password-user", weak)
	meHeaders, _ := call(owner, http.MethodGet, "/me", nil, nil, http.StatusOK)
	adminHeaders, _ := call(h.admin, http.MethodGet, "/admin/users/"+id, nil, nil, http.StatusOK)
	invalid := []struct{ name, password string }{
		{"missing-uppercase", "abcdefg1"},
		{"missing-lowercase", "ABCDEFG1"},
		{"missing-digit", "Abcdefgh"},
		{"too-short", "Aa12345"},
		{"non-ascii-uppercase", "Äbcdefg1"},
		{"non-ascii-lowercase", "A中文密码１２３1"},
		{"non-ascii-digit", "Abcdefg１"},
		{"over-unicode-limit", "Aa1" + strings.Repeat("密", 126)},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshot()
			call(h.guest, http.MethodPost, "/auth/register", map[string]string{"username": "rejected-" + tc.name, "password": tc.password}, nil, http.StatusUnprocessableEntity)
			assertUnchanged(before)
			call(h.admin, http.MethodPost, "/admin/users", map[string]string{"username": "rejected-" + tc.name, "initial_password": tc.password, "role": "user", "credit_policy_version_id": "101"}, map[string]string{"Idempotency-Key": "create-" + tc.name}, http.StatusUnprocessableEntity)
			assertUnchanged(before)
			call(owner, http.MethodPost, "/me/password", map[string]string{"current_password": weak, "new_password": tc.password}, map[string]string{"If-Match": meHeaders.Get("ETag"), "Idempotency-Key": "change-" + tc.name}, http.StatusUnprocessableEntity)
			assertUnchanged(before)
			call(h.admin, http.MethodPost, "/admin/users/"+id+"/reset-password", map[string]string{"new_password": tc.password}, map[string]string{"If-Match": adminHeaders.Get("ETag"), "Idempotency-Key": "reset-" + tc.name}, http.StatusUnprocessableEntity)
			assertUnchanged(before)
		})
	}
	// Every setting entrance accepts exactly eight characters.
	call(h.guest, http.MethodPost, "/auth/register", map[string]string{"username": "eight-char-registered", "password": minimum}, nil, http.StatusCreated)
	call(h.admin, http.MethodPost, "/admin/users", map[string]string{"username": "eight-char-created", "initial_password": minimum, "role": "user", "credit_policy_version_id": "101"}, map[string]string{"Idempotency-Key": "eight-char-create"}, http.StatusCreated)
	call(owner, http.MethodPost, "/me/password", map[string]string{"current_password": weak, "new_password": minimum}, map[string]string{"If-Match": meHeaders.Get("ETag"), "Idempotency-Key": "eight-char-change"}, http.StatusNoContent)
	call(owner, http.MethodGet, "/me", nil, nil, http.StatusUnauthorized)
	owner = h.client()
	login(owner, "legacy-password-user", minimum)
	adminHeaders, _ = call(h.admin, http.MethodGet, "/admin/users/"+id, nil, nil, http.StatusOK)
	reset := "Bb654321"
	call(h.admin, http.MethodPost, "/admin/users/"+id+"/reset-password", map[string]string{"new_password": reset}, map[string]string{"If-Match": adminHeaders.Get("ETag"), "Idempotency-Key": "eight-char-reset"}, http.StatusNoContent)
	call(owner, http.MethodGet, "/me", nil, nil, http.StatusUnauthorized)
	owner = h.client()
	login(owner, "legacy-password-user", reset)
	for _, username := range []string{"eight-char-registered", "eight-char-created"} {
		login(h.client(), username, minimum)
	}
	// Count Unicode code points rather than bytes, and preserve whitespace in
	// both the stored password and authentication path.
	for i, password := range []string{"Aa1" + strings.Repeat("密", 125), " Aa1234 "} {
		meHeaders, _ = call(owner, http.MethodGet, "/me", nil, nil, http.StatusOK)
		call(owner, http.MethodPost, "/me/password", map[string]string{"current_password": reset, "new_password": password}, map[string]string{"If-Match": meHeaders.Get("ETag"), "Idempotency-Key": fmt.Sprintf("unicode-space-change-%d", i)}, http.StatusNoContent)
		owner = h.client()
		login(owner, "legacy-password-user", password)
		reset = password
	}
	t.Log("real TLS HTTP/MySQL: all four password-setting entrances reject every missing ASCII class and invalid boundary without domain/session/audit/idempotency writes; all accept eight characters; legacy weak login/current-password verification and session revocation passed")
}
