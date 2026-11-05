package migrations

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

func TestMySQLRemoveGPUIdleThresholdsMigration(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated GPU idle migration regression")
	}
	for _, mode := range []string{"fresh", "upgrade"} {
		t.Run(mode, func(t *testing.T) {
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
			database := "ci_remove_idle_thresholds_" + hex.EncodeToString(nonce[:])
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
			pool.SetMaxOpenConns(1)
			exec := func(statement string) {
				t.Helper()
				if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			if mode == "upgrade" {
				// Preserve the real historical schema and journal: Up must apply only
				// the forward cutover to populated rows, not a rewritten init.sql.
				exec("CREATE TABLE backend_schema_migrations(name VARCHAR(255) PRIMARY KEY,checksum CHAR(64) NOT NULL,applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)) ENGINE=InnoDB")
				ns, err := names()
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range ns {
					if file == "migrations/20261105_00_remove_gpu_idle_thresholds.sql" {
						break
					}
					source, err := files.ReadFile(file)
					if err != nil {
						t.Fatal(err)
					}
					statements, err := Statements(string(source))
					if err != nil {
						t.Fatal(err)
					}
					for _, statement := range statements {
						u := strings.ToUpper(statement)
						if file == "migrations/20261029_instance_metrics.sql" || strings.HasPrefix(u, "CREATE DATABASE") || strings.HasPrefix(u, "USE ") {
							continue
						}
						exec(statement)
					}
					sum := sha256.Sum256(source)
					if err := db.WithContext(ctx).Exec("INSERT INTO backend_schema_migrations(name,checksum) VALUES(?,?)", file, hex.EncodeToString(sum[:])).Error; err != nil {
						t.Fatal(err)
					}
				}
				exec(`INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'idle-migration-admin','test-hash','admin','active')`)
				exec(`INSERT INTO idle_policy_versions(id,code,version,sample_interval_seconds,evaluation_window_seconds,consecutive_idle_windows,cpu_utilization_threshold_pct,gpu_utilization_threshold_pct,gpu_memory_threshold_pct,network_bytes_threshold,action,status,effective_at,created_by_user_id,revision,updated_at) VALUES(1,'idle-preserved','1',10,1800,2,17,5,10,4096,'release_gpu','active','2026-11-01',1,7,'2026-11-01')`)
				// Include both keys, either key individually, and an already-clean row.
				// Identical names inside unrelated kinds/nested objects must survive.
				exec(`INSERT INTO policy_versions(id,kind,code,version,source_id,config,status,effective_at,created_by_user_id,revision,updated_at) VALUES
(1,'gpu-idle','idle-both','1',1,'{"enabled":true,"default_timeout_seconds":1800,"min_timeout_seconds":60,"max_timeout_seconds":7200,"allowed_actions":["release_gpu","stop_instance"],"graceful_stop_timeout_seconds":60,"allow_force_stop":false,"gpu_utilization_threshold_pct":5,"gpu_memory_threshold_pct":10,"otherconfig":{"gpu_memory_threshold_pct":29}}','active','2026-11-01',1,10,'2026-11-01'),
(2,'gpu-idle','idle-util','1',NULL,'{"enabled":false,"gpu_utilization_threshold_pct":0}','draft',NULL,1,20,'2026-11-01'),
(3,'gpu-idle','idle-memory','1',NULL,'{"default_timeout_seconds":777,"gpu_memory_threshold_pct":100}','retired','2026-11-01',1,30,'2026-11-01'),
(4,'gpu-idle','idle-clean','1',NULL,'{"enabled":true,"default_timeout_seconds":1234}','active','2026-11-01',1,40,'2026-11-01'),
(5,'gpu-scheduling','scheduling-preserved','1',NULL,'{"source_high_load_pct":90,"target_low_load_pct":30,"gpu_utilization_threshold_pct":71,"gpu_memory_threshold_pct":72}','active','2026-11-01',1,50,'2026-11-01'),
(6,'metrics','metrics-preserved','1',NULL,'{"gpu_utilization_threshold_pct":81,"gpu_memory_threshold_pct":82}','active','2026-11-01',1,60,'2026-11-01')`)
			}
			if err := Up(ctx, db); err != nil {
				t.Fatal(err)
			}
			if err := Ready(ctx, db); err != nil {
				t.Fatal(err)
			}
			queries := map[string]int64{
				`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='idle_policy_versions' AND column_name IN ('gpu_utilization_threshold_pct','gpu_memory_threshold_pct')`:                     0,
				`SELECT COUNT(*) FROM information_schema.table_constraints WHERE constraint_schema=DATABASE() AND table_name='idle_policy_versions' AND constraint_name='ck_idle_policy_versions_thresholds' AND constraint_type='CHECK'`: 1,
			}
			if mode == "upgrade" {
				queries[`SELECT COUNT(*) FROM policy_versions WHERE kind='gpu-idle' AND JSON_CONTAINS_PATH(config,'one','$.gpu_utilization_threshold_pct','$.gpu_memory_threshold_pct')`] = 0
				queries[`SELECT COUNT(*) FROM policy_versions WHERE id=1 AND source_id=1 AND status='active' AND revision=11 AND updated_at='2026-11-01' AND config=CAST('{"enabled":true,"default_timeout_seconds":1800,"min_timeout_seconds":60,"max_timeout_seconds":7200,"allowed_actions":["release_gpu","stop_instance"],"graceful_stop_timeout_seconds":60,"allow_force_stop":false,"otherconfig":{"gpu_memory_threshold_pct":29}}' AS JSON)`] = 1
				queries[`SELECT COUNT(*) FROM policy_versions WHERE id=2 AND status='draft' AND revision=21 AND updated_at='2026-11-01' AND config=CAST('{"enabled":false}' AS JSON)`] = 1
				queries[`SELECT COUNT(*) FROM policy_versions WHERE id=3 AND status='retired' AND revision=31 AND config=CAST('{"default_timeout_seconds":777}' AS JSON)`] = 1
				queries[`SELECT COUNT(*) FROM policy_versions WHERE id=4 AND revision=40 AND updated_at='2026-11-01' AND config=CAST('{"enabled":true,"default_timeout_seconds":1234}' AS JSON)`] = 1
				queries[`SELECT COUNT(*) FROM policy_versions WHERE id=5 AND revision=50 AND updated_at='2026-11-01' AND config=CAST('{"source_high_load_pct":90,"target_low_load_pct":30,"gpu_utilization_threshold_pct":71,"gpu_memory_threshold_pct":72}' AS JSON)`] = 1
				queries[`SELECT COUNT(*) FROM policy_versions WHERE id=6 AND revision=60 AND updated_at='2026-11-01' AND config=CAST('{"gpu_utilization_threshold_pct":81,"gpu_memory_threshold_pct":82}' AS JSON)`] = 1
				queries[`SELECT COUNT(*) FROM idle_policy_versions WHERE id=1 AND code='idle-preserved' AND sample_interval_seconds=10 AND evaluation_window_seconds=1800 AND consecutive_idle_windows=2 AND cpu_utilization_threshold_pct=17 AND network_bytes_threshold=4096 AND action='release_gpu' AND status='active' AND revision=7 AND updated_at='2026-11-01'`] = 1
			}
			for query, want := range queries {
				var got int64
				if err := db.WithContext(ctx).Raw(query).Scan(&got).Error; err != nil || got != want {
					t.Errorf("%s: got %d, want %d: %v", query, got, want, err)
				}
			}
			if mode == "fresh" {
				exec(`INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'idle-fresh-admin','test-hash','admin','active')`)
				exec(`INSERT INTO idle_policy_versions(id,code,version,sample_interval_seconds,evaluation_window_seconds,consecutive_idle_windows,cpu_utilization_threshold_pct,network_bytes_threshold,action,status,effective_at,created_by_user_id) VALUES(1,'idle-fresh','1',1,60,1,0,0,'release_gpu','active','2026-11-01',1)`)
			}
			for _, cpu := range []int{-1, 101} {
				if err := db.WithContext(ctx).Exec("UPDATE idle_policy_versions SET cpu_utilization_threshold_pct=? WHERE id=1", cpu).Error; err == nil {
					t.Errorf("CPU threshold constraint accepted %d", cpu)
				}
			}
			for _, cpu := range []int{0, 100} {
				if err := db.WithContext(ctx).Exec("UPDATE idle_policy_versions SET cpu_utilization_threshold_pct=? WHERE id=1", cpu).Error; err != nil {
					t.Fatal(err)
				}
			}
			// The migration journal makes another upgrade a no-op.
			if err := Up(ctx, db); err != nil {
				t.Fatal(err)
			}
		})
	}
}
