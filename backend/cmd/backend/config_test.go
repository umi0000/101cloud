package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
	driver "github.com/go-sql-driver/mysql"
)

func TestCommandConfig(t *testing.T) {
	for _, command := range []string{"serve", "install", "migrate"} {
		got, path, err := commandConfig([]string{command, "-c", "/tmp/config with spaces"})
		if err != nil || got != command || path != "/tmp/config with spaces" {
			t.Fatalf("%q %q %v", got, path, err)
		}
	}
	for _, args := range [][]string{nil, {"unknown"}, {"serve", "-c"}, {"serve", "-c", ""}, {"serve", "extra"}, {"serve", "--port", "8080"}} {
		if _, _, err := commandConfig(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}
func TestParseConfigLiterals(t *testing.T) {
	values, err := parseConfigFile(strings.NewReader("# comment\r\n CLOUD_ORIGIN = 'https://cloud.example'\r\nCLOUD_MYSQL_DSN=\"user:p#=$word@tcp(localhost)/db\"\nCLOUD_EMPTY=\n"))
	if err != nil {
		t.Fatal(err)
	}
	if values["CLOUD_ORIGIN"] != "https://cloud.example" || values["CLOUD_MYSQL_DSN"] != "user:p#=$word@tcp(localhost)/db" || values["CLOUD_EMPTY"] != "" {
		t.Fatalf("incorrect literal parsing: %v", values)
	}
	for _, text := range []string{"SECRET=value", "CLOUD_X='secret", "CLOUD_X=secret\nCLOUD_X=again", "CLOUD_X=secret\x00", "export CLOUD_X=secret", "CLOUD_X=" + strings.Repeat("s", 65536)} {
		_, err := parseConfigFile(strings.NewReader(text))
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
}
func TestFileEnvironmentPrecedenceAndAtomicParsing(t *testing.T) {
	t.Setenv("CLOUD_CONFIG_TEST_EXISTING", "environment")
	t.Setenv("CLOUD_CONFIG_TEST_EMPTY", "")
	key := "CLOUD_CONFIG_TEST_NEW"
	old, present := os.LookupEnv(key)
	os.Unsetenv(key)
	t.Cleanup(func() {
		if present {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	})
	path := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(path, []byte("CLOUD_CONFIG_TEST_EXISTING=file\nCLOUD_CONFIG_TEST_EMPTY=file\n"+key+"='literal'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfigFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CLOUD_CONFIG_TEST_EXISTING") != "environment" || os.Getenv("CLOUD_CONFIG_TEST_EMPTY") != "" || os.Getenv(key) != "literal" {
		t.Fatal("file overrode environment or failed to load")
	}
	os.Unsetenv(key)
	if err := os.WriteFile(path, []byte(key+"=partial\ninvalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfigFile(path); err == nil {
		t.Fatal("accepted invalid file")
	}
	if _, exists := os.LookupEnv(key); exists {
		t.Fatal("invalid file partially applied")
	}
	if err := loadConfigFile(path + ".missing"); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestDatabaseConfig(t *testing.T) {
	const business = "app:business-secret@tcp(127.0.0.1:3306)/business"
	for _, tt := range []struct {
		name, command, app, admin, user, database string
		wantError                                 bool
	}{
		{"fallback", "migrate", business, "", "app", "business", false},
		{"admin without database", "migrate", business, "root:admin-secret@tcp(127.0.0.1:3306)/", "root", "business", false},
		{"admin matching database", "migrate", business, "root:admin-secret@tcp(127.0.0.1:3306)/business", "root", "business", false},
		{"wrong database", "migrate", business, "root:admin-secret@tcp(127.0.0.1:3306)/other", "", "", true},
		{"wrong endpoint", "migrate", business, "root:admin-secret@tcp(127.0.0.1:3307)/", "", "", true},
		{"hostname alias rejected", "migrate", business, "root:admin-secret@tcp(localhost:3306)/", "", "", true},
		{"wrong transport", "migrate", business, "root:admin-secret@unix(/tmp/mysql.sock)/", "", "", true},
		{"missing business database", "migrate", "app:business-secret@tcp(127.0.0.1:3306)/", "root:admin-secret@tcp(127.0.0.1:3306)/business", "", "", true},
		{"invalid business", "migrate", "business-secret", "root:admin-secret@tcp(127.0.0.1:3306)/", "", "", true},
		{"invalid admin", "migrate", business, "admin-secret", "", "", true},
		{"serve ignores admin", "serve", business, "root:admin-secret@tcp(127.0.0.1:3306)/", "app", "business", false},
		{"serve ignores invalid admin", "serve", business, "admin-secret", "app", "business", false},
		{"normalized default endpoint", "migrate", "app:business-secret@tcp()/business", "root:admin-secret@tcp(127.0.0.1:3306)/", "root", "business", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := platform.Config{DSN: tt.app, Listen: "127.0.0.1:8080"}
			got, err := databaseConfig(tt.command, cfg, tt.admin)
			if tt.wantError {
				if err == nil {
					t.Fatal("unsafe configuration accepted")
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatal("error leaked credentials")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := driver.ParseDSN(got.DSN)
			if err != nil || parsed.User != tt.user || parsed.DBName != tt.database {
				t.Fatal("incorrect migration credentials or target")
			}
			if cfg.DSN != tt.app || got.Listen != cfg.Listen {
				t.Fatal("changed business configuration")
			}
			if tt.command == "serve" || tt.admin == "" {
				if got.DSN != tt.app {
					t.Fatal("changed runtime or fallback DSN")
				}
			} else if parsed.Passwd != "admin-secret" {
				t.Fatal("administrative password not selected")
			}
		})
	}
}
