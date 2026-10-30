package platform

import (
	"encoding/base64"
	"errors"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func LoadConfig() (Config, error) {
	c := Config{Listen: env("CLOUD_LISTEN", "127.0.0.1:8080"), Origin: os.Getenv("CLOUD_ORIGIN"), DSN: os.Getenv("CLOUD_MYSQL_DSN"), SessionTTL: 12 * time.Hour, ConsoleTTL: 10 * time.Minute, ConsoleMaxConcurrent: 4}
	u, e := url.Parse(c.Origin)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return c, errors.New("CLOUD_ORIGIN must be canonical HTTPS origin")
	}
	if c.DSN == "" {
		return c, errors.New("CLOUD_MYSQL_DSN required")
	}
	keyPath := os.Getenv("CLOUD_MASTER_KEY_FILE")
	if keyPath == "" {
		return c, errors.New("CLOUD_MASTER_KEY_FILE required")
	}
	info, e := os.Stat(keyPath)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return c, errors.New("master key must be a regular 0600 file")
	}
	raw, e := os.ReadFile(keyPath)
	if e != nil {
		return c, errors.New("cannot read master key")
	}
	c.MasterKey, e = base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(raw)))
	if e != nil || len(c.MasterKey) != 32 {
		return c, errors.New("master key must contain base64 of 32 random bytes")
	}
	for name, target := range map[string]*time.Duration{"CLOUD_SESSION_TTL": &c.SessionTTL, "CLOUD_CONSOLE_TTL": &c.ConsoleTTL} {
		if text := os.Getenv(name); text != "" {
			v, e := time.ParseDuration(text)
			if e != nil || v <= 0 {
				return c, errors.New("invalid positive duration " + name)
			}
			*target = v
		}
	}
	if s := os.Getenv("CLOUD_CONSOLE_MAX_CONCURRENT"); s != "" {
		n, e := strconv.Atoi(s)
		if e != nil || n < 1 || n > 100 {
			return c, errors.New("invalid console concurrency")
		}
		c.ConsoleMaxConcurrent = n
	}
	for _, s := range strings.Split(env("CLOUD_NODE_ALLOW_CIDRS", "127.0.0.1/32,::1/128"), ",") {
		p, e := netip.ParsePrefix(strings.TrimSpace(s))
		if e != nil {
			return c, errors.New("invalid node CIDR")
		}
		c.NodeAllowCIDRs = append(c.NodeAllowCIDRs, p.Masked())
	}
	return c, nil
}
func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func OpenDB(c Config) (*gorm.DB, error) {
	cfg, e := driver.ParseDSN(c.DSN)
	if e != nil {
		return nil, errors.New("invalid MySQL DSN")
	}
	if cfg.DBName == "" {
		return nil, errors.New("explicit database name required")
	}
	cfg.ParseTime = true
	cfg.Loc, e = time.LoadLocation("Etc/GMT-8")
	if e != nil {
		return nil, e
	}
	cfg.MultiStatements = false
	cfg.InterpolateParams = false
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 30 * time.Second
	cfg.WriteTimeout = 30 * time.Second
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+08:00'"
	cfg.Params["sql_mode"] = "'STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'"
	db, e := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: false})
	if e != nil {
		return nil, errors.New("cannot connect to MySQL")
	}
	sqlDB, e := db.DB()
	if e != nil {
		return nil, e
	}
	sqlDB.SetMaxOpenConns(16)
	sqlDB.SetMaxIdleConns(8)
	sqlDB.SetConnMaxLifetime(10 * time.Minute)
	return db, nil
}
