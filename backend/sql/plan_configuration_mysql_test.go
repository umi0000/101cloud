package migrations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestMySQLPlanConfigurationMigrationPreservesLegacyInstances(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated plan migration regression")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	owner, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "ci_plan_config_" + hex.EncodeToString(nonce[:])
	if _, err := owner.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := owner.ExecContext(cleanup, "DROP DATABASE `"+name+"`"); err != nil {
			t.Error(err)
		}
	}()
	cfg.DBName = name
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	exec := func(statements ...string) {
		t.Helper()
		for _, statement := range statements {
			if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
	}
	exec(
		`CREATE TABLE nodes(id BIGINT UNSIGNED PRIMARY KEY) ENGINE=InnoDB`,
		`CREATE TABLE instance_plans(id BIGINT UNSIGNED PRIMARY KEY,image_type VARCHAR(24),gpu_max_hourly_pt DECIMAL(20,6),policy_versions JSON,revision BIGINT) ENGINE=InnoDB`,
		`CREATE TABLE node_network_bindings(id BIGINT UNSIGNED PRIMARY KEY,node_id BIGINT UNSIGNED) ENGINE=InnoDB`,
		`CREATE TABLE plan_network_options(plan_id BIGINT UNSIGNED,network_option_id BIGINT UNSIGNED) ENGINE=InnoDB`,
		`CREATE TABLE policy_versions(id BIGINT UNSIGNED PRIMARY KEY,kind VARCHAR(24),config JSON) ENGINE=InnoDB`,
		`CREATE TABLE instances(id BIGINT UNSIGNED PRIMARY KEY,product_type VARCHAR(24),max_gpu_hourly_pt DECIMAL(20,6),allowed_user_actions JSON,gpu_user_permissions JSON,gpu_policy JSON,revision BIGINT) ENGINE=InnoDB`,
		`INSERT INTO nodes VALUES(1),(2)`,
		`INSERT INTO node_network_bindings VALUES(1,1),(2,1),(3,2)`,
		`INSERT INTO instance_plans VALUES(1,'container',3.25,'{"gpu-idle":"7"}',9),(2,'virtual-machine',NULL,JSON_OBJECT(),4)`,
		`INSERT INTO plan_network_options VALUES(1,1),(1,2),(1,3),(2,1)`,
		`INSERT INTO policy_versions VALUES(7,'gpu-idle','{"default_timeout_seconds":600,"allowed_actions":["stop_instance"]}')`,
		`INSERT INTO instances VALUES(1,'elastic',2.5,JSON_ARRAY(),JSON_OBJECT(),'{"idle":{"enabled":true},"rebalance":{"enabled":true}}',12),(2,'elastic',1.25,NULL,NULL,NULL,13),(3,'custom',0,NULL,NULL,NULL,14)`,
	)
	source, err := files.ReadFile("migrations/20261102_10_plan_configuration.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements, err := Statements(string(source))
	if err != nil {
		t.Fatal(err)
	}
	exec(statements...)
	for query, want := range map[string]int64{
		`SELECT COUNT(*) FROM plan_nodes`:                                      3,
		`SELECT COUNT(*) FROM plan_nodes WHERE plan_id=1 AND node_id IN (1,2)`: 2,
		`SELECT COUNT(*) FROM instance_plans WHERE id=1 AND revision=9 AND JSON_TYPE(JSON_EXTRACT(gpu_policy,'$.idle.enabled'))='BOOLEAN' AND JSON_EXTRACT(gpu_policy,'$.idle.enabled')=CAST('false' AS JSON) AND JSON_EXTRACT(gpu_policy,'$.idle.timeout_seconds')=600 AND JSON_UNQUOTE(JSON_EXTRACT(gpu_policy,'$.idle.action'))='stop_instance' AND JSON_UNQUOTE(JSON_EXTRACT(gpu_policy,'$.rebalance.max_hourly_pt'))='3.250000' AND JSON_LENGTH(allowed_user_actions)=10 AND JSON_EXTRACT(gpu_user_permissions,'$.idle')=CAST('true' AS JSON)`: 1,
		`SELECT COUNT(*) FROM instance_plans WHERE id=2 AND revision=4 AND gpu_max_hourly_pt=0 AND JSON_LENGTH(allowed_user_actions)=6 AND JSON_LENGTH(gpu_user_permissions)=0`:                                                                                                                                          1,
		`SELECT COUNT(*) FROM instances WHERE id=1 AND revision=12 AND JSON_LENGTH(allowed_user_actions)=0 AND JSON_LENGTH(gpu_user_permissions)=0 AND JSON_EXTRACT(gpu_policy,'$.idle.enabled')=CAST('true' AS JSON) AND JSON_EXTRACT(gpu_policy,'$.rebalance.enabled')=CAST('true' AS JSON) AND max_gpu_hourly_pt=2.5`: 1,
		`SELECT COUNT(*) FROM instances WHERE id=2 AND revision=13 AND JSON_LENGTH(allowed_user_actions)=10 AND JSON_TYPE(JSON_EXTRACT(gpu_user_permissions,'$.idle'))='BOOLEAN' AND JSON_UNQUOTE(JSON_EXTRACT(gpu_policy,'$.rebalance.max_hourly_pt'))='1.250000'`:                                                      1,
		`SELECT COUNT(*) FROM instances WHERE id=3 AND revision=14 AND allowed_user_actions IS NULL AND gpu_user_permissions IS NULL AND gpu_policy IS NULL`:                                                                                                                                                             1,
	} {
		var got int64
		if err := db.WithContext(ctx).Raw(query).Scan(&got).Error; err != nil || got != want {
			t.Fatalf("%s: got=%d want=%d error=%v", query, got, want, err)
		}
	}
}
