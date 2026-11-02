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

func TestMySQLEditablePlansMigrationFreezesInstances(t *testing.T) {
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
	name := "ci_editable_" + hex.EncodeToString(nonce[:])
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
		`CREATE TABLE instance_plans(id BIGINT PRIMARY KEY,status VARCHAR(24),revision BIGINT,vcpus BIGINT UNSIGNED,memory_bytes BIGINT UNSIGNED,disk_bytes BIGINT UNSIGNED,allowed_overrides JSON,CONSTRAINT ck_instance_plans_status CHECK(status IN ('draft','active','disabled','retired'))) ENGINE=InnoDB`,
		`CREATE TABLE instances(id BIGINT PRIMARY KEY,instance_plan_id BIGINT,product_type VARCHAR(24),revision BIGINT,state VARCHAR(24),config JSON) ENGINE=InnoDB`,
		`INSERT INTO instance_plans VALUES(1,'active',7,2,4294967296,21474836480,'{"vcpus":{"min":1,"max":4}}'),(2,'draft',8,3,8,9,JSON_OBJECT()),(3,'retired',9,4,8,9,JSON_OBJECT()),(4,'active',10,5,8,9,JSON_OBJECT())`,
		`INSERT INTO instances VALUES(1,1,'elastic',42,'running','{"keep":true}'),(2,NULL,'custom',43,'stopped',JSON_OBJECT())`,
	)
	source, err := files.ReadFile("migrations/20261102_20_editable_plans.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements, err := Statements(string(source))
	if err != nil {
		t.Fatal(err)
	}
	exec(statements...)
	for _, query := range []string{
		`SELECT COUNT(*) FROM instance_plans WHERE status='active'`,
		`SELECT COUNT(*) FROM instance_plans WHERE status='disabled'`,
	} {
		var count int64
		if err := db.Raw(query).Scan(&count).Error; err != nil || count != 2 {
			t.Fatalf("%s: %d %v", query, count, err)
		}
	}
	var count int64
	query := `SELECT COUNT(*) FROM instances WHERE id=1 AND revision=42 AND state='running' AND JSON_EXTRACT(config,'$.keep')=CAST('true' AS JSON) AND plan_snapshot=JSON_OBJECT('revision','7','vcpus',2,'memory_bytes','4294967296','disk_bytes','21474836480','allowed_overrides',JSON_OBJECT('vcpus',JSON_OBJECT('min',1,'max',4)))`
	if err := db.Raw(query).Scan(&count).Error; err != nil || count != 1 {
		t.Fatalf("snapshot: %d %v", count, err)
	}
	exec(`UPDATE instance_plans SET vcpus=99,allowed_overrides=JSON_OBJECT(),revision=revision+1 WHERE id=1`)
	if err := db.Raw(query).Scan(&count).Error; err != nil || count != 1 {
		t.Fatalf("snapshot mutated: %d %v", count, err)
	}
	if err := db.Raw(`SELECT COUNT(*) FROM instances WHERE id=2 AND plan_snapshot IS NULL AND revision=43`).Scan(&count).Error; err != nil || count != 1 {
		t.Fatalf("custom: %d %v", count, err)
	}
	if err := db.Exec(`UPDATE instance_plans SET status='draft' WHERE id=1`).Error; err == nil {
		t.Fatal("draft accepted")
	}
	if err := db.Exec(`INSERT INTO instances VALUES(3,999,'elastic',1,'running',JSON_OBJECT(),NULL)`).Error; err == nil {
		t.Fatal("unfrozen elastic accepted")
	}
}
