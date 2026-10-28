// Package client supplies a node-scoped HTTPS client authenticated with a
// Bearer token and the server leaf-certificate fingerprint imported out of band.
package client

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ConnectionConfig is the secret connection document exported by agent init.
// Import it over a trusted channel: its fingerprint is the trust anchor, not a
// hint for trust-on-first-use. Keep this document private like the node token.
type ConnectionConfig struct {
	Version          int    `json:"version"`
	Endpoint         string `json:"endpoint"`
	Token            string `json:"token"`
	ServerCertSHA256 string `json:"server_cert_sha256"`
}

// LoadConnection reads an owner-only JSON document, rejecting unknown,
// duplicate, missing, null and trailing fields/documents.
func LoadConnection(path string) (ConnectionConfig, error) {
	var c ConnectionConfig
	file, err := os.Open(path)
	if err != nil {
		return c, errors.New("cannot open connection document")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return c, errors.New("connection document must be a regular file with mode 0600")
	}
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(data) > 65536 {
		return c, errors.New("cannot read connection document or document exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return c, errors.New("connection document must be a JSON object")
	}
	seen := make(map[string]bool, 4)
	for decoder.More() {
		field, err := decoder.Token()
		if err != nil {
			return ConnectionConfig{}, errors.New("invalid connection document")
		}
		name, ok := field.(string)
		if !ok || seen[name] {
			return ConnectionConfig{}, errors.New("duplicate or invalid connection field")
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ConnectionConfig{}, errors.New("invalid connection field value")
		}
		switch name {
		case "version":
			err = json.Unmarshal(value, &c.Version)
		case "endpoint":
			err = json.Unmarshal(value, &c.Endpoint)
		case "token":
			err = json.Unmarshal(value, &c.Token)
		case "server_cert_sha256":
			err = json.Unmarshal(value, &c.ServerCertSHA256)
		default:
			return ConnectionConfig{}, errors.New("unknown connection field")
		}
		if err != nil {
			return ConnectionConfig{}, errors.New("invalid connection field type")
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return ConnectionConfig{}, errors.New("invalid connection document")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ConnectionConfig{}, errors.New("trailing connection document data")
	}
	if len(seen) != 4 {
		return ConnectionConfig{}, errors.New("connection document requires all four fields")
	}
	if _, _, err := validateConnection(c); err != nil {
		return ConnectionConfig{}, err
	}
	return c, nil
}

func validateConnection(c ConnectionConfig) (*url.URL, []byte, error) {
	if c.Version != 1 {
		return nil, nil, errors.New("unsupported connection document version")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(c.Token)
	if err != nil || len(c.Token) != 43 || len(decoded) != 32 {
		return nil, nil, errors.New("connection token must encode 32 bytes as unpadded base64url")
	}
	pin, err := hex.DecodeString(c.ServerCertSHA256)
	if err != nil || len(pin) != sha256.Size || strings.ToLower(c.ServerCertSHA256) != c.ServerCertSHA256 {
		return nil, nil, errors.New("server certificate fingerprint must be 64 lowercase hexadecimal characters")
	}
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || !validHTTPSURL(endpoint) || (endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.ForceQuery || strings.Contains(c.Endpoint, "#") {
		return nil, nil, errors.New("connection endpoint must be an HTTPS origin without userinfo, path, query or fragment")
	}
	return endpoint, pin, nil
}

func validHTTPSURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.Opaque != "" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" || strings.ContainsAny(u.Host, "\r\n\t %\\") || strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}

func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	} else if n, err := strconv.Atoi(port); err == nil {
		port = strconv.Itoa(n)
	}
	return strings.ToLower(u.Hostname()) + ":" + port
}

// NewHTTPClient creates a TLS 1.3 client restricted to the imported node origin.
// Redirects are returned without following them. No client-wide timeout is set,
// allowing operation streams; callers should use contexts for deadlines.
func NewHTTPClient(c ConnectionConfig) (*http.Client, error) {
	endpoint, pin, err := validateConnection(c)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		// Deliberately do not use ProxyFromEnvironment: node credentials and
		// management traffic must not be routed through ambient HTTP proxies.
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			ServerName: endpoint.Hostname(),
			// The imported leaf fingerprint replaces public/system CA trust.
			// VerifyConnection below restores the complete fixed trust boundary:
			// exact leaf pin, TLS 1.3, validity, hostname, constraints and explicit
			// serverAuth signing usage. No certificate is ever learned on connect.
			InsecureSkipVerify: true,
			VerifyConnection: func(state tls.ConnectionState) error {
				if state.Version < tls.VersionTLS13 || len(state.PeerCertificates) == 0 {
					return errors.New("node requires TLS 1.3 and a server certificate")
				}
				leaf := state.PeerCertificates[0]
				digest := sha256.Sum256(leaf.Raw)
				if subtle.ConstantTimeCompare(digest[:], pin) != 1 {
					return errors.New("node server certificate fingerprint mismatch")
				}
				serverAuth := false
				for _, usage := range leaf.ExtKeyUsage {
					serverAuth = serverAuth || usage == x509.ExtKeyUsageServerAuth
				}
				if leaf.IsCA || !serverAuth || leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
					return errors.New("node certificate must be a non-CA serverAuth signing identity")
				}
				roots := x509.NewCertPool()
				roots.AddCert(leaf)
				_, err := leaf.Verify(x509.VerifyOptions{
					Roots: roots, DNSName: endpoint.Hostname(),
					KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
				})
				if err != nil {
					return fmt.Errorf("verify pinned node certificate: %w", err)
				}
				return nil
			},
		},
	}
	return &http.Client{
		Transport:     &httpsTransport{base: transport, origin: origin(endpoint), token: c.Token},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

type httpsTransport struct {
	base   *http.Transport
	origin string
	token  string
}

func (t *httpsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || !validHTTPSURL(req.URL) || origin(req.URL) != t.origin {
		return nil, errors.New("host agent client only permits its configured HTTPS origin without userinfo")
	}
	if req.Host != "" {
		hostURL, err := url.Parse("https://" + req.Host)
		if err != nil || !validHTTPSURL(hostURL) || hostURL.Path != "" || hostURL.RawQuery != "" || origin(hostURL) != t.origin {
			return nil, errors.New("host agent request Host must match its configured origin")
		}
	}
	// Clone so adding the secret does not mutate the caller's request or allow
	// a supplied duplicate/mixed-case Authorization field to reach the wire.
	out := req.Clone(req.Context())
	if out.Header == nil {
		out.Header = make(http.Header)
	}
	for name := range out.Header {
		if strings.EqualFold(name, "Authorization") {
			delete(out.Header, name)
		}
	}
	out.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(out)
}

func (t *httpsTransport) CloseIdleConnections() {
	t.base.CloseIdleConnections()
}
