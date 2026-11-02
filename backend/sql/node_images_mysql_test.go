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

const nodeImagesMigration = "migrations/20261102_00_node_images.sql"

func TestMySQLNodeImagesMigration(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated node image migration regression")
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
			name := "ci_node_images_" + hex.EncodeToString(nonce[:])
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
			pool.SetMaxOpenConns(1)
			exec := func(statements ...string) {
				t.Helper()
				for _, statement := range statements {
					if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
						t.Fatalf("%s: %v", statement, err)
					}
				}
			}
			count := func(query string, want int64) {
				t.Helper()
				var got int64
				if err := db.WithContext(ctx).Raw(query).Scan(&got).Error; err != nil || got != want {
					t.Fatalf("%s: got=%d want=%d error=%v", query, got, want, err)
				}
			}
			if mode == "upgrade" {
				exec("CREATE TABLE backend_schema_migrations(name VARCHAR(255) PRIMARY KEY,checksum CHAR(64) NOT NULL,applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)) ENGINE=InnoDB")
				ns, err := names()
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range ns {
					if file == nodeImagesMigration {
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
				exec(
					"INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'image-admin','unused','admin','active')",
					"INSERT INTO nodes(id,name,agent_endpoint,token_ciphertext,key_version,server_cert_sha256,default_storage_pool,default_project_name,allocatable_vcpus,allocatable_memory_bytes,allocatable_disk_bytes,resident_vcpu_budget,resident_memory_budget_bytes,resident_disk_budget_bytes,status,administrative_state,health) VALUES(1,'a','https://a.example:8443','cipher',1,UNHEX(SHA2('a',256)),'default','project-a',4,1024,1024,4,1024,1024,'active','active','online'),(2,'b','https://b.example:8443','cipher',1,UNHEX(SHA2('b',256)),'default','project-b',4,1024,1024,4,1024,1024,'active','maintenance','unknown')",
					"INSERT INTO images(id,code,version,name,source_type,source_url,fingerprint,architecture,image_type,gpu_ready,os_name,os_release,properties,status,created_by_user_id) VALUES(1,'local','1','Local','local',NULL,UNHEX(SHA2('local',256)),'x86_64','container',TRUE,'Alpine','3',JSON_OBJECT('cloud_init',TRUE,'custom','preserve'),'active',1),(2,'remote','1','Remote','remote','https://legacy.example',UNHEX(SHA2('remote',256)),'aarch64','container',FALSE,'Alpine','3',JSON_OBJECT(),'active',1)",
				)
				// Freeze a real instance row without manufacturing unrelated policy
				// fixtures. Foreign-key checks are restored before the migration;
				// all image/node/task references used by this migration are real.
				exec("SET FOREIGN_KEY_CHECKS=0",
					"INSERT INTO instances(id,user_id,node_id,image_id,instance_plan_id,logical_name,project_name,incus_name,image_type,architecture,image_fingerprint,vcpus,memory_bytes,disk_bytes,storage_pool,pricing_version_id,cpu_hourly_pt,memory_gib_hourly_pt,disk_gib_hourly_pt,credit_policy_version_id,billing_mode,desired_state,observed_state,provisioning_state,auto_release_gpu,auto_rebind_gpu,idle_policy_version_id,max_gpu_hourly_pt,init_parameters,incus_config_snapshot,allowed_user_actions,gpu_user_permissions,policy_versions,gpu_policy) VALUES(7,1,1,1,1,'existing','project-a','existing','container','aarch64',UNHEX(SHA2('frozen',256)),1,1,1,'default',1,0,0,0,1,'admin_exempt','running','running','ready',FALSE,FALSE,1,0,JSON_OBJECT(),JSON_OBJECT('preserve',TRUE),JSON_ARRAY(),JSON_OBJECT(),JSON_OBJECT(),JSON_OBJECT())",
					"SET FOREIGN_KEY_CHECKS=1",
					"INSERT INTO tasks(id,task_type,idempotency_scope,idempotency_key,user_id,node_id,instance_id,is_instance_mutation,status,parameters_redacted,result_redacted,revision) VALUES(43,'instance.create','existing','unknown43',1,1,7,1,'unknown',JSON_OBJECT('preserve',TRUE),JSON_OBJECT('operation','preserve'),9)")
			}
			if err := Up(ctx, db); err != nil {
				t.Fatal(err)
			}
			if err := Ready(ctx, db); err != nil {
				t.Fatal(err)
			}
			if mode == "fresh" {
				count("SELECT COUNT(*) FROM image_nodes", 0)
				count("SELECT COUNT(*) FROM tasks WHERE task_type='image.prepare'", 0)
			} else {
				count("SELECT COUNT(*) FROM image_nodes", 2)
				count("SELECT COUNT(*) FROM image_nodes x JOIN nodes n ON n.id=x.node_id JOIN images i ON i.id=x.image_id WHERE x.image_id=1 AND x.project_name=n.default_project_name AND x.architecture=i.architecture AND x.fingerprint=i.fingerprint AND x.gpu_ready=1 AND x.cloud_init=1 AND x.source_id IS NULL AND x.status='pending' AND x.last_verified_at IS NULL AND x.error IS NULL AND x.properties=i.properties", 2)
				count("SELECT COUNT(*) FROM image_nodes x JOIN tasks t ON t.id=x.task_id WHERE t.task_type='image.prepare' AND t.idempotency_scope='migration:20261102_00_node_images' AND t.idempotency_key=CAST(x.id AS CHAR) AND t.user_id=1 AND t.node_id=x.node_id AND t.instance_id IS NULL AND t.is_instance_mutation=0 AND t.status='queued' AND JSON_TYPE(JSON_EXTRACT(t.parameters_redacted,'$.image_node_id'))='STRING' AND JSON_UNQUOTE(JSON_EXTRACT(t.parameters_redacted,'$.image_node_id'))=CAST(x.id AS CHAR) AND JSON_LENGTH(t.parameters_redacted)=1", 2)
				count("SELECT COUNT(*) FROM instances WHERE id=7 AND architecture='aarch64' AND image_fingerprint=UNHEX(SHA2('frozen',256)) AND observed_state='running' AND JSON_EXTRACT(incus_config_snapshot,'$.preserve')=CAST('true' AS JSON)", 1)
				count("SELECT COUNT(*) FROM tasks WHERE id=43 AND status='unknown' AND instance_id=7 AND is_instance_mutation=1 AND revision=9 AND JSON_UNQUOTE(JSON_EXTRACT(result_redacted,'$.operation'))='preserve'", 1)
				// Exercise statement-level resume too, not only the journal skip.
				source, err := files.ReadFile(nodeImagesMigration)
				if err != nil {
					t.Fatal(err)
				}
				statements, err := Statements(string(source))
				if err != nil {
					t.Fatal(err)
				}
				exec(statements...)
				count("SELECT COUNT(*) FROM image_nodes", 2)
				count("SELECT COUNT(*) FROM tasks WHERE task_type='image.prepare'", 2)
				count("SELECT COUNT(*) FROM tasks WHERE id=43 AND status='unknown' AND revision=9", 1)
			}
			if err := Up(ctx, db); err != nil {
				t.Fatal(err)
			}
			count("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='image_nodes' AND column_name='error' AND data_type='json' AND is_nullable='YES'", 1)
		})
	}
}
