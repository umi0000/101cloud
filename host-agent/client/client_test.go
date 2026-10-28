package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/hostagent/internal/gateway"
)

var nodeToken = base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func testCertificate(t *testing.T, mutate func(*x509.Certificate)) tls.Certificate {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	if mutate != nil {
		mutate(template)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
}

func testServer(t *testing.T, identity tls.Certificate, handler http.Handler, version uint16) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{identity}, MinVersion: tls.VersionTLS12, MaxVersion: version}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func connectionFor(endpoint string, certificate tls.Certificate) ConnectionConfig {
	pin := sha256.Sum256(certificate.Certificate[0])
	return ConnectionConfig{Version: 1, Endpoint: endpoint, Token: nodeToken, ServerCertSHA256: hex.EncodeToString(pin[:])}
}

func validConnection() ConnectionConfig {
	return ConnectionConfig{Version: 1, Endpoint: "https://node.example.test:8443", Token: nodeToken, ServerCertSHA256: strings.Repeat("a", 64)}
}

func TestPinnedBearerHTTPSStreamingAndRedirects(t *testing.T) {
	identity := testCertificate(t, nil)
	var followed atomic.Bool
	var crossOrigin atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		crossOrigin.Store(true)
	}))
	defer other.Close()
	server := testServer(t, identity, gateway.TokenAuth(nodeToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 || len(r.TLS.PeerCertificates) != 0 {
			t.Error("expected TLS 1.3 without a client identity")
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("token leaked through authentication boundary")
		}
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/followed", http.StatusFound)
		case "/external":
			http.Redirect(w, r, other.URL, http.StatusFound)
		case "/followed":
			followed.Store(true)
		default:
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			io.WriteString(w, "stream")
		}
	})), tls.VersionTLS13)
	client, err := NewHTTPClient(connectionFor(server.URL, identity))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if client.Timeout != 0 {
		t.Fatal("client-wide timeout prevents operation streaming")
	}
	for _, path := range []string{"/", "/redirect", "/external"} {
		response, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if path == "/" && (response.StatusCode != http.StatusOK || string(body) != "stream") {
			t.Fatalf("unexpected stream: %d %q", response.StatusCode, body)
		}
		if path != "/" && response.StatusCode != http.StatusFound {
			t.Fatalf("redirect followed: %d", response.StatusCode)
		}
	}
	if followed.Load() || crossOrigin.Load() {
		t.Fatal("redirect target was contacted")
	}
}

