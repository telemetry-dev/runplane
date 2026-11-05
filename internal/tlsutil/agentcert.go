// Package tlsutil makes the self-signed agent certificate, pins it on the
// agent side, and serves the dashboard with ACME certificates.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const certLifetime = 10 * 365 * 24 * time.Hour

// AgentCert loads the agent listener certificate from dir/agent.crt and
// dir/agent.key. If either file is missing, it makes a new self-signed
// ECDSA P-256 certificate and writes both files. It returns the certificate
// and its fingerprint ("sha256:<hex>").
func AgentCert(dir string) (tls.Certificate, string, error) {
	certPath := filepath.Join(dir, "agent.crt")
	keyPath := filepath.Join(dir, "agent.key")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err == nil {
		return cert, Fingerprint(cert.Certificate[0]), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return tls.Certificate{}, "", fmt.Errorf("load agent certificate: %w", err)
	}

	certPEM, keyPEM, err := newSelfSigned()
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, "", fmt.Errorf("make tls dir: %w", err)
	}
	// Write the key first: a certificate without its key is useless, and a
	// key without its certificate makes a new pair at the next start.
	if err := writeFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := writeFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("parse agent certificate: %w", err)
	}
	return cert, Fingerprint(cert.Certificate[0]), nil
}

// Fingerprint returns "sha256:<hex>" of a DER certificate.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PinnedClientConfig returns a client TLS config that accepts only the
// certificate with this fingerprint. It does not use the system CA pool or
// check host names.
func PinnedClientConfig(fingerprint string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // VerifyPeerCertificate checks the pin.
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("server sent no certificate")
			}
			if got := Fingerprint(rawCerts[0]); !strings.EqualFold(got, fingerprint) {
				return fmt.Errorf("server certificate %s does not match pinned %s", got, fingerprint)
			}
			return nil
		},
	}
}

func newSelfSigned() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate agent key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "runplane agent listener"},
		NotBefore:             now.Add(-time.Hour), // allow for clock skew between servers
		NotAfter:              now.Add(certLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("make agent certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encode agent key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// writeFile writes data to a temporary file and renames it into place, so a
// crash never leaves a half-written file.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
