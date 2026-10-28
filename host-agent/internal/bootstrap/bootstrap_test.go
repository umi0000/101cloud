package bootstrap

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"101cloud/hostagent/internal/config"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{StateDir: filepath.Join(t.TempDir(), "agent"), Endpoint: "https://127.0.0.1:8443", Validity: time.Hour}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func requireEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("initialization left files behind: %v", entries)
	}
}

func TestInitProducesMatchingPrivateState(t *testing.T) {
	for _, endpoint := range []string{"https://127.0.0.1:8443", "https://[2001:db8::2]:9443", "https://node.example.test:8443"} {
		t.Run(endpoint, func(t *testing.T) {
			opts := testOptions(t)
			opts.Endpoint = endpoint
			before := time.Now()
			paths, err := Init(opts)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(opts.StateDir)
			if err != nil || info.Mode().Perm() != 0700 {
				t.Fatalf("private directory: %v, %v", info, err)
			}
			entries, err := os.ReadDir(opts.StateDir)
			if err != nil || len(entries) != 5 {
				t.Fatalf("expected exactly five state files: %v, %v", entries, err)
			}
			for _, path := range []string{paths.Config, paths.Certificate, paths.Key, paths.Token, paths.Connection} {
				info, err := os.Stat(path)
				if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
					t.Fatalf("private file %s: %v, %v", path, info, err)
				}
			}
			cfg, err := config.Load(paths.Config)
			if err != nil {
				t.Fatal(err)
			}
			u, err := parseEndpoint(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Listen != u.Host || cfg.IncusSocket != DefaultIncusSocket || cfg.TLS.Cert != paths.Certificate || cfg.TLS.Key != paths.Key || cfg.TokenFile != paths.Token {
				t.Fatalf("agent configuration disagrees with paths or endpoint: %+v, %+v", cfg, paths)
			}
			var connection map[string]any
			if err := json.Unmarshal(readFile(t, paths.Connection), &connection); err != nil {
				t.Fatal(err)
			}
			if len(connection) != 4 || connection["version"] != float64(1) || connection["endpoint"] != endpoint {
				t.Fatalf("invalid connection contract: %v", connection)
			}
			token := string(readFile(t, paths.Token))
			decoded, err := base64.RawURLEncoding.DecodeString(token)
			if err != nil || len(decoded) != 32 || len(token) != 43 || connection["token"] != token {
				t.Fatalf("token is not matching canonical random 32-byte base64url: %v", err)
			}
			block, rest := pem.Decode(readFile(t, paths.Certificate))
			if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
				t.Fatal("invalid certificate PEM")
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			fingerprint := sha256.Sum256(cert.Raw)
			if connection["server_cert_sha256"] != hex.EncodeToString(fingerprint[:]) {
				t.Fatal("connection fingerprint does not match the leaf DER")
			}
			if cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage != x509.KeyUsageDigitalSignature || !reflect.DeepEqual(cert.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) {
				t.Fatalf("unexpected certificate privileges: %+v", cert)
			}
			if cert.NotBefore.After(before) || cert.NotAfter.Before(before.Add(opts.Validity-time.Second)) || cert.NotAfter.After(time.Now().Add(opts.Validity)) {
				t.Fatalf("unexpected certificate validity: %v to %v", cert.NotBefore, cert.NotAfter)
			}
			if !bytes.Equal(cert.RawSubject, cert.RawIssuer) || cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) != nil {
				t.Fatal("certificate is not self-signed")
			}
			roots := x509.NewCertPool()
			roots.AddCert(cert)
			if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: u.Hostname(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
				t.Fatalf("certificate does not authenticate advertised endpoint: %v", err)
			}
			if len(cert.DNSNames)+len(cert.IPAddresses) != 1 {
				t.Fatal("expected precisely one endpoint SAN")
			}
			keyBlock, _ := pem.Decode(readFile(t, paths.Key))
			if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
				t.Fatal("invalid private key PEM")
			}
			private, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			key, ok := private.(*ecdsa.PrivateKey)
			if !ok || key.Curve != elliptic.P256() || !key.PublicKey.Equal(cert.PublicKey) {
				t.Fatal("expected matching P-256 private key")
			}
		})
	}
}

func TestInitExplicitLocalOptions(t *testing.T) {
	opts := testOptions(t)
	opts.Listen = "0.0.0.0:9999"
	opts.IncusSocket = "/nonexistent/incus.socket"
	paths, err := Init(opts)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != opts.Listen || cfg.IncusSocket != opts.IncusSocket {
		t.Fatalf("explicit local options lost: %+v", cfg)
	}
}

