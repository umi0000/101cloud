package gateway

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type serverIdentity struct {
	certFile string
	keyFile  string
	roots    *x509.CertPool
}

func newServerIdentity(t *testing.T, signer crypto.Signer, mutate func(*x509.Certificate)) serverIdentity {
	t.Helper()
	if signer == nil {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signer = key
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	if mutate != nil {
		mutate(template)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, signer.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(signer)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	identity := serverIdentity{certFile: filepath.Join(dir, "server.crt"), keyFile: filepath.Join(dir, "server.key"), roots: x509.NewCertPool()}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(identity.certFile, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity.keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	identity.roots.AppendCertsFromPEM(certPEM)
	return identity
}

func authenticatedTestServer(t *testing.T, handler http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	identity := newServerIdentity(t, nil, nil)
	configuration, err := TLSConfig(identity.certFile, identity.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = configuration
	server.StartTLS()
	t.Cleanup(server.Close)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: identity.roots, MinVersion: tls.VersionTLS13}}
	t.Cleanup(transport.CloseIdleConnections)
	return server, &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func TestTLSSelfSignedServerWithoutClientCertificate(t *testing.T) {
	server, client := authenticatedTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 || len(r.TLS.PeerCertificates) != 0 {
			t.Error("expected TLS 1.3 without client certificates")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", response.StatusCode)
	}
	legacy := client.Transport.(*http.Transport).Clone()
	legacy.TLSClientConfig.MinVersion = tls.VersionTLS12
	legacy.TLSClientConfig.MaxVersion = tls.VersionTLS12
	defer legacy.CloseIdleConnections()
	if response, err := (&http.Client{Transport: legacy}).Get(server.URL); err == nil {
		response.Body.Close()
		t.Fatal("TLS 1.2 accepted")
	}
}

func TestTLSRejectsUnsafeKeyFiles(t *testing.T) {
	for _, mode := range []os.FileMode{0400, 0640, 0644, 0666} {
		identity := newServerIdentity(t, nil, nil)
		if err := os.Chmod(identity.keyFile, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := TLSConfig(identity.certFile, identity.keyFile); err == nil {
			t.Errorf("accepted private-key mode %04o", mode)
		}
	}
	identity := newServerIdentity(t, nil, nil)
	if _, err := TLSConfig(identity.certFile, t.TempDir()); err == nil {
		t.Fatal("accepted directory as private key")
	}
}

func TestTLSRejectsUnusableServerIdentity(t *testing.T) {
	for _, kind := range []string{"expired", "future", "client-only", "any-usage", "no-san", "ca", "no-signing"} {
		t.Run(kind, func(t *testing.T) {
			identity := newServerIdentity(t, nil, func(cert *x509.Certificate) {
				switch kind {
				case "expired":
					cert.NotBefore, cert.NotAfter = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
				case "future":
					cert.NotBefore, cert.NotAfter = time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)
				case "client-only":
					cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
				case "any-usage":
					cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny}
				case "no-san":
					cert.IPAddresses = nil
				case "ca":
					cert.IsCA, cert.BasicConstraintsValid = true, true
				case "no-signing":
					cert.KeyUsage = x509.KeyUsageKeyEncipherment
				}
			})
			if _, err := TLSConfig(identity.certFile, identity.keyFile); err == nil {
				t.Fatal("accepted unusable identity")
			}
		})
	}
}

func TestTLSRejectsWeakCryptography(t *testing.T) {
	weakKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	identity := newServerIdentity(t, weakKey, nil)
	if _, err := TLSConfig(identity.certFile, identity.keyFile); err == nil {
		t.Fatal("accepted 1024-bit RSA key")
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, algorithm := range []x509.SignatureAlgorithm{x509.MD5WithRSA, x509.SHA1WithRSA, x509.ECDSAWithSHA1, x509.UnknownSignatureAlgorithm} {
		if err := strongCertificate(&x509.Certificate{SignatureAlgorithm: algorithm, PublicKey: public}); err == nil {
			t.Errorf("accepted weak signature algorithm %v", algorithm)
		}
	}
}
