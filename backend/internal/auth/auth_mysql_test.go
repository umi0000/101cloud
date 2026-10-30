package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

// Explicit opt-in against a caller-owned, fully migrated MySQL test database.
// Only this invocation's randomly scoped rate-limit buckets are written/deleted.
func TestMySQLLoginRateLimitBinaryBuckets(t *testing.T) {
	dsn := os.Getenv("CLOUD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_DSN to a migrated MySQL test database")
	}
	db, err := platform.OpenDB(platform.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	a := testAuthApp()
	a.DB = db
	unique, err := randomToken(32)
	if err != nil {
		t.Fatal(err)
	}
	username := "auth-rate-test:" + unique
	digest := sha256.Sum256([]byte(unique))
	address := net.IP(digest[:16]).String()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "https://cloud.example/api/v1/auth/login", nil).WithContext(ctx)
	if remaining := a.Time().Truncate(15 * time.Minute).Add(15 * time.Minute).Sub(a.Time()); remaining < 30*time.Second {
		t.Skip("fixed rate-limit window is about to roll over")
	}
	c.Request.RemoteAddr = net.JoinHostPort(address, "54321")
	ipKey := keyed(a, "login-rate:ip:"+c.ClientIP())
	userKey := keyed(a, "login-rate:username:"+strings.ToLower(username))
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := pool.ExecContext(cleanupCtx, "DELETE FROM auth_rate_limits WHERE bucket_key IN (?,?)", ipKey, userKey); err != nil {
			t.Errorf("clean up rate-limit buckets: %v", err)
		}
	}()
	// This exercises the actual GORM INSERT path: a []byte argument after '(' in
	// raw SQL used to expand into 32 placeholders and fail with MySQL error 1136.
	for attempt := 1; attempt <= 10; attempt++ {
		if err := loginLimit(c, a, username); err != nil {
			t.Fatalf("attempt %d unexpectedly failed: %v", attempt, err)
		}
	}
	err = loginLimit(c, a, username)
	var rejection *platform.HTTPError
	if !errors.As(err, &rejection) || rejection.Status != http.StatusTooManyRequests || rejection.Code != "rate_limited" {
		t.Fatalf("11th attempt did not enforce account limit: %v", err)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("rate-limit rejection omitted Retry-After")
	}
	for _, key := range [][]byte{ipKey, userKey} {
		var attempts, keyBytes uint64
		if err := pool.QueryRowContext(ctx, "SELECT attempts,OCTET_LENGTH(bucket_key) FROM auth_rate_limits WHERE bucket_key=?", key).Scan(&attempts, &keyBytes); err != nil {
			t.Fatal(err)
		}
		if attempts != 11 || keyBytes != 32 {
			t.Fatalf("persisted bucket attempts=%d key bytes=%d, want 11 and 32", attempts, keyBytes)
		}
	}
}
