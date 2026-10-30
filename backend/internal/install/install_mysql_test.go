package install

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	driver "github.com/go-sql-driver/mysql"
)

func TestMySQLFirstInstallAndOverwriteGuards(t *testing.T) {
	adminDSN := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if adminDSN == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated first-install regression")
	}
	admin, e := driver.ParseDSN(adminDSN)
	if e != nil {
		t.Fatal(e)
	}
	admin.DBName = ""
	var nonce [8]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		t.Fatal(e)
	}
	suffix := hex.EncodeToString(nonce[:])
	database := "ci_install_" + suffix
	username := "ci_" + suffix
	app := *admin
	app.User = username
	app.Passwd = "test'\\;@" + suffix
	app.DBName = database
	cfg := platform.Config{DSN: app.FormatDSN()}
	host := os.Getenv("CLOUD_TEST_MYSQL_APP_HOST")
	if host == "" {
		if app.Net == "unix" {
			host = "localhost"
		} else {
			host = "127.0.0.1"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	owner, e := sql.Open("mysql", admin.FormatDSN())
	if e != nil {
		t.Fatal(e)
	}
	defer owner.Close()
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		c, e := owner.Conn(cleanupCtx)
		if e != nil {
			t.Error(e)
			return
		}
		defer c.Close()
		_, _ = c.ExecContext(cleanupCtx, "SET SESSION sql_mode='NO_BACKSLASH_ESCAPES'")
		if _, e = c.ExecContext(cleanupCtx, "DROP USER IF EXISTS "+sqlLiteral(username)+"@"+sqlLiteral(host)); e != nil {
			t.Error(e)
		}
		if _, e = c.ExecContext(cleanupCtx, "DROP DATABASE IF EXISTS `"+database+"`"); e != nil {
			t.Error(e)
		}
	}()
	if e = Run(ctx, cfg, adminDSN, host); e != nil {
		t.Fatal(e)
	}
	runtime, e := platform.OpenDB(cfg)
	if e != nil {
		t.Fatal(e)
	}
	pool, e := runtime.DB()
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	if _, e = pool.ExecContext(ctx, "INSERT INTO availability_zones(code,name,status) VALUES('guard','Guard zone','active')"); e != nil {
		t.Fatalf("business DML not granted: %v", e)
	}
	_, e = pool.ExecContext(ctx, "CREATE TABLE should_be_denied(id INT)")
	if e == nil {
		t.Fatal("business account received DDL privileges")
	}
	if e = Run(ctx, cfg, adminDSN, host); e == nil {
		t.Fatal("repeated first install overwrote existing schema")
	}
	var name string
	if e = pool.QueryRowContext(ctx, "SELECT name FROM availability_zones WHERE code='guard'").Scan(&name); e != nil || name != "Guard zone" {
		t.Fatalf("existing business data changed: %q %v", name, e)
	}
	app.DBName = database + "_blocked"
	if e = Run(ctx, platform.Config{DSN: app.FormatDSN()}, adminDSN, host); e == nil {
		t.Fatal("existing MySQL account was replaced")
	}
	var created int
	if e = owner.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name=?", app.DBName).Scan(&created); e != nil || created != 0 {
		t.Fatalf("account collision created a partial target database: %d %v", created, e)
	}
}
