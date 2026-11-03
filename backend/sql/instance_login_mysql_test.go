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

func TestMySQLInstanceLoginMigrationPreservesLegacyInstances(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated login migration regression")
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
	name := "ci_login_migration_" + hex.EncodeToString(nonce[:])
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
		`CREATE TABLE instances(id BIGINT PRIMARY KEY,revision BIGINT,state VARCHAR(24),incus_config_snapshot JSON) ENGINE=InnoDB`,
		`INSERT INTO instances VALUES(1,42,'running','{"keep":true}'),(2,43,'stopped',JSON_OBJECT())`,
	} {
		if err = db.WithContext(ctx).Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	source, err := files.ReadFile("migrations/20261103_00_instance_login.sql")
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
	if err = db.Raw(`SELECT COUNT(*) FROM instances WHERE login_config_ciphertext IS NULL AND login_initialized_at IS NULL AND ((id=1 AND revision=42 AND state='running' AND incus_config_snapshot=JSON_OBJECT('keep',CAST('true' AS JSON))) OR (id=2 AND revision=43 AND state='stopped' AND incus_config_snapshot=JSON_OBJECT()))`).Scan(&preserved).Error; err != nil || preserved != 2 {
		t.Fatalf("migration changed existing instances: count=%d err=%v", preserved, err)
	}
	// Old clients need not supply either new column; binary ciphertext is not
	// coerced through a text charset and completion can be checkpointed alone.
	if err = db.Exec(`INSERT INTO instances(id,revision,state,incus_config_snapshot) VALUES(3,1,'pending',JSON_OBJECT())`).Error; err != nil {
		t.Fatal(err)
	}
	sealed := []byte{0, 255, 128, 42}
	if err = db.Exec(`UPDATE instances SET login_config_ciphertext=?,login_initialized_at=NOW(6) WHERE id=3`, sealed).Error; err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err = db.Raw(`SELECT login_config_ciphertext FROM instances WHERE id=3 AND login_initialized_at IS NOT NULL`).Row().Scan(&stored); err != nil || string(stored) != string(sealed) {
		t.Fatal("dedicated encrypted login field did not preserve binary ciphertext")
	}
}
