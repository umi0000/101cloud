package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRejectsAmbiguousConfiguration(t *testing.T) {
	for _, data := range []string{
		`{"listen":"127.0.0.1:8443","incus_sockte":"/tmp/socket"}`,
		`{} {}`,
		`{"listen":"not-an-address"}`,
		`{"listen":"127.0.0.1:8443","incus_socket":"relative.sock","token_file":"token","tls":{"cert":"x","key":"y"}}`,
		`{"listen":"127.0.0.1:8443","token_file":"token","tls":{"cert":"x","key":"y","client_ca":"old.crt"}}`,
	} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("accepted invalid or ambiguous configuration")
			}
		})
	}
}

func TestCertificatePathsResolveAgainstConfigurationDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	data := `{"listen":"127.0.0.1:8443","token_file":"token","tls":{"cert":"pki/server.crt","key":"pki/server.key"}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.TLS.Cert != filepath.Join(dir, "pki/server.crt") || c.TLS.Key != filepath.Join(dir, "pki/server.key") || c.TokenFile != filepath.Join(dir, "token") {
		t.Fatalf("paths do not follow config directory: %+v", c.TLS)
	}
}
