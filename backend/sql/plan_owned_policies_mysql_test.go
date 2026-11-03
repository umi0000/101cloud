package migrations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestMySQLPlanOwnedPolicyMigrationPreservesHistory(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated policy cutover regression")
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
	name := "ci_plan_owned_" + hex.EncodeToString(nonce[:])
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
		`CREATE TABLE policy_versions(id BIGINT UNSIGNED PRIMARY KEY,kind VARCHAR(32),source_id BIGINT UNSIGNED,config JSON) ENGINE=InnoDB`,
		`CREATE TABLE platform_policy_bindings(kind VARCHAR(32) PRIMARY KEY,policy_id BIGINT UNSIGNED) ENGINE=InnoDB`,
		`CREATE TABLE policy_binding_sets(id BIGINT UNSIGNED PRIMARY KEY,revision BIGINT) ENGINE=InnoDB`,
		`CREATE TABLE nodes(id BIGINT UNSIGNED PRIMARY KEY,revision BIGINT,updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)) ENGINE=InnoDB`,
		`CREATE TABLE instance_plans(id BIGINT UNSIGNED PRIMARY KEY,image_type VARCHAR(24),gpu_model VARCHAR(255),gpu_access_mode VARCHAR(16),pricing_version_id BIGINT UNSIGNED,credit_policy_version_id BIGINT UNSIGNED,idle_policy_version_id BIGINT UNSIGNED NOT NULL,policy_versions JSON,revision BIGINT,updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)) ENGINE=InnoDB`,
		`CREATE TABLE instances(id BIGINT UNSIGNED PRIMARY KEY,image_type VARCHAR(24),pricing_version_id BIGINT UNSIGNED,credit_policy_version_id BIGINT UNSIGNED,idle_policy_version_id BIGINT UNSIGNED NOT NULL,policy_versions JSON,cpu_hourly_pt DECIMAL(20,6),revision BIGINT,state VARCHAR(24),config JSON,updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)) ENGINE=InnoDB`,
		`CREATE TABLE gpu_price_versions(id BIGINT UNSIGNED PRIMARY KEY,gpu_model VARCHAR(255),access_mode VARCHAR(16),gpu_hourly_pt DECIMAL(20,6)) ENGINE=InnoDB`,
		`CREATE TABLE node_gpus(id BIGINT UNSIGNED PRIMARY KEY,gpu_price_version_id BIGINT UNSIGNED,status VARCHAR(24),KEY ix_node_gpus_price(gpu_price_version_id),CONSTRAINT fk_node_gpus_price FOREIGN KEY(gpu_price_version_id) REFERENCES gpu_price_versions(id),CONSTRAINT ck_node_gpus_available_price CHECK(status<>'available' OR gpu_price_version_id IS NOT NULL)) ENGINE=InnoDB`,
		`CREATE TABLE gpu_allocations(id BIGINT UNSIGNED PRIMARY KEY,instance_id BIGINT UNSIGNED,gpu_price_version_id BIGINT UNSIGNED,gpu_hourly_pt DECIMAL(20,6),active_slot INT,revision BIGINT) ENGINE=InnoDB`,
		`CREATE TABLE credit_enforcements(id BIGINT UNSIGNED PRIMARY KEY,account_id BIGINT UNSIGNED,policy_version_id BIGINT UNSIGNED,status VARCHAR(16),shutdown_deadline DATETIME(6),policy_snapshot JSON,revision BIGINT,active_account_id BIGINT UNSIGNED GENERATED ALWAYS AS (CASE WHEN status IN ('grace','stopping','unknown') THEN account_id ELSE NULL END) STORED,UNIQUE KEY uq_credit_enforcement_active(active_account_id)) ENGINE=InnoDB`,
		`CREATE TABLE credit_enforcement_instances(id BIGINT PRIMARY KEY,enforcement_id BIGINT,instance_id BIGINT,status VARCHAR(24),revision BIGINT) ENGINE=InnoDB`,
		`CREATE TABLE credit_accounts(id BIGINT PRIMARY KEY,credit_policy_version_id BIGINT,grant_cursor_at DATETIME(6),revision BIGINT) ENGINE=InnoDB`,
		`CREATE TABLE credit_buckets(id BIGINT PRIMARY KEY,remaining_pt DECIMAL(20,6),reserved_pt DECIMAL(20,6),expires_at DATETIME(6),revision BIGINT) ENGINE=InnoDB`,
		`CREATE TABLE credit_ledger(id BIGINT PRIMARY KEY,amount_delta_pt DECIMAL(20,6),revision BIGINT) ENGINE=InnoDB`,
		`CREATE TABLE credit_daily_grants(id BIGINT PRIMARY KEY,policy_version_id BIGINT,revision BIGINT) ENGINE=InnoDB`,
		`CREATE TABLE usage_sessions(id BIGINT PRIMARY KEY,gpu_price_version_id BIGINT,gpu_hourly_pt DECIMAL(20,6),revision BIGINT) ENGINE=InnoDB`,
		`CREATE TABLE metric_gpu_high_windows(node_id BIGINT UNSIGNED,gpu_uuid VARCHAR(128),high_since DATETIME(6),PRIMARY KEY(node_id,gpu_uuid)) ENGINE=InnoDB`,
		`INSERT INTO policy_versions VALUES(1,'pricing',1,JSON_OBJECT()),(2,'gpu-pricing',1,JSON_OBJECT()),(3,'credit',1,JSON_OBJECT()),(4,'gpu-idle',1,JSON_OBJECT()),(5,'gpu-scheduling',NULL,JSON_OBJECT()),(6,'metrics',NULL,JSON_OBJECT()),(7,'pricing',2,JSON_OBJECT()),(8,'credit',2,JSON_OBJECT()),(9,'gpu-pricing',2,JSON_OBJECT())`,
		`INSERT INTO platform_policy_bindings VALUES('pricing',7),('credit',8),('gpu-idle',4),('gpu-scheduling',5),('metrics',6),('gpu-pricing',9)`,
		`INSERT INTO policy_binding_sets VALUES(1,33)`,
		`INSERT INTO nodes VALUES(1,42,'2026-10-01')`,
		`INSERT INTO instance_plans VALUES(1,'container','A100','exclusive',1,1,1,'{"gpu-pricing":"9","metrics":"6"}',17,'2026-10-01'),(2,'virtual-machine',NULL,NULL,1,1,1,JSON_OBJECT(),18,'2026-10-01')`,
		`INSERT INTO instances VALUES(1,'container',1,1,1,JSON_OBJECT(),3.25,19,'running','{"preserve":true}','2026-10-01'),(2,'container',1,1,1,'{"pricing":"7","credit":"8","gpu-pricing":"9"}',7,20,'stopped',JSON_OBJECT(),'2026-10-01')`,
		`INSERT INTO gpu_price_versions VALUES(1,'A100','shared',0),(2,'A100','shared',4)`,
		`INSERT INTO node_gpus VALUES(1,1,'available')`,
		`INSERT INTO gpu_allocations VALUES(1,1,1,0,1,23),(2,1,2,4,NULL,24)`,
		`INSERT INTO credit_enforcements(id,account_id,policy_version_id,status,shutdown_deadline,policy_snapshot,revision) VALUES(1,1,1,'grace','2026-11-03 12:00:00','{"shutdown_grace_seconds":7200}',25)`,
		`INSERT INTO credit_enforcement_instances VALUES(1,1,1,'pending',26)`,
		`INSERT INTO credit_ledger VALUES(1,-1.25,27)`,
		`INSERT INTO credit_accounts VALUES(1,2,'2026-11-02 06:30:00',30)`,
		`INSERT INTO credit_buckets VALUES(1,100.25,2.5,'2026-11-04 06:30:00',31)`,
		`INSERT INTO credit_daily_grants VALUES(1,1,28)`,
		`INSERT INTO usage_sessions VALUES(1,1,0,29)`,
		`INSERT INTO metric_gpu_high_windows VALUES(1,'GPU-1','2026-10-01')`,
	)
	source, err := files.ReadFile("migrations/20261103_20_plan_owned_policies.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements, err := Statements(string(source))
	if err != nil {
		t.Fatal(err)
	}
	// Unsafe legacy inputs must fail while all historical/default tables remain.
	guard := func(fragment string) {
		t.Helper()
		for _, statement := range statements {
			if strings.Contains(statement, fragment) {
				if err := db.WithContext(ctx).Exec(statement).Error; err == nil {
					t.Fatalf("migration guard accepted unsafe input: %s", fragment)
				}
				return
			}
		}
		t.Fatalf("migration guard missing: %s", fragment)
	}
	exec(`INSERT INTO gpu_allocations VALUES(3,1,2,4,1,32)`)
	guard("multiple active GPU pricing sources")
	exec(`DELETE FROM gpu_allocations WHERE id=3`, `DELETE FROM platform_policy_bindings WHERE kind='metrics'`)
	guard("existing explicit metrics binding for populated nodes")
	exec(`INSERT INTO platform_policy_bindings VALUES('metrics',6)`)
	exec(statements...)
	for query, want := range map[string]int64{
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('platform_policy_bindings','policy_binding_sets')`: 0,
		`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='node_gpus' AND column_name='gpu_price_version_id'`:   0,
		`SELECT COUNT(*) FROM nodes WHERE metrics_policy_id=6 AND revision=42 AND updated_at='2026-10-01'`:                                                  1,
		`SELECT COUNT(*) FROM instance_plans WHERE id=1 AND revision=17 AND updated_at='2026-10-01' AND gpu_access_mode='exclusive' AND JSON_UNQUOTE(JSON_EXTRACT(policy_versions,'$."gpu-pricing"'))='9' AND JSON_UNQUOTE(JSON_EXTRACT(policy_versions,'$.pricing'))='1' AND JSON_UNQUOTE(JSON_EXTRACT(policy_versions,'$.credit'))='3'`:                                                                                                                                  1,
		`SELECT COUNT(*) FROM instances WHERE id=1 AND revision=19 AND updated_at='2026-10-01' AND state='running' AND cpu_hourly_pt=3.25 AND JSON_EXTRACT(config,'$.preserve')=CAST('true' AS JSON) AND JSON_UNQUOTE(JSON_EXTRACT(policy_versions,'$."gpu-pricing"'))='2' AND JSON_UNQUOTE(JSON_EXTRACT(policy_versions,'$.pricing'))='1' AND JSON_UNQUOTE(JSON_EXTRACT(policy_versions,'$.credit'))='3' AND JSON_UNQUOTE(JSON_EXTRACT(policy_versions,'$.metrics'))='6'`: 1,
		`SELECT COUNT(*) FROM instances WHERE id=2 AND revision=20 AND cpu_hourly_pt=7 AND JSON_UNQUOTE(JSON_EXTRACT(policy_versions,'$.pricing'))='7' AND JSON_UNQUOTE(JSON_EXTRACT(policy_versions,'$.credit'))='8' AND JSON_UNQUOTE(JSON_EXTRACT(policy_versions,'$."gpu-pricing"'))='9'`:                                                                                                                                                                               1,
		`SELECT COUNT(*) FROM gpu_allocations WHERE (id=1 AND gpu_price_version_id=1 AND gpu_hourly_pt=0 AND revision=23) OR (id=2 AND gpu_price_version_id=2 AND gpu_hourly_pt=4 AND revision=24)`:                                                                                                                                                                                                                                                                        2,
		`SELECT COUNT(*) FROM credit_enforcements WHERE id=1 AND policy_version_id=1 AND status='grace' AND shutdown_deadline='2026-11-03 12:00:00' AND JSON_EXTRACT(policy_snapshot,'$.shutdown_grace_seconds')=7200 AND revision=25`:                                                                                                                                                                                                                                     1,
		`SELECT COUNT(*) FROM credit_enforcement_instances WHERE id=1 AND enforcement_id=1 AND instance_id=1 AND status='pending' AND revision=26`:                                                                                                                                                                                                                                                                                                                         1,
		`SELECT COUNT(*) FROM credit_accounts WHERE id=1 AND credit_policy_version_id=2 AND grant_cursor_at='2026-11-02 06:30:00' AND revision=30`:                                                                                                                                                                                                                                                                                                                         1,
		`SELECT COUNT(*) FROM credit_buckets WHERE id=1 AND remaining_pt=100.25 AND reserved_pt=2.5 AND expires_at='2026-11-04 06:30:00' AND revision=31`:                                                                                                                                                                                                                                                                                                                  1,
		`SELECT COUNT(*) FROM credit_ledger WHERE id=1 AND amount_delta_pt=-1.25 AND revision=27`:                                                                                                                                                                                                                                                                                                                                                                          1,
		`SELECT COUNT(*) FROM credit_daily_grants WHERE id=1 AND policy_version_id=1 AND revision=28`:                                                                                                                                                                                                                                                                                                                                                                      1,
		`SELECT COUNT(*) FROM usage_sessions WHERE id=1 AND gpu_price_version_id=1 AND gpu_hourly_pt=0 AND revision=29`:                                                                                                                                                                                                                                                                                                                                                    1,
		`SELECT COUNT(*) FROM metric_gpu_high_windows`: 0,
	} {
		var got int64
		if err := db.WithContext(ctx).Raw(query).Scan(&got).Error; err != nil || got != want {
			t.Fatalf("%s: got=%d want=%d error=%v", query, got, want, err)
		}
	}
	// Distinct frozen instance policies may enforce one shared wallet concurrently.
	exec(`INSERT INTO credit_enforcements(id,account_id,policy_version_id,status,shutdown_deadline,policy_snapshot,revision) VALUES(2,1,2,'grace','2026-11-03 13:00:00',JSON_OBJECT(),1)`)
	if err := db.Exec(`INSERT INTO credit_enforcements(id,account_id,policy_version_id,status,shutdown_deadline,policy_snapshot,revision) VALUES(3,1,2,'grace','2026-11-03 14:00:00',JSON_OBJECT(),1)`).Error; err == nil {
		t.Fatal("duplicate active wallet/policy group accepted")
	}
}
