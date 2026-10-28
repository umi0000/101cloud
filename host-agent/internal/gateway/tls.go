package gateway

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// TLSConfig loads the node's server identity. HTTPS authenticates the server;
// TokenAuth authenticates requests, so client certificates are not requested.
func TLSConfig(certFile, keyFile string) (*tls.Config, error) {
	keyPEM, err := readPrivateKey(keyFile)
	if err != nil {
		return nil, err
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, errors.New("cannot read server certificate")
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, errors.New("invalid server certificate or private key")
	}
	for i, der := range certificate.Certificate {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, errors.New("invalid server certificate chain")
		}
		if err := strongCertificate(cert); err != nil {
			return nil, fmt.Errorf("server certificate: %w", err)
		}
		if now := time.Now(); now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
			return nil, errors.New("server certificate is expired or not yet valid")
		}
		if i == 0 && (cert.IsCA || !hasServerAuth(cert) || cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 || len(cert.DNSNames)+len(cert.IPAddresses) == 0) {
			return nil, errors.New("server identity must be a non-CA serverAuth signing certificate with DNS or IP SAN")
		}
		if i == 0 {
			certificate.Leaf = cert
		}
	}

	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		// net/http only supports HTTP/1.1 upgrades for Incus WebSockets. Do
		// not advertise HTTP/2 without configuring a separate HTTP/2 server.
		NextProtos: []string{"http/1.1"},
	}, nil
}

func readPrivateKey(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open server private key")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("server private key must be a regular file with mode 0600")
	}
	// Read the same open file that was inspected, not a replaceable path.
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, errors.New("cannot read server private key")
	}
	return data, nil
}

func strongCertificate(cert *x509.Certificate) error {
	switch cert.SignatureAlgorithm {
	case x509.SHA256WithRSA, x509.SHA384WithRSA, x509.SHA512WithRSA,
		x509.SHA256WithRSAPSS, x509.SHA384WithRSAPSS, x509.SHA512WithRSAPSS,
		x509.ECDSAWithSHA256, x509.ECDSAWithSHA384, x509.ECDSAWithSHA512,
		x509.PureEd25519:
	default:
		return errors.New("unsupported or weak certificate signature algorithm")
	}
	switch key := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		if key.N.BitLen() >= 2048 {
			return nil
		}
	case *ecdsa.PublicKey:
		if key.Curve != nil && key.Curve.Params().BitSize >= 256 {
			return nil
		}
	case ed25519.PublicKey:
		if len(key) == ed25519.PublicKeySize {
			return nil
		}
	}
	return errors.New("unsupported or weak certificate public key")
}

func hasServerAuth(cert *x509.Certificate) bool {
	for _, usage := range cert.ExtKeyUsage {
		if usage == x509.ExtKeyUsageServerAuth {
			return true
		}
	}
	return false
}