func TestInitNeverOverwritesExistingState(t *testing.T) {
	opts := testOptions(t)
	paths, err := Init(opts)
	if err != nil {
		t.Fatal(err)
	}
	original := make(map[string][]byte)
	for _, path := range []string{paths.Config, paths.Certificate, paths.Key, paths.Token, paths.Connection} {
		original[path] = readFile(t, path)
	}
	if _, err := Init(opts); err == nil {
		t.Fatal("reinitialization succeeded")
	}
	for path, expected := range original {
		if !bytes.Equal(readFile(t, path), expected) {
			t.Fatalf("reinitialization changed %s", path)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(opts.StateDir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected staging residue: %v, %v", entries, err)
	}
}

func TestInitRejectsExistingDestinations(t *testing.T) {
	for _, kind := range []string{"empty-directory", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			opts := testOptions(t)
			var err error
			switch kind {
			case "empty-directory":
				err = os.Mkdir(opts.StateDir, 0700)
			case "file":
				err = os.WriteFile(opts.StateDir, []byte("keep"), 0600)
			case "symlink":
				err = os.Symlink(filepath.Join(filepath.Dir(opts.StateDir), "missing"), opts.StateDir)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Init(opts); err == nil {
				t.Fatal("existing destination accepted")
			}
			entries, err := os.ReadDir(filepath.Dir(opts.StateDir))
			if err != nil || len(entries) != 1 {
				t.Fatalf("unexpected residue: %v, %v", entries, err)
			}
		})
	}
}

func TestPublicationFailureRemovesAllStagedSecrets(t *testing.T) {
	opts := testOptions(t)
	sentinel := errors.New("injected publication failure")
	paths, err := initialize(opts, func(stage, destination string) error {
		for _, name := range []string{"agent.json", "server.crt", "server.key", "token", "connection.json"} {
			if len(readFile(t, filepath.Join(stage, name))) == 0 {
				t.Fatalf("missing staged %s", name)
			}
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) || paths != (Paths{}) {
		t.Fatalf("unexpected failure result: %+v, %v", paths, err)
	}
	requireEmpty(t, filepath.Dir(opts.StateDir))
}

func TestPublicationCannotReplaceConcurrentEmptyDirectory(t *testing.T) {
	opts := testOptions(t)
	_, err := initialize(opts, func(stage, destination string) error {
		if err := os.Mkdir(destination, 0750); err != nil {
			t.Fatal(err)
		}
		return publishDirectory(stage, destination)
	})
	if err == nil {
		t.Fatal("publication replaced a concurrently created directory")
	}
	requireEmpty(t, opts.StateDir)
	entries, err := os.ReadDir(filepath.Dir(opts.StateDir))
	if err != nil || len(entries) != 1 || entries[0].Name() != "agent" {
		t.Fatalf("staging secrets remain: %v, %v", entries, err)
	}
}

func TestInvalidEndpointsLeaveNoState(t *testing.T) {
	for _, endpoint := range []string{
		"", "http://node:8443", "https://node", "https://node:", "https://node:0", "https://node:65536", "https://node:https", "https://node:+8443",
		"https://:8443", "https://user@node:8443", "https://user:pass@node:8443", "https://node:8443/", "https://node:8443/api", "https://node:8443/%2f",
		"https://node:8443?token=secret", "https://node:8443?", "https://node:8443#secret", "https://node:8443#", "https://node:8443/?x=y",
		"https://[fe80::1%25en0]:8443", "https://*.example.test:8443", "https://bad_host:8443", "https://-bad:8443", "https://node..test:8443", " https://node:8443",
	} {
		t.Run(endpoint, func(t *testing.T) {
			opts := testOptions(t)
			opts.Endpoint = endpoint
			if _, err := Init(opts); err == nil {
				t.Fatal("invalid endpoint accepted")
			}
			requireEmpty(t, filepath.Dir(opts.StateDir))
		})
	}
}

func TestInvalidLocalOptionsLeaveNoState(t *testing.T) {
	for _, change := range []func(*Options){
		func(o *Options) { o.Validity = 0 },
		func(o *Options) { o.Validity = -time.Hour },
		func(o *Options) { o.Listen = "localhost" },
		func(o *Options) { o.Listen = "0.0.0.0:0" },
		func(o *Options) { o.Listen = "localhost:https" },
		func(o *Options) { o.IncusSocket = "relative.socket" },
		func(o *Options) { o.StateDir = "" },
	} {
		opts := testOptions(t)
		parent := filepath.Dir(opts.StateDir)
		change(&opts)
		if _, err := Init(opts); err == nil {
			t.Fatalf("invalid options accepted: %+v", opts)
		}
		requireEmpty(t, parent)
	}
}

func TestSeparateNodesReceiveDistinctCredentials(t *testing.T) {
	first, err := Init(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Init(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{first.Token, second.Token}, {first.Key, second.Key}, {first.Certificate, second.Certificate}} {
		if strings.TrimSpace(string(readFile(t, pair[0]))) == strings.TrimSpace(string(readFile(t, pair[1]))) {
			t.Fatalf("independent installations reused credentials: %s, %s", pair[0], pair[1])
		}
	}
}
