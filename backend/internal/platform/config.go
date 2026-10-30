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
	c := Config{Listen: env("CLOUD_LISTEN", "127.0.0.1:8080"), Origin: os.Getenv("CLOUD_ORIGIN"), DSN: os.Getenv("CLOUD_MYSQL_DSN"), SessionTTL: 12 * time.Hour, ConsoleMaxConcurrent: 4}
	if e := validOrigin(c.Origin); e != nil {
		return c, e
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
	if text := os.Getenv("CLOUD_SESSION_TTL"); text != "" {
		v, e := time.ParseDuration(text)
		if e != nil || v <= 0 {
			return c, errors.New("invalid positive duration CLOUD_SESSION_TTL")
		}
		c.SessionTTL = v
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
	if c.TrustedProxies, e = trustedProxies(os.Getenv("CLOUD_TRUSTED_PROXIES")); e != nil {
		return c, e
	}
	return c, nil
}

// HTTPS reports whether browsers reach the panel over TLS; cookie names and the
// Secure attribute follow it because browsers drop Secure/__Host- cookies on http.
func (c Config) HTTPS() bool { return strings.HasPrefix(c.Origin, "https://") }

// Origin must equal the browser's serialized Origin header byte for byte:
// lowercase scheme://host[:port], no default port, no userinfo/path/query/fragment.
func validOrigin(origin string) error {
	invalid := errors.New("CLOUD_ORIGIN must be a canonical origin http(s)://host[:port] (lowercase, no default port, no path)")
	u, e := url.Parse(origin)
	if e != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.Scheme+"://"+u.Host != origin || origin != strings.ToLower(origin) || strings.HasSuffix(u.Host, ":") {
		return invalid
	}
	if port := u.Port(); (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		return invalid
	}
	return nil
}

// Empty means no proxy is trusted and X-Forwarded-For is ignored.
func trustedProxies(list string) ([]netip.Prefix, error) {
	if strings.TrimSpace(list) == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, s := range strings.Split(list, ",") {
		s = strings.TrimSpace(s)
		var p netip.Prefix
		var e error
		if strings.Contains(s, "/") {
			p, e = netip.ParsePrefix(s)
		} else {
			var addr netip.Addr
			if addr, e = netip.ParseAddr(s); e == nil {
				if addr.Zone() != "" {
					e = errors.New("zoned address")
				}
				p = netip.PrefixFrom(addr, addr.BitLen())
			}
		}
		// IPv4-mapped IPv6 and zoned addresses would match differently in net.IPNet.
		if e != nil || !p.IsValid() || p.Addr().Zone() != "" || p.Addr().Is4In6() {
			return nil, errors.New("invalid CLOUD_TRUSTED_PROXIES entry " + strconv.Quote(s))
		}
		out = append(out, p.Masked())
	}
	return out, nil
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
