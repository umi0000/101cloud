package migrations

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

const zoneRemoval = "migrations/20261031_95_remove_availability_zones.sql"

func TestMySQLAvailabilityZoneRemoval(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated availability zone migration regression")
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
	for _, mode := range []string{"fresh", "upgrade"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			var nonce [8]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				t.Fatal(err)
			}
			name := "ci_zones_" + hex.EncodeToString(nonce[:])
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
			app := *cfg
			app.DBName = name
			db, err := gorm.Open(mysql.Open(app.FormatDSN()), &gorm.Config{})
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
			if mode == "fresh" {
				if err := Up(ctx, db); err != nil {
					t.Fatal(err)
				}
				exec("INSERT INTO network_pools(id,name,address_family,network_address,prefix_length,range_start,range_end,dns_servers,status) VALUES(1,'lan',4,INET6_ATON('192.0.2.0'),24,INET6_ATON('192.0.2.10'),INET6_ATON('192.0.2.20'),JSON_ARRAY(),'active')")
			} else {
				// Apply and journal every earlier migration exactly as Up would, so
				// Up below runs only the zone removal against live zoned rows. One
				// connection keeps session settings and temporary guard tables intact.
				pool.SetMaxOpenConns(1)
				exec("CREATE TABLE backend_schema_migrations(name VARCHAR(255) PRIMARY KEY,checksum CHAR(64) NOT NULL,applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)) ENGINE=InnoDB")
				ns, err := names()
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range ns {
					if file == zoneRemoval {
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
					// Up skips the metrics migration when init.sql already created it.
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
				exec(
					"INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'zone-admin','unused-test-hash','admin','active')",
					"INSERT INTO pricing_versions(id,code,version,cpu_hourly_pt,memory_gib_hourly_pt,disk_gib_hourly_pt,status,effective_at,created_by_user_id) VALUES(1,'base','1',0,0,0,'active','2026-10-30 00:00:00',1)",
					"INSERT INTO credit_policy_versions(id,code,version,daily_allowance_pt,bonus_cap_pt,timezone,reset_time,billing_window_seconds,status,effective_at,created_by_user_id) VALUES(1,'base','1',0,0,'Asia/Shanghai','00:00:00',60,'active','2026-10-30 00:00:00',1)",
					"INSERT INTO idle_policy_versions(id,code,version,sample_interval_seconds,evaluation_window_seconds,consecutive_idle_windows,cpu_utilization_threshold_pct,gpu_utilization_threshold_pct,gpu_memory_threshold_pct,network_bytes_threshold,action,status,effective_at,created_by_user_id) VALUES(1,'base','1',60,60,1,0,0,0,0,'release_gpu','active','2026-10-30 00:00:00',1)",
					"INSERT INTO availability_zones(id,code,name,status) VALUES(1,'studio-local','Studio','active')",
					"INSERT INTO nodes(id,availability_zone_id,name,agent_endpoint,token_ciphertext,key_version,server_cert_sha256,default_storage_pool,default_project_name,allocatable_vcpus,allocatable_memory_bytes,allocatable_disk_bytes,resident_vcpu_budget,resident_memory_budget_bytes,resident_disk_budget_bytes,status,administrative_state,health,revision) VALUES(1,1,'node-a','https://node-a.example:8443','cipher',1,UNHEX(SHA2('node-cert',256)),'default','default',4,1,1,4,1,1,'active','active','online',7)",
					"INSERT INTO network_pools(id,availability_zone_id,name,address_family,network_address,prefix_length,range_start,range_end,dns_servers,status,revision) VALUES(1,1,'lan',4,INET6_ATON('192.0.2.0'),24,INET6_ATON('192.0.2.10'),INET6_ATON('192.0.2.20'),JSON_ARRAY(),'active',5)",
					"INSERT INTO instance_plans(id,availability_zone_id,code,version,name,image_type,vcpus,memory_bytes,disk_bytes,gpu_count,pricing_version_id,credit_policy_version_id,idle_policy_version_id,init_parameters,allow_residency,status,revision) VALUES(1,1,'basic','1','Basic','container',1,1,1,0,1,1,1,JSON_OBJECT(),FALSE,'draft',3)",
				)
				if err := Up(ctx, db); err != nil {
					t.Fatal(err)
				}
				for _, check := range []struct{ table, where string }{
					{"nodes", "id=1 AND name='node-a' AND administrative_state='active' AND revision=7"},
					{"network_pools", "id=1 AND name='lan' AND network_address=INET6_ATON('192.0.2.0') AND revision=5"},
					{"instance_plans", "id=1 AND code='basic' AND name='Basic' AND pricing_version_id=1 AND revision=3"},
				} {
					var count int64
					if err := db.WithContext(ctx).Table(check.table).Where(check.where).Count(&count).Error; err != nil || count != 1 {
						t.Fatalf("zone removal changed %s: count=%d error=%v", check.table, count, err)
					}
				}
			}
			if err := Ready(ctx, db); err != nil {
				t.Fatal(err)
			}
			err = db.WithContext(ctx).Exec("INSERT INTO network_pools(id,name,address_family,network_address,prefix_length,range_start,range_end,dns_servers,status) VALUES(2,'lan',4,INET6_ATON('198.51.100.0'),24,INET6_ATON('198.51.100.10'),INET6_ATON('198.51.100.20'),JSON_ARRAY(),'active')").Error
			var duplicate *driver.MySQLError
			if !errors.As(err, &duplicate) || duplicate.Number != 1062 {
				t.Fatalf("duplicate pool name accepted: %v", err)
			}
			exists, err := hasTable(ctx, db, "availability_zones")
			if err != nil || exists {
				t.Fatalf("availability_zones remains: exists=%t error=%v", exists, err)
			}
			var columns, references int64
			if err := db.WithContext(ctx).Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND column_name='availability_zone_id'").Scan(&columns).Error; err != nil || columns != 0 {
				t.Fatalf("zone columns remain: count=%d error=%v", columns, err)
			}
			if err := db.WithContext(ctx).Raw("SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND index_name IN ('ix_nodes_zone_status','uq_network_pools_zone_name','ix_network_pools_zone_status','ix_instance_plans_zone_name')").Scan(&references).Error; err != nil || references != 0 {
				t.Fatalf("zone indexes remain: count=%d error=%v", references, err)
			}
			for _, index := range []struct{ table, name, columns string }{
				{"nodes", "ix_nodes_status", "status"},
				{"network_pools", "uq_network_pools_name", "name"},
				{"network_pools", "ix_network_pools_status", "status"},
				{"instance_plans", "ix_instance_plans_name", "name"},
			} {
				var indexed []string
				if err := db.WithContext(ctx).Raw("SELECT column_name FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=? ORDER BY seq_in_index", index.table, index.name).Scan(&indexed).Error; err != nil || strings.Join(indexed, ",") != index.columns {
					t.Fatalf("%s.%s: columns=%v error=%v", index.table, index.name, indexed, err)
				}
			}
		})
	}
}
