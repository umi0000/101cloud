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

func TestMySQLStoppedResourcesFreeMigration(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated stopped-resource migration regression")
	}
	for _, legacyCheck := range []bool{false, true} {
		name := "without_legacy_check"
		if legacyCheck {
			name = "with_legacy_check"
		}
		t.Run(name, func(t *testing.T) {
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
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			var nonce [8]byte
			if _, err = rand.Read(nonce[:]); err != nil {
				t.Fatal(err)
			}
			database := "ci_stopped_resources_" + hex.EncodeToString(nonce[:])
			if _, err = owner.ExecContext(ctx, "CREATE DATABASE `"+database+"`"); err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
				defer stop()
				if _, err := owner.ExecContext(cleanup, "DROP DATABASE `"+database+"`"); err != nil {
					t.Error(err)
				}
			}()
			cfg.DBName = database
			db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			exec := func(statement string) {
				t.Helper()
				if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			for _, statement := range []string{
				`CREATE TABLE credit_policy_versions(id BIGINT PRIMARY KEY,storage_charge_mode VARCHAR(24),daily_allowance_pt DECIMAL(20,6),revision BIGINT,updated_at DATETIME(6))`,
				`CREATE TABLE policy_versions(id BIGINT PRIMARY KEY,kind VARCHAR(32),config JSON,revision BIGINT,updated_at DATETIME(6))`,
				`CREATE TABLE credit_enforcements(id BIGINT PRIMARY KEY,status VARCHAR(24),shutdown_deadline DATETIME(6),policy_snapshot JSON,revision BIGINT,updated_at DATETIME(6))`,
				`CREATE TABLE instance_plans(id BIGINT PRIMARY KEY,policy_versions JSON)`,
				`CREATE TABLE instances(id BIGINT PRIMARY KEY,policy_versions JSON,plan_snapshot JSON)`,
				`CREATE TABLE usage_sessions(id BIGINT PRIMARY KEY,config_snapshot JSON,started_at DATETIME(6),last_settled_at DATETIME(6),cumulative_expected_pt DECIMAL(20,6))`,
				`CREATE TABLE credit_accounts(id BIGINT PRIMARY KEY,remaining_pt DECIMAL(20,6))`,
				`CREATE TABLE credit_ledger(id BIGINT PRIMARY KEY,amount_delta_pt DECIMAL(20,6))`,
				`INSERT INTO credit_policy_versions VALUES(1,'retained',7,10,'2026-11-03'),(2,'running_only',8,20,'2026-11-03'),(3,'free',9,30,'2026-11-03')`,
				`INSERT INTO policy_versions VALUES(101,'credit',JSON_OBJECT('storage_charge_mode','retained','offline_charge_mode','continue'),10,'2026-11-03'),(102,'credit',JSON_OBJECT('storage_charge_mode','running_only'),20,'2026-11-03'),(103,'credit',JSON_OBJECT('storage_charge_mode','free'),30,'2026-11-03'),(104,'metrics',JSON_OBJECT('storage_charge_mode','retained'),40,'2026-11-03')`,
				`INSERT INTO credit_enforcements VALUES(1,'grace','2026-11-04 12:10:00',JSON_OBJECT('storage_charge_mode','retained','grace_charge_mode','accrue'),10,'2026-11-03'),(2,'stopping','2026-11-04 12:10:00',JSON_OBJECT('storage_charge_mode','retained'),20,'2026-11-03'),(3,'unknown','2026-11-04 12:10:00',JSON_OBJECT('storage_charge_mode','retained'),30,'2026-11-03'),(4,'completed','2026-11-04 12:10:00',JSON_OBJECT('storage_charge_mode','retained'),40,'2026-11-03')`,
				`INSERT INTO instance_plans VALUES(1,JSON_OBJECT('credit','101'))`,
				`INSERT INTO instances VALUES(1,JSON_OBJECT('credit','101'),JSON_OBJECT('vcpus',1,'allowed_overrides',JSON_OBJECT()))`,
				`INSERT INTO usage_sessions VALUES(1,JSON_OBJECT('vcpus',1,'disk_bytes','1073741824'),'2026-11-03 12:00:00','2026-11-03 12:01:00',1.25)`,
				`INSERT INTO credit_accounts VALUES(1,98.75)`,
				`INSERT INTO credit_ledger VALUES(1,-1.25)`,
			} {
				exec(statement)
			}
			if legacyCheck {
				exec(`ALTER TABLE credit_policy_versions ADD CONSTRAINT legacy_storage CHECK(storage_charge_mode IN ('retained','running_only','free'))`)
				exec(`ALTER TABLE policy_versions ADD CONSTRAINT legacy_config_storage CHECK(kind <> 'credit' OR JSON_UNQUOTE(JSON_EXTRACT(config,'$.storage_charge_mode')) IN ('retained','running_only','free'))`)
			}
			source, err := files.ReadFile("migrations/20261104_10_stopped_resources_free.sql")
			if err != nil {
				t.Fatal(err)
			}
			statements, err := Statements(string(source))
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range statements {
				exec(statement)
			}
			for query, want := range map[string]int64{
				`SELECT COUNT(*) FROM credit_policy_versions WHERE storage_charge_mode='running_only'`:                                                                                                                                                             2,
				`SELECT COUNT(*) FROM credit_policy_versions WHERE id=1 AND daily_allowance_pt=7 AND revision=11 AND updated_at='2026-11-03'`:                                                                                                                      1,
				`SELECT COUNT(*) FROM credit_policy_versions WHERE (id=2 AND revision=20) OR (id=3 AND storage_charge_mode='free' AND revision=30)`:                                                                                                                2,
				`SELECT COUNT(*) FROM policy_versions WHERE kind='credit' AND JSON_UNQUOTE(JSON_EXTRACT(config,'$.storage_charge_mode'))='running_only'`:                                                                                                           2,
				`SELECT COUNT(*) FROM policy_versions WHERE id=101 AND revision=11 AND updated_at='2026-11-03' AND JSON_UNQUOTE(JSON_EXTRACT(config,'$.offline_charge_mode'))='continue'`:                                                                          1,
				`SELECT COUNT(*) FROM policy_versions WHERE id=104 AND revision=40 AND JSON_UNQUOTE(JSON_EXTRACT(config,'$.storage_charge_mode'))='retained'`:                                                                                                      1,
				`SELECT COUNT(*) FROM credit_enforcements WHERE id IN (1,2,3) AND JSON_UNQUOTE(JSON_EXTRACT(policy_snapshot,'$.storage_charge_mode'))='running_only' AND shutdown_deadline='2026-11-04 12:10:00' AND revision=id*10+1 AND updated_at='2026-11-03'`: 3,
				`SELECT COUNT(*) FROM credit_enforcements WHERE id=1 AND status='grace' AND JSON_UNQUOTE(JSON_EXTRACT(policy_snapshot,'$.grace_charge_mode'))='accrue'`:                                                                                            1,
				`SELECT COUNT(*) FROM credit_enforcements WHERE id=4 AND revision=40 AND JSON_UNQUOTE(JSON_EXTRACT(policy_snapshot,'$.storage_charge_mode'))='retained'`:                                                                                           1,
				`SELECT COUNT(*) FROM instance_plans WHERE policy_versions=JSON_OBJECT('credit','101')`:                                                                                                                                                            1,
				`SELECT COUNT(*) FROM instances WHERE policy_versions=JSON_OBJECT('credit','101') AND plan_snapshot=JSON_OBJECT('vcpus',1,'allowed_overrides',JSON_OBJECT())`:                                                                                      1,
				`SELECT COUNT(*) FROM usage_sessions WHERE config_snapshot=JSON_OBJECT('vcpus',1,'disk_bytes','1073741824') AND started_at='2026-11-03 12:00:00' AND last_settled_at='2026-11-03 12:01:00' AND cumulative_expected_pt=1.25`:                        1,
				`SELECT COUNT(*) FROM credit_accounts WHERE remaining_pt=98.75`:                                                                                                                                                                                    1,
				`SELECT COUNT(*) FROM credit_ledger WHERE amount_delta_pt=-1.25`:                                                                                                                                                                                   1,
			} {
				var got int64
				if err := db.WithContext(ctx).Raw(query).Scan(&got).Error; err != nil || got != want {
					t.Errorf("%s: got %d, want %d: %v", query, got, want, err)
				}
			}
			for _, mode := range []string{"retained", "unknown"} {
				if err := db.Exec("UPDATE credit_policy_versions SET storage_charge_mode=? WHERE id=1", mode).Error; err == nil {
					t.Errorf("source accepted obsolete/invalid storage mode %s", mode)
				}
				if err := db.Exec("UPDATE policy_versions SET config=JSON_SET(config,'$.storage_charge_mode',?) WHERE id=101", mode).Error; err == nil {
					t.Errorf("public credit config accepted obsolete/invalid storage mode %s", mode)
				}
			}
			for _, mode := range []string{"running_only", "free"} {
				if err := db.Exec("UPDATE credit_policy_versions SET storage_charge_mode=? WHERE id=1", mode).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Exec("UPDATE policy_versions SET config=JSON_SET(config,'$.storage_charge_mode',?) WHERE id=101", mode).Error; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
