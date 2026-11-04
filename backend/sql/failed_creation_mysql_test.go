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

func TestMySQLFailedCreationMigrationPreservesAuditAndForeignKeys(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated creation retirement migration regression")
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
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "ci_failed_creation_migration_" + hex.EncodeToString(nonce[:])
	if _, err = owner.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
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
	for _, statement := range []string{
		`CREATE TABLE instances(id BIGINT UNSIGNED PRIMARY KEY,revision BIGINT,desired_state VARCHAR(24),deleted_at DATETIME(6),incus_config_snapshot JSON) ENGINE=InnoDB`,
		`CREATE TABLE tasks(id BIGINT UNSIGNED PRIMARY KEY,instance_id BIGINT UNSIGNED,task_type VARCHAR(64),status VARCHAR(24),FOREIGN KEY(instance_id) REFERENCES instances(id)) ENGINE=InnoDB`,
		`INSERT INTO instances VALUES(1,42,'running',NULL,'{"keep":true}'),(2,43,'deleted',NOW(6),JSON_OBJECT())`,
		`INSERT INTO tasks VALUES(10,1,'instance.create','failed')`,
	} {
		if err = db.WithContext(ctx).Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	source, err := files.ReadFile("migrations/20261104_00_failed_creation_cleanup.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements, err := Statements(string(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if err = db.WithContext(ctx).Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	var preserved int64
	if err = db.Raw(`SELECT COUNT(*) FROM instances WHERE creation_failed_at IS NULL AND creation_failure_task_id IS NULL AND creation_cleanup_state IS NULL AND ((id=1 AND revision=42 AND desired_state='running' AND deleted_at IS NULL AND incus_config_snapshot=JSON_OBJECT('keep',CAST('true' AS JSON))) OR (id=2 AND revision=43 AND desired_state='deleted' AND deleted_at IS NOT NULL))`).Scan(&preserved).Error; err != nil || preserved != 2 {
		t.Fatalf("migration changed legacy records: %d %v", preserved, err)
	}
	if err = db.Exec(`UPDATE instances SET creation_failed_at=NOW(6),creation_failure_task_id=10,creation_cleanup_state='pending',desired_state='deleted' WHERE id=1`).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(`DELETE FROM tasks WHERE id=10`).Error; err == nil {
		t.Fatal("retirement lost original failure audit foreign key")
	}
	if err = db.Exec(`UPDATE instances SET creation_cleanup_state='reusable' WHERE id=1`).Error; err == nil {
		t.Fatal("invalid cleanup state accepted")
	}
	if err = db.Exec(`UPDATE instances SET creation_failure_task_id=999 WHERE id=1`).Error; err == nil {
		t.Fatal("nonexistent original task accepted")
	}
	var physical int64
	if err = db.Raw(`SELECT COUNT(*) FROM instances WHERE id=1 AND deleted_at IS NULL AND desired_state='deleted'`).Scan(&physical).Error; err != nil || physical != 1 {
		t.Fatal("hiding retirement prematurely claimed physical deletion")
	}
}
