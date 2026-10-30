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

func TestMySQLCustomRequestRetirement(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated schema migration regression")
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
			name := "ci_retire_" + hex.EncodeToString(nonce[:])
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
			if mode == "fresh" {
				if err := Up(ctx, db); err != nil {
					t.Fatal(err)
				}
				if err := Ready(ctx, db); err != nil {
					t.Fatal(err)
				}
			} else {
				// A focused pre-upgrade fixture retains outbound request references and
				// live domain rows, without bypassing any foreign-key checks.
				for _, statement := range []string{
					"CREATE TABLE users(id BIGINT PRIMARY KEY)",
					"CREATE TABLE instances(id BIGINT PRIMARY KEY,user_id BIGINT NOT NULL,product_type VARCHAR(24) NOT NULL,observed_state VARCHAR(24) NOT NULL,custom_reason TEXT,FOREIGN KEY(user_id) REFERENCES users(id))",
					"CREATE TABLE tasks(id BIGINT PRIMARY KEY,instance_id BIGINT NOT NULL,FOREIGN KEY(instance_id) REFERENCES instances(id))",
					"CREATE TABLE audit_logs(id BIGINT PRIMARY KEY,event_type VARCHAR(128) NOT NULL)",
					"CREATE TABLE custom_instance_requests(id BIGINT PRIMARY KEY,user_id BIGINT NOT NULL,instance_id BIGINT,status VARCHAR(24),FOREIGN KEY(user_id) REFERENCES users(id),FOREIGN KEY(instance_id) REFERENCES instances(id))",
					"CREATE TABLE residency_requests(id BIGINT PRIMARY KEY,instance_id BIGINT NOT NULL,FOREIGN KEY(instance_id) REFERENCES instances(id))",
					"INSERT INTO users VALUES(1)",
					"INSERT INTO instances VALUES(1,1,'custom','running','historic reason')",
					"INSERT INTO tasks VALUES(1,1)",
					"INSERT INTO audit_logs VALUES(1,'instance.created')",
					"INSERT INTO custom_instance_requests VALUES(1,1,NULL,'cancelled')",
				} {
					if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
						t.Fatal(err)
					}
				}
				migration, err := files.ReadFile("migrations/20261030_80_retire_custom_requests.sql")
				if err != nil {
					t.Fatal(err)
				}
				statements, err := Statements(string(migration))
				if err != nil {
					t.Fatal(err)
				}
				for _, statement := range statements {
					if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
						t.Fatal(err)
					}
				}
				for _, table := range []string{"users", "instances", "tasks", "audit_logs"} {
					var count int64
					if err := db.WithContext(ctx).Table(table).Where("id=1").Count(&count).Error; err != nil || count != 1 {
						t.Fatalf("retirement changed %s: count=%d error=%v", table, count, err)
					}
				}
				var state string
				if err := db.WithContext(ctx).Raw("SELECT observed_state FROM instances WHERE id=1 AND product_type='custom' AND user_id=1").Scan(&state).Error; err != nil || state != "running" {
					t.Fatalf("running custom instance changed: state=%q error=%v", state, err)
				}
			}
			for _, table := range []string{"custom_instance_requests", "residency_requests"} {
				exists, err := hasTable(ctx, db, table)
				if err != nil || exists {
					t.Fatalf("obsolete table %s remains: exists=%t error=%v", table, exists, err)
				}
			}
			var columns int64
			if err := db.WithContext(ctx).Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='instances' AND column_name='custom_reason'").Scan(&columns).Error; err != nil || columns != 0 {
				t.Fatalf("obsolete custom_reason remains: count=%d error=%v", columns, err)
			}
		})
	}
}
