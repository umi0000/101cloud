package migrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"gorm.io/gorm"
)

//go:embed init.sql migrations/*.sql
var files embed.FS

// Statements splits SQL without treating semicolons in strings or comments as
// delimiters. Database/schema selection is controlled by the operator's DSN.
func Statements(src string) ([]string, error) {
	var out []string
	var b strings.Builder
	var quote byte
	line, block := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if line {
			if c == '\n' {
				line = false
				b.WriteByte(' ')
			}
			continue
		}
		if block {
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				block = false
				i++
				b.WriteByte(' ')
			}
			continue
		}
		if quote != 0 {
			b.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				b.WriteByte(src[i])
				continue
			}
			if c == quote {
				if i+1 < len(src) && src[i+1] == quote {
					i++
					b.WriteByte(src[i])
				} else {
					quote = 0
				}
			}
			continue
		}
		if c == '#' || (c == '-' && i+2 < len(src) && src[i+1] == '-' && unicode.IsSpace(rune(src[i+2]))) {
			line = true
			continue
		}
		if c == '/' && i+1 < len(src) && src[i+1] == '*' {
			block = true
			i++
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			b.WriteByte(c)
			continue
		}
		if c == ';' {
			if s := strings.TrimSpace(b.String()); s != "" {
				out = append(out, s)
			}
			b.Reset()
			continue
		}
		b.WriteByte(c)
	}
	if quote != 0 || block {
		return nil, errors.New("unterminated SQL literal or comment")
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		out = append(out, s)
	}
	return out, nil
}
func hasTable(ctx context.Context, db *gorm.DB, name string) (bool, error) {
	var count int64
	e := db.WithContext(ctx).Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?", name).Scan(&count).Error
	return count == 1, e
}
func names() ([]string, error) {
	entries, e := files.ReadDir("migrations")
	if e != nil {
		return nil, e
	}
	out := []string{"init.sql"}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			out = append(out, "migrations/"+entry.Name())
		}
	}
	sort.Strings(out[1:])
	return out, nil
}
func Ready(ctx context.Context, db *gorm.DB) error {
	ns, e := names()
	if e != nil {
		return e
	}
	ok, e := hasTable(ctx, db, "backend_schema_migrations")
	if e != nil {
		return e
	}
	if !ok {
		return errors.New("schema is not initialized; run migrate explicitly")
	}
	for _, name := range ns {
		var count int64
		b, _ := files.ReadFile(name)
		h := sha256.Sum256(b)
		if e = db.WithContext(ctx).Raw("SELECT COUNT(*) FROM backend_schema_migrations WHERE name=? AND checksum=?", name, hex.EncodeToString(h[:])).Scan(&count).Error; e != nil {
			return e
		}
		if count != 1 {
			return fmt.Errorf("pending or changed migration %s", name)
		}
	}
	return nil
}
func legacyGuard(ctx context.Context, db *gorm.DB) error {
	if exists, err := hasTable(ctx, db, "port_forwards"); err != nil {
		return err
	} else if exists {
		var count int64
		if err := db.Table("port_forwards").Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return errors.New("legacy NAT forwarding history requires an explicit operator archive before retirement; no forwarding records are deleted automatically")
		}
	}
	journal, e := hasTable(ctx, db, "backend_schema_migrations")
	if e != nil {
		return e
	}
	if journal {
		var count int64
		if e = db.Raw("SELECT COUNT(*) FROM backend_schema_migrations WHERE name='migrations/20261030_40_billing.sql'").Scan(&count).Error; e != nil {
			return e
		}
		if count > 0 {
			return nil
		}
	}
	for _, table := range []string{"pricing_versions", "gpu_price_versions", "credit_policy_versions", "idle_policy_versions", "credit_accounts"} {
		exists, e := hasTable(ctx, db, table)
		if e != nil {
			return e
		}
		if !exists {
			continue
		}
		var count int64
		if e = db.Table(table).Count(&count).Error; e != nil {
			return e
		}
		if count > 0 {
			return errors.New("legacy financial data requires an explicit approved policy-data migration before backend schema upgrade; no financial modes are inferred")
		}
	}
	return nil
}
func Up(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
		var locked int
		if e := conn.Raw("SELECT GET_LOCK(SHA2(CONCAT('101cloud.migrate.',DATABASE()),256),30)").Scan(&locked).Error; e != nil {
			return e
		}
		if locked != 1 {
			return errors.New("migration lock unavailable")
		}
		defer conn.Exec("SELECT RELEASE_LOCK(SHA2(CONCAT('101cloud.migrate.',DATABASE()),256))")
		if e := legacyGuard(ctx, conn); e != nil {
			return e
		}
		if e := conn.Exec("CREATE TABLE IF NOT EXISTS backend_schema_migrations(name VARCHAR(255) PRIMARY KEY,checksum CHAR(64) NOT NULL,applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)) ENGINE=InnoDB").Error; e != nil {
			return e
		}
		ns, e := names()
		if e != nil {
			return e
		}
		for _, name := range ns {
			b, e := files.ReadFile(name)
			if e != nil {
				return e
			}
			h := sha256.Sum256(b)
			sum := hex.EncodeToString(h[:])
			var existing string
			if e = conn.Raw("SELECT checksum FROM backend_schema_migrations WHERE name=?", name).Scan(&existing).Error; e != nil {
				return e
			}
			if existing != "" {
				if existing != sum {
					return fmt.Errorf("applied migration checksum changed: %s", name)
				}
				continue
			}
			skip := false
			if name == "init.sql" {
				skip, e = hasTable(ctx, conn, "users")
			} else if name == "migrations/20261029_instance_metrics.sql" {
				skip, e = hasTable(ctx, conn, "instance_metric_samples")
				if skip {
					for _, t := range []string{"instance_gpu_metric_samples", "instance_metric_rollups", "instance_gpu_metric_rollups", "node_metric_cursors"} {
						ok, err := hasTable(ctx, conn, t)
						if err != nil {
							return err
						}
						if !ok {
							return errors.New("partial metrics schema; repair explicitly before migration")
						}
					}
				}
			}
			if e != nil {
				return e
			}
			if !skip {
				statements, e := Statements(string(b))
				if e != nil {
					return e
				}
				for i, s := range statements {
					u := strings.ToUpper(strings.TrimSpace(s))
					if strings.HasPrefix(u, "CREATE DATABASE") || strings.HasPrefix(u, "USE ") {
						continue
					}
					if e = conn.Exec(s).Error; e != nil {
						return fmt.Errorf("%s statement %d: %w (DDL may already be committed; repair before retry)", name, i+1, e)
					}
				}
			}
			if e = conn.Exec("INSERT INTO backend_schema_migrations(name,checksum) VALUES(?,?)", name, sum).Error; e != nil {
				return e
			}
		}
		return nil
	})
}
