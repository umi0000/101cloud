package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
