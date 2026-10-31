package catalog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

// Availability zones are gone: their routes no longer exist and a client that
// still sends zone_id is rejected by strict decoding before any lookup.
func TestRemovedZoneContract(t *testing.T) {
	router := gin.New()
	user := router.Group("/api/v1")
	admin := router.Group("/api/v1/admin")
	Register(platform.New(nil, platform.Config{}), user, admin)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/catalog/zones"},
		{http.MethodGet, "/admin/zones"},
		{http.MethodPost, "/admin/zones"},
		{http.MethodGet, "/admin/zones/1"},
		{http.MethodPatch, "/admin/zones/1"},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(route.method, "/api/v1"+route.path, strings.NewReader(`{}`)))
			if response.Code != http.StatusNotFound {
				t.Fatalf("removed zone route returned %d: %s", response.Code, response.Body.String())
			}
		})
	}
	for _, tc := range []struct{ name, path, body string }{
		{"node", "/api/v1/admin/nodes", `{"name":"node-a","zone_id":"1"}`},
		{"network pool", "/api/v1/admin/network-pools", `{"name":"lan","zone_id":"1","cidr":"192.0.2.0/24"}`},
		{"plan", "/api/v1/admin/plans", `{"code":"basic","zone_id":"1"}`},
		{"quote", "/api/v1/quotes", `{"name":"web","zone_id":"1","plan_id":"1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			var body struct {
				Error struct {
					Code    string         `json:"code"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusUnprocessableEntity || body.Error.Code != "validation_failed" || body.Error.Details["field"] != "zone_id" {
				t.Fatalf("zone_id accepted or misreported: %d %s", response.Code, response.Body.String())
			}
		})
	}
}
