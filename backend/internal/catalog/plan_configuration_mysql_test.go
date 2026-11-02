package catalog

import (
	"context"
	"net/http/httptest"
	"testing"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

func TestMySQLPlanPlacementRequiresAllNodeProjectTargets(t *testing.T) {
	_, root := openCatalogMySQL(t, "ci_plan_targets_")
	db := root.Begin()
	if db.Error != nil {
		t.Fatal(db.Error)
	}
	t.Cleanup(func() { db.Rollback() })
	for table, definition := range map[string]string{
		"nodes":                 "id BIGINT PRIMARY KEY,administrative_state VARCHAR(24),health VARCHAR(24),connection_verified_at DATETIME(6),resources JSON",
		"images":                "id BIGINT PRIMARY KEY,status VARCHAR(24),image_type VARCHAR(24),properties JSON",
		"image_nodes":           "id BIGINT PRIMARY KEY,image_id BIGINT,node_id BIGINT,project_name VARCHAR(64),architecture VARCHAR(24),fingerprint BINARY(32),gpu_ready BOOL,cloud_init BOOL,properties JSON,status VARCHAR(24),last_verified_at DATETIME(6)",
		"node_network_bindings": "id BIGINT PRIMARY KEY,node_id BIGINT,project_name VARCHAR(64),network_pool_id BIGINT,status VARCHAR(24),validation_status VARCHAR(24),mode VARCHAR(24)",
		"network_pools":         "id BIGINT PRIMARY KEY,status VARCHAR(24)",
	} {
		if err := db.Exec("CREATE TEMPORARY TABLE `" + table + "` (" + definition + ") ENGINE=InnoDB").Error; err != nil {
			t.Fatal(err)
		}
	}
	exec := func(statement string) {
		t.Helper()
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO nodes VALUES(1,'active','online',NOW(6),'{"architecture":"x86_64"}'),(2,'active','online',NOW(6),'{"architecture":"x86_64"}')`)
	exec(`INSERT INTO images VALUES(1,'active','container',JSON_OBJECT())`)
	exec(`INSERT INTO network_pools VALUES(1,'active')`)
	exec(`INSERT INTO node_network_bindings VALUES(1,1,'tenant-a',1,'active','verified','bridge'),(2,2,'tenant-b',1,'active','verified','bridge')`)
	exec(`INSERT INTO image_nodes VALUES(1,1,1,'tenant-a','x86_64',UNHEX(REPEAT('aa',32)),TRUE,TRUE,JSON_OBJECT(),'ready',NOW(6)),(2,1,2,'tenant-b','x86_64',UNHEX(REPEAT('bb',32)),TRUE,TRUE,JSON_OBJECT(),'pending',NULL)`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/plans", nil).WithContext(context.Background())
	a := platform.New(db, platform.Config{})
	model := "NVIDIA A100"
	b := planInput{InstanceType: "container", NodeIDs: []string{"1", "2"}, ImageIDs: []string{"1"}, NetworkOptionIDs: []string{"1", "2"}, GPU: GPURequest{Model: &model}}
	if err := validatePlanPlacement(c, a, b, false); err != nil {
		t.Fatalf("draft with preparation target rejected: %v", err)
	}
	if err := validatePlanPlacement(c, a, b, true); err == nil {
		t.Fatal("publication accepted an unverified image target")
	}
	exec(`UPDATE image_nodes SET status='ready',last_verified_at=NOW(6) WHERE id=2`)
	if err := validatePlanPlacement(c, a, b, true); err != nil {
		t.Fatalf("compatible published placement rejected: %v", err)
	}
	for name, change := range map[string]func(*planInput){
		"missing-nodes":     func(b *planInput) { b.NodeIDs = nil },
		"outside-allowlist": func(b *planInput) { b.NodeIDs = []string{"1"} },
		"uncovered-node":    func(b *planInput) { b.NetworkOptionIDs = []string{"1"} },
		"duplicate-node":    func(b *planInput) { b.NodeIDs = []string{"1", "1"} },
		"duplicate-image":   func(b *planInput) { b.ImageIDs = []string{"1", "1"} },
		"duplicate-network": func(b *planInput) { b.NetworkOptionIDs = []string{"1", "1"} },
	} {
		t.Run(name, func(t *testing.T) {
			input := b
			change(&input)
			requireValidation(t, validatePlanPlacement(c, a, input, false))
		})
	}
	for name, statement := range map[string]string{
		"wrong-project":      `UPDATE image_nodes SET project_name='default' WHERE id=2`,
		"wrong-architecture": `UPDATE image_nodes SET architecture='aarch64' WHERE id=2`,
		"gpu-not-ready":      `UPDATE image_nodes SET gpu_ready=FALSE WHERE id=2`,
		"disabled-node":      `UPDATE nodes SET administrative_state='disabled' WHERE id=2`,
		"unverified-network": `UPDATE node_network_bindings SET validation_status='pending' WHERE id=2`,
	} {
		t.Run(name, func(t *testing.T) {
			exec(statement)
			if err := validatePlanPlacement(c, a, b, true); err == nil {
				t.Fatal("publication accepted incompatible placement")
			}
			exec(`UPDATE image_nodes SET project_name='tenant-b',architecture='x86_64',gpu_ready=TRUE WHERE id=2`)
			exec(`UPDATE nodes SET administrative_state='active' WHERE id=2`)
			exec(`UPDATE node_network_bindings SET validation_status='verified' WHERE id=2`)
		})
	}
}
