package platform

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

// Explicit opt-in: this test uses only constant SELECTs and never changes a
// schema or data, so it is safe alongside a caller-owned MySQL test database.
func TestMySQLTimeRoundTrip(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_DSN for real MySQL time round-trip")
	}
	db, e := OpenDB(Config{DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, e := pool.Conn(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer first.Close()
	second, e := pool.Conn(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer second.Close()
	for _, conn := range []interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}{first, second} {
		var value time.Time
		var zone string
		if e = conn.QueryRowContext(ctx, "SELECT CAST('2026-01-02 03:04:05.123456' AS DATETIME(6)),@@session.time_zone").Scan(&value, &zone); e != nil {
			t.Fatal(e)
		}
		_, offset := value.Zone()
		if offset != 28800 || zone != "+08:00" || value.UTC().Format(time.RFC3339Nano) != "2026-01-01T19:04:05.123456Z" {
			t.Fatalf("DATETIME wall time changed: %s, session %s", value.Format(time.RFC3339Nano), zone)
		}
	}
}
