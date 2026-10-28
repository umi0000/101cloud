// Package bootstrap creates a complete, private agent state directory without contacting a node.
package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"101cloud/hostagent/internal/config"
)

const DefaultValidity = 365 * 24 * time.Hour
const DefaultIncusSocket = "/var/lib/incus/unix.socket"

// Options describes a new installation. StateDir must not already exist.
// Empty Listen and IncusSocket use the endpoint address and default local socket.
type Options struct {
	StateDir    string
	Endpoint    string
	Listen      string
	IncusSocket string
	Validity    time.Duration
}

// Paths contains only published file paths, never secret material.
type Paths struct {
	Config      string
	Certificate string
	Key         string
	Token       string
	Connection  string
}

// Init generates all state privately, then atomically publishes it without replacing
// any existing destination. A failure removes the staging directory and its secrets.
func Init(opts Options) (Paths, error) {
	return initialize(opts, publishDirectory)
}

func initialize(opts Options, publish func(string, string) error) (Paths, error) {
	var paths Paths
	endpoint, err := parseEndpoint(opts.Endpoint)
	if err != nil {
		return paths, err
	}
	if opts.StateDir == "" {
		return paths, errors.New("state-dir is required")
	}
	stateDir, err := filepath.Abs(opts.StateDir)
	if err != nil {
		return paths, fmt.Errorf("state-dir: %w", err)
	}
	if _, err := os.Lstat(stateDir); err == nil {
		return paths, fmt.Errorf("state-dir already exists: %s", stateDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return paths, fmt.Errorf("inspect state-dir: %w", err)
	}
	if opts.Listen == "" {
		opts.Listen = endpoint.Host
	}
	if err := validateAddress(opts.Listen); err != nil {
		return paths, fmt.Errorf("listen: %w", err)
	}
	if opts.IncusSocket == "" {
		opts.IncusSocket = DefaultIncusSocket
	}
	if !filepath.IsAbs(opts.IncusSocket) {
		return paths, errors.New("incus-socket must be an absolute path")
	}
	if opts.Validity <= 0 {
		return paths, errors.New("validity must be positive")
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return paths, fmt.Errorf("generate server key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return paths, fmt.Errorf("generate certificate serial: %w", err)
	}
	serial.Add(serial, big.NewInt(1))
	now := time.Now().UTC()
	cert := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: endpoint.Hostname()},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(opts.Validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	if ip := net.ParseIP(endpoint.Hostname()); ip != nil {
		cert.IPAddresses = []net.IP{ip}
	} else {
		cert.DNSNames = []string{endpoint.Hostname()}
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		return paths, fmt.Errorf("create server certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return paths, fmt.Errorf("encode server key: %w", err)
	}
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return paths, fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes[:])
	fingerprint := sha256.Sum256(der)
	connection := struct {
		Version          int    `json:"version"`
		Endpoint         string `json:"endpoint"`
		Token            string `json:"token"`
		ServerCertSHA256 string `json:"server_cert_sha256"`
	}{1, endpoint.String(), token, hex.EncodeToString(fingerprint[:])}
	cfg := config.Config{
		Listen:      opts.Listen,
		IncusSocket: opts.IncusSocket,
		TokenFile:   "token",
		TLS:         config.TLS{Cert: "server.crt", Key: "server.key"},
	}
	configJSON, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return paths, fmt.Errorf("encode agent configuration: %w", err)
	}
	connectionJSON, err := json.MarshalIndent(connection, "", "  ")
	if err != nil {
		return paths, fmt.Errorf("encode connection configuration: %w", err)
	}
	stage, err := os.MkdirTemp(filepath.Dir(stateDir), "."+filepath.Base(stateDir)+".init-")
	if err != nil {
		return paths, fmt.Errorf("create private staging directory: %w", err)
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0700); err != nil {
		return paths, fmt.Errorf("secure staging directory: %w", err)
	}
	files := []struct {
		name string
		data []byte
	}{
		{"agent.json", append(configJSON, '\n')},
		{"server.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})},
		{"server.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})},
		{"token", []byte(token)},
		{"connection.json", append(connectionJSON, '\n')},
	}
	for _, file := range files {
		if err := writePrivateFile(filepath.Join(stage, file.name), file.data); err != nil {
			return paths, fmt.Errorf("write %s: %w", file.name, err)
		}
	}
	if err := publish(stage, stateDir); err != nil {
		return paths, fmt.Errorf("publish state-dir without replacement: %w", err)
	}
	return Paths{
		Config:      filepath.Join(stateDir, "agent.json"),
		Certificate: filepath.Join(stateDir, "server.crt"),
		Key:         filepath.Join(stateDir, "server.key"),
		Token:       filepath.Join(stateDir, "token"),
		Connection:  filepath.Join(stateDir, "connection.json"),
	}, nil
}

func writePrivateFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

func parseEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("endpoint must be an HTTPS origin with an explicit port")
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawPath != "" || strings.ContainsAny(raw, "?#") {
		return nil, errors.New("endpoint must be https://host:port without userinfo, path, query or fragment")
	}
	if err := validateAddress(u.Host); err != nil {
		return nil, fmt.Errorf("endpoint: %w", err)
	}
	if !validHostname(u.Hostname()) {
		return nil, errors.New("endpoint host must be an IP address or DNS hostname")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsUnspecified() {
		return nil, errors.New("endpoint must use a reachable IP, not an unspecified listen address")
	}
	return u, nil
}

func validateAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("expected host:port with an explicit numeric port")
	}
	if host != "" && !validHostname(host) {
		return errors.New("host must be an IP address or DNS hostname")
	}
	if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return errors.New("port must be a number between 1 and 65535")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("port must be a number between 1 and 65535")
	}
	return nil
}

func validHostname(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	host = strings.TrimSuffix(host, ".")
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}
