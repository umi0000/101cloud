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

func TestMySQLConsoleNoFixedExpiryMigration(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated console migration regression")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	cfg.ParseTime = true
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
			name := "ci_console_" + hex.EncodeToString(nonce[:])
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
			executeFile := func(path string, selectConsole bool) {
				t.Helper()
				source, err := files.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				statements, err := Statements(string(source))
				if err != nil {
					t.Fatal(err)
				}
				for _, statement := range statements {
					if selectConsole && !strings.HasPrefix(statement, "CREATE TABLE console_sessions (") {
						continue
					}
					if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "fresh" {
				if err := Up(ctx, db); err != nil {
					t.Fatal(err)
				}
				if err := Ready(ctx, db); err != nil {
					t.Fatal(err)
				}
				// The new migration must not remove the independent login expiry.
				var loginExpiry int64
				if err := db.WithContext(ctx).Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='user_sessions' AND column_name='expires_at'").Scan(&loginExpiry).Error; err != nil || loginExpiry != 1 {
					t.Fatalf("login expiry changed: columns=%d error=%v", loginExpiry, err)
				}
			} else {
				// Use the immutable original console definition, including its real
				// checks and foreign keys, with minimal referenced domain tables.
				for _, table := range []string{"users", "instances", "nodes", "tasks"} {
					if err := db.WithContext(ctx).Exec("CREATE TABLE `" + table + "` (id BIGINT UNSIGNED PRIMARY KEY) ENGINE=InnoDB").Error; err != nil {
						t.Fatal(err)
					}
					if err := db.WithContext(ctx).Exec("INSERT INTO `" + table + "` VALUES(1)").Error; err != nil {
						t.Fatal(err)
					}
				}
				executeFile("migrations/20261030_50_monitoring.sql", true)
				for _, status := range []string{"preparing", "ready", "active", "closed", "failed", "expired"} {
					if err := db.WithContext(ctx).Exec("INSERT INTO console_sessions(id,user_id,auth_session_id,instance_id,node_id,instance_owner_id,is_admin,kind,status,cols,`rows`,secrets_ciphertext,prepare_task_id,closed_at,expires_at,revision,created_at,updated_at) VALUES(?,1,2,1,1,1,FALSE,'terminal',?,80,24,'history',1,'2026-10-30 12:00:00','2026-10-30 12:10:00',7,'2026-10-30 11:00:00','2026-10-30 12:00:00')", status, status).Error; err != nil {
						t.Fatal(err)
					}
				}
				executeFile("migrations/20261031_90_console_no_fixed_expiry.sql", false)
				for _, status := range []string{"preparing", "ready", "active", "closed", "failed", "expired"} {
					var count int64
					if err := db.WithContext(ctx).Table("console_sessions").Where("id=? AND status=? AND revision=7 AND secrets_ciphertext='history' AND closed_at='2026-10-30 12:00:00' AND created_at='2026-10-30 11:00:00'", status, status).Count(&count).Error; err != nil || count != 1 {
						t.Fatalf("migration changed %s history: count=%d error=%v", status, count, err)
					}
				}
			}
			var expiryColumns, expiryIndexes int64
			if err := db.WithContext(ctx).Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='console_sessions' AND column_name='expires_at'").Scan(&expiryColumns).Error; err != nil || expiryColumns != 0 {
				t.Fatalf("console expiry column remains: count=%d error=%v", expiryColumns, err)
			}
			if err := db.WithContext(ctx).Raw("SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='console_sessions' AND index_name IN ('ix_console_user_status_expiry','ix_console_status_expiry')").Scan(&expiryIndexes).Error; err != nil || expiryIndexes != 0 {
				t.Fatalf("expiry indexes remain: count=%d error=%v", expiryIndexes, err)
			}
			var indexed []string
			if err := db.WithContext(ctx).Raw("SELECT column_name FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='console_sessions' AND index_name='ix_console_user_status' ORDER BY seq_in_index").Scan(&indexed).Error; err != nil || strings.Join(indexed, ",") != "user_id,status" {
				t.Fatalf("quota index changed: columns=%v error=%v", indexed, err)
			}
			var historyCheck string
			if err := db.WithContext(ctx).Raw("SELECT check_clause FROM information_schema.check_constraints WHERE constraint_schema=DATABASE() AND constraint_name='ck_console_status'").Scan(&historyCheck).Error; err != nil || !strings.Contains(historyCheck, "expired") {
				t.Fatalf("historical expired status no longer accepted: clause=%q error=%v", historyCheck, err)
			}
		})
	}
}
