package instances

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

func TestCustomProvisioningReplacesApplicationRoutes(t *testing.T) {
	router := gin.New()
	user := router.Group("/api/v1")
	admin := router.Group("/api/v1/admin")
	Register(platform.New(nil, platform.Config{}), user, admin)
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/custom-instance-requests"},
		{http.MethodGet, "/custom-instance-requests"},
		{http.MethodGet, "/custom-instance-requests/1"},
		{http.MethodPost, "/custom-instance-requests/1/cancel"},
		{http.MethodGet, "/admin/custom-instance-requests"},
		{http.MethodGet, "/admin/custom-instance-requests/1"},
		{http.MethodPost, "/admin/custom-instance-requests/1/approve"},
		{http.MethodPost, "/admin/custom-instance-requests/1/reject"},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(route.method, "/api/v1"+route.path, strings.NewReader(`{}`)))
			if response.Code != http.StatusNotFound {
				t.Fatalf("removed application route returned %d: %s", response.Code, response.Body.String())
			}
		})
	}
	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		{"missing name", `{"owner_user_id":"1","approved_config":{}}`, "invalid_request"},
		{"invalid instance type", `{"owner_user_id":"1","name":"Direct instance","approved_config":{"instance_type":"invalid"}}`, "invalid_request"},
		{"invalid resources", `{"owner_user_id":"1","name":"Direct instance","approved_config":{"instance_type":"container","resources":{"vcpus":0,"memory_bytes":"1","disk_bytes":"1"}}}`, "invalid_request"},
		{"removed application reason", `{"owner_user_id":"1","name":"Direct instance","approved_config":{},"reason":"application"}`, "validation_failed"},
		{"removed review note", `{"owner_user_id":"1","name":"Direct instance","approved_config":{},"review_note":"approved"}`, "validation_failed"},
		{"missing node", `{"owner_user_id":"1","name":"Direct instance","approved_config":{"instance_type":"container","image_id":"1","network_option_id":"1","resources":{"vcpus":1,"memory_bytes":"1","disk_bytes":"1"},"gpu":{"count":0,"model":null,"access_mode":"shared","max_hourly_pt":"0"},"init_parameters":{}}}`, "invalid_request"},
		{"removed zone", `{"owner_user_id":"1","name":"Direct instance","approved_config":{"instance_type":"container","zone_id":"1","node_id":"1"}}`, "validation_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/admin/custom-instances", strings.NewReader(tc.body)))
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusUnprocessableEntity || body.Error.Code != tc.code {
				t.Fatalf("direct provisioning validation returned %d: %s", response.Code, response.Body.String())
			}
		})
	}
}