func TestClientDoesNotUseEnvironmentProxies(t *testing.T) {
	var proxied atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Store(true)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	// Keep a non-loopback URL (which ambient proxy helpers would route), but
	// resolve it locally. The certificate must still verify that URL hostname.
	identity := testCertificate(t, func(cert *x509.Certificate) { cert.DNSNames = []string{"node.example.test"} })
	server := testServer(t, identity, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+nodeToken {
			t.Errorf("Authorization = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}), tls.VersionTLS13)
	c := connectionFor("https://node.example.test:8443", identity)
	client, err := NewHTTPClient(c)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	transport := client.Transport.(*httpsTransport).base
	if transport.Proxy != nil {
		t.Fatal("transport consults ambient proxies")
	}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	request, _ := http.NewRequest(http.MethodGet, c.Endpoint, nil)
	request.Header["authorization"] = []string{"caller-secret"}
	request.Header["Authorization"] = []string{"another-secret", "duplicate"}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || proxied.Load() {
		t.Fatal("request used an external proxy")
	}
	if request.Header.Get("Authorization") != "another-secret" || request.Header["authorization"][0] != "caller-secret" {
		t.Fatal("client mutated caller's request credentials")
	}
}

func TestClientRejectsOtherOriginsAndUserinfoBeforeDial(t *testing.T) {
	client, err := NewHTTPClient(validConnection())
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	transport := client.Transport.(*httpsTransport).base
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Error("unsafe request reached the dialer")
		return nil, context.Canceled
	}
	for _, endpoint := range []string{
		"http://node.example.test:8443/", "https://other.example.test:8443/", "https://node.example.test:9443/",
		"https://user:secret@node.example.test:8443/", "https://node.example.test:8443.evil.test/", "https://node.example.test:8443/#fragment",
	} {
		if response, err := client.Get(endpoint); err == nil {
			response.Body.Close()
			t.Errorf("accepted unsafe request URL %q", endpoint)
		}
	}
	request, _ := http.NewRequest(http.MethodGet, validConnection().Endpoint, nil)
	request.Host = "other.example.test:8443"
	if response, err := client.Do(request); err == nil {
		response.Body.Close()
		t.Fatal("accepted a different Host header")
	}
}

func TestClientRejectsUntrustedOrUnusableServer(t *testing.T) {
	for _, kind := range []string{"wrong-pin", "replacement", "expired", "future", "hostname", "client-only", "any-usage", "no-signing", "ca", "TLS1.2"} {
		t.Run(kind, func(t *testing.T) {
			identity := testCertificate(t, func(cert *x509.Certificate) {
				switch kind {
				case "expired":
					cert.NotBefore, cert.NotAfter = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
				case "future":
					cert.NotBefore, cert.NotAfter = time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)
				case "hostname":
					cert.IPAddresses = nil
					cert.DNSNames = []string{"wrong.example.test"}
				case "client-only":
					cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
				case "any-usage":
					cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny}
				case "no-signing":
					cert.KeyUsage = x509.KeyUsageKeyEncipherment
				case "ca":
					cert.IsCA, cert.BasicConstraintsValid = true, true
				}
			})
			version := uint16(tls.VersionTLS13)
			if kind == "TLS1.2" {
				version = tls.VersionTLS12
			}
			server := testServer(t, identity, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted server received credentials") }), version)
			c := connectionFor(server.URL, identity)
			switch kind {
			case "wrong-pin":
				c.ServerCertSHA256 = strings.Repeat("0", 64)
			case "replacement":
				c = connectionFor(server.URL, testCertificate(t, nil))
			}
			client, err := NewHTTPClient(c)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			if response, err := client.Get(server.URL); err == nil {
				response.Body.Close()
				t.Fatal("untrusted server was accepted")
			}
		})
	}
}

func TestLoadConnectionStrictJSONAndPermissions(t *testing.T) {
	c := validConnection()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "connection.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConnection(path)
	if err != nil || loaded != c {
		t.Fatalf("loaded = %+v, err = %v", loaded, err)
	}
	for _, mode := range []os.FileMode{0400, 0640, 0644, 0666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConnection(path); err == nil {
			t.Errorf("accepted connection permissions %04o", mode)
		}
	}
	if _, err := LoadConnection(t.TempDir()); err == nil {
		t.Fatal("accepted directory as connection document")
	}
	for _, malformed := range []string{
		"", "null", "[]", "{}", string(data) + "{}", string(data) + " garbage",
		strings.Replace(string(data), `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(string(data), `"version":1`, `"Version":1`, 1),
		strings.Replace(string(data), `"version":1`, `"version":null`, 1),
		strings.Replace(string(data), `"version":1`, `"version":"1"`, 1),
		strings.Replace(string(data), `"version":1`, `"version":1,"unknown":true`, 1),
		strings.Replace(string(data), `"endpoint":"`+c.Endpoint+`",`, "", 1),
		strings.Replace(string(data), `"token":"`+c.Token+`"`, `"token":null`, 1),
		string(data[:len(data)-1]), strings.Repeat(" ", 65537),
	} {
		path := filepath.Join(t.TempDir(), "connection.json")
		if err := os.WriteFile(path, []byte(malformed), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConnection(path); err == nil {
			t.Errorf("accepted malformed document %.120q", malformed)
		}
	}
}

func TestRejectInvalidConnectionValues(t *testing.T) {
	for _, kind := range []string{"version", "http", "userinfo", "path", "query", "empty-query", "fragment", "opaque", "missing-host", "port", "zero-port", "token-length", "token-padding", "token-alphabet", "token-canonical", "pin-length", "pin-uppercase", "pin-alphabet"} {
		t.Run(kind, func(t *testing.T) {
			c := validConnection()
			switch kind {
			case "version":
				c.Version = 2
			case "http":
				c.Endpoint = "http://node.example.test:8443"
			case "userinfo":
				c.Endpoint = "https://secret@node.example.test:8443"
			case "path":
				c.Endpoint += "/1.0"
			case "query":
				c.Endpoint += "?token=secret"
			case "empty-query":
				c.Endpoint += "?"
			case "fragment":
				c.Endpoint += "#fragment"
			case "opaque":
				c.Endpoint = "https:node.example.test"
			case "missing-host":
				c.Endpoint = "https:///"
			case "port":
				c.Endpoint = "https://node.example.test:65536"
			case "zero-port":
				c.Endpoint = "https://node.example.test:0"
			case "token-length":
				c.Token = "short"
			case "token-padding":
				c.Token += "="
			case "token-alphabet":
				c.Token = strings.Repeat("/", 43)
			case "token-canonical":
				c.Token = strings.Repeat("_", 43)
			case "pin-length":
				c.ServerCertSHA256 = "abcd"
			case "pin-uppercase":
				c.ServerCertSHA256 = strings.Repeat("A", 64)
			case "pin-alphabet":
				c.ServerCertSHA256 = strings.Repeat("g", 64)
			}
			if client, err := NewHTTPClient(c); err == nil {
				client.CloseIdleConnections()
				t.Fatal("accepted invalid connection values")
			}
			data, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "connection.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConnection(path); err == nil {
				t.Fatal("loaded invalid connection values")
			}
		})
	}
}

func TestOriginNormalization(t *testing.T) {
	for _, endpoint := range []string{"https://node.example.test", "https://NODE.example.test:443/", "https://node.example.test:0443/"} {
		u, err := url.Parse(endpoint)
		if err != nil || origin(u) != "node.example.test:443" {
			t.Fatalf("origin(%q) = %q, err = %v", endpoint, origin(u), err)
		}
	}
}
