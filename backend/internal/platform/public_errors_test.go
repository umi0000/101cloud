package platform

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPublicErrorRetainsSafeUnknownCause(t *testing.T) {
	cause := EDetails(409, "storage_pool_capacity_exceeded", "节点存储池可用空间不足。", Row{"requested": "18446744073709551615", "available": "16106127360"})
	got := PublicError(Unknown(cause), true)
	if got["code"] != "storage_pool_capacity_exceeded" || got["message"] != "节点存储池可用空间不足。" {
		t.Fatalf("safe wrapped cause lost: %+v", got)
	}
	b, err := json.Marshal(Row{"error": got, "private_output": "do not expose"})
	if err != nil {
		t.Fatal(err)
	}
	saved := StoredTaskError(string(b), "unknown", "storage_pool_capacity_exceeded")
	encoded, _ := json.Marshal(saved)
	if strings.Contains(string(encoded), "private_output") || !strings.Contains(string(encoded), "18446744073709551615") {
		t.Fatalf("result whitelist or precision lost: %s", encoded)
	}
	if StoredTaskError(string(b), "succeeded", "") != nil {
		t.Fatal("successful task exposed stale error")
	}
}

func TestPublicErrorsNeverExposeUntypedSecrets(t *testing.T) {
	secret := "SELECT password FROM credentials; token=private"
	for _, unknown := range []bool{false, true} {
		b, _ := json.Marshal(PublicError(errors.New(secret), unknown))
		if strings.Contains(string(b), secret) || !strings.Contains(string(b), "管理员") {
			t.Fatalf("unsafe/nonactionable fallback: %s", b)
		}
	}
	for _, value := range []string{`{"error":"password=private"}`, `{"error":{"code":"wrong","message":"password=private"}}`, `{"raw_error":"password=private"}`} {
		b, _ := json.Marshal(StoredTaskError(value, "unknown", "external_result_unknown"))
		if strings.Contains(string(b), "password") {
			t.Fatalf("historical raw output leaked: %s", b)
		}
	}
}

func TestFailPreservesChineseAndRequestID(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("request_id", "request-safe-123")
	Fail(c, EDetails(409, "quota_exceeded", "用户内存配额不足。", Row{"resource": "memory_bytes", "available": "0"}))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "用户内存配额不足") || !strings.Contains(w.Body.String(), "request-safe-123") {
		t.Fatalf("safe HTTP failure lost context: %s", w.Body.String())
	}
}

func TestDecodeProvidesSafeFieldDiagnostics(t *testing.T) {
	for _, tc := range []struct{ body, field, code string }{
		{`{"vcpus":"secret-value"}`, "vcpus", "validation_failed"},
		{`{"unexpected":1}`, "unexpected", "validation_failed"},
		{`{"vcpus":1,"vcpus":2}`, "", "invalid_json"},
		{`{"vcpus":`, "", "invalid_json"},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
		var body struct {
			VCPUs uint64 `json:"vcpus"`
		}
		err := Decode(c, &body)
		var he *HTTPError
		if !errors.As(err, &he) || he.Code != tc.code {
			t.Fatalf("unexpected decode error: %v", err)
		}
		got := PublicError(err, false)
		b, _ := json.Marshal(got)
		if strings.Contains(string(b), "secret-value") || !strings.Contains(string(b), "请") {
			t.Fatalf("unsafe/unhelpful validation: %s", b)
		}
		if tc.field != "" && got["details"].(map[string]any)["field"] != tc.field {
			t.Fatalf("field path absent: %+v", got)
		}
	}
}
