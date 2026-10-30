package install

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	migrations "101cloud/backend/sql"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var databaseName = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// Run is an explicit first-install operation. Administrative credentials never
// enter the runtime configuration, database, or application logs.
func Run(ctx context.Context, cfg platform.Config, adminDSN, accountHost string) error {
	if adminDSN == "" {
		return errors.New("CLOUD_MYSQL_ADMIN_DSN is required for install only")
	}
	app, e := driver.ParseDSN(cfg.DSN)
	if e != nil {
		return errors.New("invalid business MySQL DSN")
	}
	admin, e := driver.ParseDSN(adminDSN)
	if e != nil {
		return errors.New("invalid administrative MySQL DSN")
	}
	if !databaseName.MatchString(app.DBName) || app.User == "" || len(app.User) > 32 || app.Passwd == "" {
		return errors.New("business DSN requires an ASCII database name, a user (at most 32 bytes), and a nonempty password")
	}
	if strings.ContainsRune(app.User, 0) || strings.ContainsRune(app.Passwd, 0) {
		return errors.New("MySQL account credentials must not contain NUL bytes")
	}
	if strings.EqualFold(app.User, "root") || app.User == admin.User {
		return errors.New("business user must differ from the administrative user and must not be root")
	}
	if app.Net != admin.Net || app.Addr != admin.Addr {
		return errors.New("administrative and business DSNs must address the same MySQL endpoint")
	}
	if accountHost == "" {
		switch {
		case app.Net == "unix":
			accountHost = "localhost"
		case strings.HasPrefix(app.Addr, "127.0.0.1:"):
			accountHost = "127.0.0.1"
		default:
			return errors.New("CLOUD_MYSQL_APP_HOST is required for non-loopback TCP; it identifies the backend client host, not the MySQL server")
		}
	}
	if len(accountHost) > 255 || strings.TrimSpace(accountHost) != accountHost || strings.ContainsAny(accountHost, "\x00\r\n") {
		return errors.New("invalid CLOUD_MYSQL_APP_HOST")
	}
	admin.DBName = ""
	admin.MultiStatements = false
	admin.InterpolateParams = false
	admin.Timeout = 10 * time.Second
	admin.ReadTimeout = 30 * time.Second
	admin.WriteTimeout = 30 * time.Second
	db, e := sql.Open("mysql", admin.FormatDSN())
	if e != nil {
		return errors.New("cannot open administrative MySQL connection")
	}
	defer db.Close()
	conn, e := db.Conn(ctx)
	if e != nil {
		return errors.New("cannot connect to MySQL with administrative credentials")
	}
	defer conn.Close()
	var locked int
	if e = conn.QueryRowContext(ctx, "SELECT GET_LOCK(SHA2(?,256),30)", "101cloud.install."+app.DBName).Scan(&locked); e != nil || locked != 1 {
		return errors.New("cannot acquire first-install database lock")
	}
	defer conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(SHA2(?,256))", "101cloud.install."+app.DBName)
	var tables, users int
	if e = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=?", app.DBName).Scan(&tables); e != nil {
		return errors.New("cannot inspect target database")
	}
	if tables != 0 {
		return errors.New("target database is not empty; install refuses to overwrite it; use migrate for an existing installation")
	}
	if e = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.user WHERE User=? AND Host=?", app.User, accountHost).Scan(&users); e != nil {
		return errors.New("administrative account must be able to inspect MySQL users")
	}
	if users != 0 {
		return errors.New("business MySQL account already exists; install will not replace its password or privileges")
	}
	if _, e = conn.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS `"+app.DBName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); e != nil {
		return errors.New("cannot create target database")
	}
	// Use the admin identity for schema work, on the same target database. The
	// ordinary business connection is not opened until grants have been applied.
	schemaConfig := cfg
	admin.DBName = app.DBName
	schemaConfig.DSN = admin.FormatDSN()
	schema, e := platform.OpenDB(schemaConfig)
	if e != nil {
		return fmt.Errorf("schema connection failed after database creation; inspect the partial install: %w", e)
	}
	schemaPool, e := schema.DB()
	if e != nil {
		return e
	}
	defer schemaPool.Close()
	if e = migrations.Up(ctx, schema); e != nil {
		return fmt.Errorf("schema initialization failed; DDL may be committed and is not automatically deleted: %w", e)
	}
	// MySQL account identifiers do not accept prepared-statement placeholders.
	// Fix escaping semantics on this dedicated connection before quoting values.
	if _, e = conn.ExecContext(ctx, "SET SESSION sql_mode='NO_BACKSLASH_ESCAPES,STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION'"); e != nil {
		return errors.New("cannot establish safe MySQL account statement quoting")
	}
	account := sqlLiteral(app.User) + "@" + sqlLiteral(accountHost)
	if _, e = conn.ExecContext(ctx, "CREATE USER "+account+" IDENTIFIED BY "+sqlLiteral(app.Passwd)); e != nil {
		return errors.New("tables initialized but business user creation failed; inspect MySQL before retrying; existing objects are retained")
	}
	if _, e = conn.ExecContext(ctx, "GRANT SELECT, INSERT, UPDATE, DELETE ON `"+app.DBName+"`.* TO "+account); e != nil {
		return errors.New("business account created but grants failed; inspect and repair its grants explicitly")
	}
	// Verify with the actual application identity, never by continuing to use DBA.
	runtime, e := gorm.Open(mysql.Open(app.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		return errors.New("business account was created but cannot connect; check CLOUD_MYSQL_APP_HOST and authentication")
	}
	runtimePool, e := runtime.DB()
	if e != nil {
		return e
	}
	defer runtimePool.Close()
	if e = runtimePool.PingContext(ctx); e != nil {
		return errors.New("business account connection verification failed")
	}
	return nil
}

func sqlLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
