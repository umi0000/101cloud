package instances

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

func TestMySQLResizeLimitsReservations(t *testing.T) {
	db, a := admissionMySQLFixture(t)
	exec := func(s string) {
		t.Helper()
		if err := db.Exec(s).Error; err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE instances SET provisioning_state='ready',desired_state='stopped',observed_state='stopped' WHERE id=1")
	exec("INSERT INTO users VALUES(2,'active')")
	exec("INSERT INTO instances VALUES(3,2,1,'elastic','stopped','stopped','ready',NULL,NULL,7,8,3,NULL,1),(4,2,1,'custom','running','running','ready',NULL,NULL,2,3,5,NULL,1)")
	r, err := platform.Get(db, "instances", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	check := func(v uint64, m, d string) {
		t.Helper()
		got, err := resizeMaxResources(a, db, r)
		if err != nil || got != (Resources{v, m, d}) {
			t.Fatalf("got %+v, %v; want %d/%s/%s", got, err, v, m, d)
		}
	}
	// The current instance and released failure are excluded; stopped elastic
	// CPU/memory are free on the node, but its disk remains reserved.
	check(18, "17", "12")
	exec("INSERT INTO instances VALUES(5,1,1,'elastic','stopped','stopped','ready',NULL,NULL,3,4,2,NULL,1)")
	check(17, "16", "10")
	exec("DELETE FROM instances WHERE id=5")
	exec("UPDATE nodes SET resident_vcpu_budget=7,resident_memory_budget_bytes=9,resident_disk_budget_bytes=14")
	check(5, "6", "9")
	exec("UPDATE user_limits SET max_vcpus=3,max_memory_bytes=4,max_disk_bytes=6")
	check(3, "4", "6")
	// Unknown and unsafe failed records must not lose their reservation.
	exec("UPDATE instances SET provisioning_state='unknown' WHERE id=2")
	check(0, "0", "2")
	exec("UPDATE instances SET provisioning_state='failed',incus_uuid='still-exists' WHERE id=2")
	check(0, "0", "2")
	exec("UPDATE instances SET incus_uuid=NULL WHERE id=2")
	check(3, "4", "6")
	exec("UPDATE instances SET deleted_at=NOW(6) WHERE id=4")
	exec("UPDATE user_limits SET max_vcpus=100000,max_memory_bytes=9007199254740993,max_disk_bytes=9007199254740993")
	exec("UPDATE nodes SET allocatable_vcpus=100000,resident_vcpu_budget=100000,allocatable_memory_bytes=9007199254740993,resident_memory_budget_bytes=9007199254740993,allocatable_disk_bytes=9007199254740993,resident_disk_budget_bytes=9007199254740993")
	check(65535, "9007199254740993", "9007199254740990")
	for _, tc := range []struct{ change, restore, code string }{
		{"UPDATE users SET status='disabled' WHERE id=1", "UPDATE users SET status='active' WHERE id=1", "owner_inactive"},
		{"UPDATE nodes SET health='offline'", "UPDATE nodes SET health='online'", "node_unavailable"},
		{"UPDATE nodes SET connection_verified_at=NULL", "UPDATE nodes SET connection_verified_at=NOW(6)", "node_unavailable"},
		{"UPDATE nodes SET resources_sampled_at=NOW(6)-INTERVAL 2 MINUTE", "UPDATE nodes SET resources_sampled_at=NOW(6)", "node_unavailable"},
		{"UPDATE user_limits SET max_instances=0", "UPDATE user_limits SET max_instances=20", "quota_exceeded"},
		{"UPDATE user_limits SET max_resident_instances=0", "UPDATE user_limits SET max_resident_instances=20", "quota_exceeded"},
		{"DELETE FROM user_limits", "INSERT INTO user_limits VALUES(1,20,20,20,20,20)", "quota_not_configured"},
	} {
		exec(tc.change)
		_, err := resizeMaxResources(a, db, r)
		api, ok := err.(*platform.HTTPError)
		if !ok || api.Code != tc.code {
			t.Fatalf("%s: got %v", tc.change, err)
		}
		exec(tc.restore)
	}
	var count int64
	if err := db.Table("instances").Where("revision<>1").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("preview mutated revisions: %d %v", count, err)
	}
}

func TestMySQLResizeLimitsOwnershipAndPrivacy(t *testing.T) {
	db, a := admissionMySQLFixture(t)
	if err := db.Exec("ALTER TABLE instances ADD allowed_user_actions JSON").Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		user    uint64
		admin   bool
		actions string
		status  int
	}{
		{"owner allowed", 1, false, `["resize"]`, 200},
		{"owner denied", 1, false, `[]`, 403},
		{"other hidden", 2, false, `["resize"]`, 404},
		{"admin for owner", 2, true, `[]`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.Exec("UPDATE instances SET allowed_user_actions=? WHERE id=1", tc.actions).Error; err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set("user", platform.User{ID: tc.user})
				c.Set("admin", tc.admin)
			})
			router.GET("/instances/:id/resize-limits", platform.Handle(func(c *gin.Context) (any, error) { return resizeLimits(c, a) }))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/instances/1/resize-limits", nil))
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.status == 200 {
				var body struct {
					Data map[string]json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Data) != 1 || body.Data["max_resources"] == nil {
					t.Fatalf("unexpected public payload: %s (%v)", w.Body.String(), err)
				}
				if strings.Contains(w.Body.String(), "used") || strings.Contains(w.Body.String(), "user_id") {
					t.Fatalf("preview leaked usage: %s", w.Body.String())
				}
			}
		})
	}
}
