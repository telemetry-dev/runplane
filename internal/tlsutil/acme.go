package tlsutil

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// ACME serves the dashboard over HTTPS with a Let's Encrypt certificate for
// one domain. Port 80 answers HTTP-01 challenges and sends all other requests
// to HTTPS. Start and Stop can be called at any time when the settings change.
type ACME struct {
	cacheDir            string
	log                 *slog.Logger
	httpsAddr, httpAddr string
	retryEvery          time.Duration

	mu      sync.Mutex
	manager *autocert.Manager
	domain  string
	email   string
	servers []server
	portErr string      // why the ports did not open
	retry   *time.Timer // the next try to open the ports

	// lastErr is the error text of the last certificate request for the
	// domain. It is apart from mu, because Stop holds mu while handshakes end.
	lastErr atomic.Value
}

type server struct {
	*http.Server
	ln net.Listener
}

// The listen addresses of ACME. The HTTP-01 challenges of Let's Encrypt
// come to port 80.
const (
	HTTPSAddr = ":443"
	HTTPAddr  = ":80"
)

// NewACME returns an ACME server that keeps its account key and certificates
// in cacheDir.
func NewACME(cacheDir string, log *slog.Logger) *ACME {
	a := &ACME{cacheDir: cacheDir, log: log, httpsAddr: HTTPSAddr, httpAddr: HTTPAddr, retryEvery: time.Minute}
	a.setErr(nil)
	return a
}

// Start serves handler over HTTPS for domain. If the servers already run,
// Start stops them first. It returns an error if a port cannot be bound, for
// example because another web server has it. Status then reports the error,
// and ACME tries the ports again each minute until Start or Stop.
func (a *ACME) Start(domain, email string, handler http.Handler) error {
	if domain == "" {
		return errors.New("start https: no domain")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.start(domain, email, handler)
}

func (a *ACME) start(domain, email string, handler http.Handler) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.stop(ctx)

	// autocert has no way to stop the renewal timers of a manager, so keep
	// the manager while the domain and email stay the same.
	if a.manager == nil || a.domain != domain || a.email != email {
		a.manager = &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(a.cacheDir),
			HostPolicy: autocert.HostWhitelist(domain),
			Email:      email,
		}
		a.domain, a.email = domain, email
		a.setErr(nil)
	}

	httpsLn, err := net.Listen("tcp", a.httpsAddr)
	if err != nil {
		return a.retryLater(err, domain, email, handler)
	}
	httpLn, err := net.Listen("tcp", a.httpAddr)
	if err != nil {
		httpsLn.Close()
		return a.retryLater(err, domain, email, handler)
	}

	errLog := slog.NewLogLogger(a.log.Handler(), slog.LevelDebug)
	tlsConfig := a.manager.TLSConfig()
	getCert := tlsConfig.GetCertificate
	tlsConfig.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		cert, err := getCert(hello)
		// Handshakes for other names, for example from scanners, fail by
		// the host policy and say nothing about the certificate.
		if strings.EqualFold(strings.TrimSuffix(hello.ServerName, "."), domain) {
			a.setErr(err)
		}
		return cert, err
	}
	httpsSrv := &http.Server{
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          errLog,
	}
	httpSrv := &http.Server{
		Handler:           a.manager.HTTPHandler(redirect(domain)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          errLog,
	}
	a.servers = []server{{httpsSrv, httpsLn}, {httpSrv, httpLn}}

	go a.serve("https", func() error { return httpsSrv.ServeTLS(httpsLn, "", "") })
	go a.serve("http", func() error { return httpSrv.Serve(httpLn) })
	a.log.Info("https started", "domain", domain, "https", httpsLn.Addr().String(), "http", httpLn.Addr().String())
	return nil
}

func (a *ACME) retryLater(err error, domain, email string, handler http.Handler) error {
	a.portErr = "Cannot open the port: " + err.Error()
	if op, ok := errors.AsType[*net.OpError](err); ok && op.Addr != nil {
		_, port, _ := net.SplitHostPort(op.Addr.String())
		a.portErr = fmt.Sprintf("Cannot open port %s: %v", port, op.Err)
	}
	var t *time.Timer
	t = time.AfterFunc(a.retryEvery, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.retry != t {
			return // Start or Stop came first
		}
		if err := a.start(domain, email, handler); err != nil {
			a.log.Debug("https ports still do not open", "domain", domain, "err", err)
		}
	})
	a.retry = t
	return err
}

// Certificate states of Status.
const (
	CertNone       = "none"       // HTTPS is off
	CertRequesting = "requesting" // no valid certificate yet
	CertValid      = "valid"
	CertError      = "error" // the ports do not open, or no valid certificate and the last request failed
)

type CertStatus struct {
	State    string
	NotAfter time.Time // zero without a valid certificate
	Error    string    // why the last certificate request failed
}

// Status reads the certificate of the domain from the cache. A failed
// request is an error state only while no valid certificate exists, so a
// failed renewal keeps the state valid with the error.
func (a *ACME) Status(ctx context.Context) CertStatus {
	a.mu.Lock()
	on, manager, domain, portErr := len(a.servers) > 0, a.manager, a.domain, a.portErr
	a.mu.Unlock()
	if portErr != "" {
		return CertStatus{State: CertError, Error: portErr}
	}
	if !on {
		return CertStatus{State: CertNone}
	}
	s := CertStatus{State: CertRequesting, Error: a.lastErr.Load().(string)}
	if data, err := manager.Cache.Get(ctx, domain); err == nil {
		if leaf := leafOf(data); leaf != nil && time.Now().Before(leaf.NotAfter) {
			s.State, s.NotAfter = CertValid, leaf.NotAfter
			return s
		}
	}
	if s.Error != "" {
		s.State = CertError
	}
	return s
}

func (a *ACME) setErr(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	a.lastErr.Store(msg)
}

// leafOf returns the first certificate of an autocert cache entry: a PEM
// private key, then the certificate chain.
func leafOf(data []byte) *x509.Certificate {
	for {
		var b *pem.Block
		if b, data = pem.Decode(data); b == nil {
			return nil
		}
		if b.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return nil
			}
			return cert
		}
	}
}

// Stop closes long requests, such as event streams, when ctx ends.
func (a *ACME) Stop(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stop(ctx)
}

func (a *ACME) stop(ctx context.Context) {
	if a.retry != nil {
		a.retry.Stop()
		a.retry = nil
	}
	a.portErr = ""
	if len(a.servers) == 0 {
		return
	}
	for _, srv := range a.servers {
		if err := srv.Shutdown(ctx); err != nil {
			srv.Close()
		}
		// Serve may not have taken the listener yet; close it here so the
		// port is free when Stop returns.
		srv.ln.Close()
	}
	a.servers = nil
	a.log.Info("https stopped", "domain", a.domain)
}

func (a *ACME) serve(name string, serve func() error) {
	if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		a.log.Error("https server failed", "server", name, "err", err)
	}
}

// redirect ignores the request Host header, so port 80 is not an open
// redirect.
func redirect(domain string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+domain+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}
