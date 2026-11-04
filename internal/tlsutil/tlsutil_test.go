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
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
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
		t.Fatalf("fingerprint %s, want %s", fp1, want)
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Fatalf("key is %T, want ECDSA P-256", cert.PublicKey)
	}
	if left := time.Until(cert.NotAfter); left < 9*365*24*time.Hour {
		t.Fatalf("certificate expires in %v, want about 10 years", left)
	}

	info, err := os.Stat(filepath.Join(dir, "agent.key"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("agent.key mode %o, want 600", mode)
	}
}

func TestPinnedClientConfig(t *testing.T) {
	cert, fp, err := AgentCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				c.(*tls.Conn).Handshake()
				c.Close()
			}()
		}
	}()

	_, otherFP, err := AgentCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		pin     string
		wantErr bool
	}{
		{"matching pin", fp, false},
		{"other certificate", otherFP, true},
		{"empty pin", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := tls.Dial("tcp", ln.Addr().String(), PinnedClientConfig(tt.pin))
			if err == nil {
				conn.Close()
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("dial error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

func TestACMEPort80(t *testing.T) {
	a := NewACME(t.TempDir(), slog.New(slog.DiscardHandler))
	a.HTTPSAddr, a.HTTPAddr = freeAddr(t), freeAddr(t)
	if err := a.Start("rp.example.com", "ops@example.com", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop(context.Background()) })

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(host, path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://"+a.HTTPAddr+path, nil)
		req.Host = host
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	// A fake Host header must not turn port 80 into an open redirect.
	resp := get("evil.example.net", "/jobs?status=failed")
	if resp.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("status %d, want 308", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://rp.example.com/jobs?status=failed" {
		t.Fatalf("Location %q", loc)
	}

	// Challenge requests reach autocert instead of the redirect: an unknown
	// token is 404 for our domain and 403 for other hosts.
	if resp := get("rp.example.com", "/.well-known/acme-challenge/abc"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("challenge for our domain: status %d, want 404", resp.StatusCode)
	}
	if resp := get("evil.example.net", "/.well-known/acme-challenge/abc"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("challenge for other host: status %d, want 403", resp.StatusCode)
	}
}

func TestACMERejectsOtherHosts(t *testing.T) {
	a := NewACME(t.TempDir(), slog.New(slog.DiscardHandler))
	a.HTTPSAddr, a.HTTPAddr = freeAddr(t), freeAddr(t)
	if err := a.Start("rp.example.com", "", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop(context.Background()) })

	// Without the host policy, any SNI name would make autocert ask the CA
	// for a certificate and use up the rate limit.
	conn, err := tls.Dial("tcp", a.HTTPSAddr, &tls.Config{ServerName: "evil.example.net", InsecureSkipVerify: true})
	if err == nil {
		conn.Close()
		t.Fatal("handshake for another host name succeeded")
	}
}

func TestACMERestart(t *testing.T) {
	a := NewACME(t.TempDir(), slog.New(slog.DiscardHandler))
	a.HTTPSAddr, a.HTTPAddr = freeAddr(t), freeAddr(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })

	if err := a.Start("one.example.com", "", ok); err != nil {
		t.Fatal(err)
	}
	// A second Start with new settings replaces the servers on the same ports.
	if err := a.Start("two.example.com", "", ok); err != nil {
		t.Fatalf("restart: %v", err)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get("http://" + a.HTTPAddr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "https://two.example.com/" {
		t.Fatalf("after restart Location %q, want the new domain", loc)
	}

	a.Stop(context.Background())
	if conn, err := net.DialTimeout("tcp", a.HTTPAddr, time.Second); err == nil {
		conn.Close()
		t.Fatal("port 80 still accepts connections after Stop")
	}
}

func TestACMEStatus(t *testing.T) {
	dir := t.TempDir()
	a := NewACME(dir, slog.New(slog.DiscardHandler))
	a.HTTPSAddr, a.HTTPAddr = freeAddr(t), freeAddr(t)
	ctx := context.Background()
	if s := a.Status(ctx); s.State != CertNone {
		t.Fatalf("status before Start = %+v, want none", s)
	}
	if err := a.Start("rp.example.com", "", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop(ctx) })

	// A failed handshake for another name is not a certificate error.
	if conn, err := tls.Dial("tcp", a.HTTPSAddr, &tls.Config{ServerName: "evil.example.net", InsecureSkipVerify: true}); err == nil {
		conn.Close()
	}
	if s := a.Status(ctx); s.State != CertRequesting || s.Error != "" {
		t.Fatalf("status without a certificate = %+v, want requesting with no error", s)
	}

	// autocert keeps the key and the chain under the domain name.
	notAfter := time.Now().Add(60 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := os.WriteFile(filepath.Join(dir, "rp.example.com"), cachedCert(t, "rp.example.com", notAfter), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := a.Status(ctx); s.State != CertValid || !s.NotAfter.Equal(notAfter) {
		t.Fatalf("status with a certificate = %+v, want valid until %s", s, notAfter)
	}
	a.Stop(ctx)
	if s := a.Status(ctx); s.State != CertNone {
		t.Fatalf("status after Stop = %+v, want none", s)
	}
}

// Another web server on port 443 must show in the dashboard, and HTTPS must
// start when the port is free again.
func TestACMEPortInUse(t *testing.T) {
	ctx := context.Background()
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := NewACME(t.TempDir(), slog.New(slog.DiscardHandler))
	a.HTTPSAddr, a.HTTPAddr, a.retryEvery = held.Addr().String(), freeAddr(t), 20*time.Millisecond
	t.Cleanup(func() { a.Stop(ctx) })

	if err := a.Start("rp.example.com", "", http.NotFoundHandler()); err == nil {
		t.Fatal("Start with the HTTPS port in use has no error")
	}
	_, port, _ := net.SplitHostPort(a.HTTPSAddr)
	if s := a.Status(ctx); s.State != CertError || !strings.Contains(s.Error, port) {
		t.Fatalf("status with the port in use = %+v, want an error that names port %s", s, port)
	}

	held.Close()
	waitFor(t, func() bool { return a.Status(ctx).State == CertRequesting })
	if conn, err := net.DialTimeout("tcp", a.HTTPSAddr, time.Second); err != nil {
		t.Fatalf("HTTPS does not listen after the port is free: %v", err)
	} else {
		conn.Close()
	}
}

// After Stop, the domain is gone: no error shows, and no later try takes
// the port.
func TestACMEStopEndsTheTries(t *testing.T) {
	ctx := context.Background()
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := NewACME(t.TempDir(), slog.New(slog.DiscardHandler))
	a.HTTPSAddr, a.HTTPAddr, a.retryEvery = held.Addr().String(), freeAddr(t), 20*time.Millisecond
	if err := a.Start("rp.example.com", "", http.NotFoundHandler()); err == nil {
		t.Fatal("Start with the HTTPS port in use has no error")
	}
	a.Stop(ctx)
	held.Close()
	if s := a.Status(ctx); s.State != CertNone {
		t.Fatalf("status after Stop = %+v, want none", s)
	}
	time.Sleep(10 * a.retryEvery)
	if conn, err := net.DialTimeout("tcp", a.HTTPSAddr, time.Second); err == nil {
		conn.Close()
		t.Fatal("a try after Stop opened the HTTPS port")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}

// cachedCert is an autocert cache entry: the private key, then the
// certificate.
func cachedCert(t *testing.T, domain string, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{domain},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
	}
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

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}
