package tlsutil

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentCertPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	_, fp1, err := AgentCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, fp2, err := AgentCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Fatalf("second start made a new certificate: %s != %s; pinned agents would fail", fp1, fp2)
	}
	// The install script computes the fingerprint from agent.crt with openssl;
	// it must match what AgentCert reports.
	data, err := os.ReadFile(filepath.Join(dir, "agent.crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	sum := sha256.Sum256(block.Bytes)
	if want := "sha256:" + hex.EncodeToString(sum[:]); fp1 != want {
		t.Errorf("fingerprint %s, want %s", fp1, want)
	}
	if info, err := os.Stat(filepath.Join(dir, "agent.key")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("agent.key: %v, %v; want mode 600", info, err)
	}
}

func TestPinnedClientConfig(t *testing.T) {
	cert, fp, err := AgentCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()
	_, otherFP, err := AgentCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for pin, wantErr := range map[string]bool{fp: false, otherFP: true, "": true} {
		conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), PinnedClientConfig(pin))
		if err == nil {
			conn.Close()
		}
		if (err != nil) != wantErr {
			t.Errorf("pin %q: dial error = %v, want error %v", pin, err, wantErr)
		}
	}
}

func TestACME(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	a := testACME(t, dir, freeAddr(t))
	if s := a.Status(ctx); s.State != CertNone {
		t.Fatalf("status before Start = %+v, want none", s)
	}
	if err := a.Start("old.example.com", "", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	// A second Start with new settings replaces the servers on the same ports.
	if err := a.Start("rp.example.com", "ops@example.com", http.NotFoundHandler()); err != nil {
		t.Fatalf("restart: %v", err)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(host, path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://"+a.httpAddr+path, nil)
		req.Host = host
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	// A fake Host header must not turn port 80 into an open redirect.
	if resp := get("evil.example.net", "/jobs?status=failed"); resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "https://rp.example.com/jobs?status=failed" {
		t.Errorf("redirect: status %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Challenge requests reach autocert instead of the redirect: an unknown
	// token is 404 for our domain and 403 for other hosts.
	if resp := get("rp.example.com", "/.well-known/acme-challenge/abc"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("challenge for our domain: status %d, want 404", resp.StatusCode)
	}
	if resp := get("evil.example.net", "/.well-known/acme-challenge/abc"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("challenge for other host: status %d, want 403", resp.StatusCode)
	}

	// Without the host policy, any SNI name would make autocert ask the CA
	// for a certificate and use up the rate limit. The failed handshake is
	// not a certificate error.
	if conn, err := tls.Dial("tcp", a.httpsAddr, &tls.Config{ServerName: "evil.example.net", InsecureSkipVerify: true}); err == nil {
		conn.Close()
		t.Error("handshake for another host name succeeded")
	}
	if s := a.Status(ctx); s.State != CertRequesting || s.Error != "" {
		t.Errorf("status without a certificate = %+v, want requesting with no error", s)
	}

	// autocert keeps the key and the chain under the domain name.
	notAfter := time.Now().Add(60 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := os.WriteFile(filepath.Join(dir, "rp.example.com"), cachedCert(t, "rp.example.com", notAfter), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := a.Status(ctx); s.State != CertValid || !s.NotAfter.Equal(notAfter) {
		t.Errorf("status with a certificate = %+v, want valid until %s", s, notAfter)
	}

	a.Stop(ctx)
	if s := a.Status(ctx); s.State != CertNone {
		t.Errorf("status after Stop = %+v, want none", s)
	}
	if conn, err := net.DialTimeout("tcp", a.httpAddr, time.Second); err == nil {
		conn.Close()
		t.Error("port 80 still accepts connections after Stop")
	}
}

// Another web server on port 443 must show in the dashboard, and HTTPS must
// start when the port is free again.
func TestACMEPortInUse(t *testing.T) {
	ctx := t.Context()
	held := listen(t)
	a := testACME(t, t.TempDir(), held.Addr().String())
	if err := a.Start("rp.example.com", "", http.NotFoundHandler()); err == nil {
		t.Fatal("Start with the HTTPS port in use has no error")
	}
	_, port, _ := net.SplitHostPort(a.httpsAddr)
	if s := a.Status(ctx); s.State != CertError || !strings.Contains(s.Error, port) {
		t.Fatalf("status with the port in use = %+v, want an error that names port %s", s, port)
	}

	held.Close()
	for deadline := time.Now().Add(5 * time.Second); a.Status(ctx).State != CertRequesting; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("HTTPS did not start after the port was free")
		}
	}
	conn, err := net.DialTimeout("tcp", a.httpsAddr, time.Second)
	if err != nil {
		t.Fatalf("HTTPS does not listen after the port is free: %v", err)
	}
	conn.Close()
}

// After Stop, the domain is gone: no error shows, and no later try takes
// the port.
func TestACMEStopEndsTheTries(t *testing.T) {
	ctx := t.Context()
	held := listen(t)
	a := testACME(t, t.TempDir(), held.Addr().String())
	if err := a.Start("rp.example.com", "", http.NotFoundHandler()); err == nil {
		t.Fatal("Start with the HTTPS port in use has no error")
	}
	a.Stop(ctx)
	held.Close()
	if s := a.Status(ctx); s.State != CertNone {
		t.Fatalf("status after Stop = %+v, want none", s)
	}
	time.Sleep(10 * a.retryEvery)
	if conn, err := net.DialTimeout("tcp", a.httpsAddr, time.Second); err == nil {
		conn.Close()
		t.Fatal("a try after Stop opened the HTTPS port")
	}
}

func testACME(t *testing.T, dir, httpsAddr string) *ACME {
	a := NewACME(dir, slog.New(slog.DiscardHandler))
	a.httpsAddr, a.httpAddr, a.retryEvery = httpsAddr, freeAddr(t), 20*time.Millisecond
	t.Cleanup(func() { a.Stop(context.Background()) })
	return a
}

// cachedCert is an autocert cache entry: the private key, then the
// certificate.
func cachedCert(t *testing.T, domain string, notAfter time.Time) []byte {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{domain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
}

func listen(t *testing.T) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func freeAddr(t *testing.T) string {
	ln := listen(t)
	defer ln.Close()
	return ln.Addr().String()
}
