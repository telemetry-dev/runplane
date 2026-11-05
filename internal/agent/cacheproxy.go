package agent

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/runplane/runplane/internal/cache"
	"github.com/runplane/runplane/internal/sandbox"
)

// CacheProxy serves the Actions cache on the service port of an agent. It
// sends each request through the connection to the
// primary, where the cache server runs. It finds the sandbox from the
// source address of the request and refuses requests from other addresses.
// Run connects it to the primary.
type CacheProxy struct {
	driver    sandbox.Driver
	proxy     *httputil.ReverseProxy
	transport *http.Transport

	mu   sync.Mutex
	sess *yamux.Session // nil while the agent has no connection
}

func NewCacheProxy(driver sandbox.Driver, log *slog.Logger) *CacheProxy {
	p := &CacheProxy{driver: driver}
	p.transport = &http.Transport{
		DialContext:         p.dial,
		DisableCompression:  true,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     time.Minute,
	}
	p.proxy = &httputil.ReverseProxy{
		Rewrite:   rewriteCache,
		Transport: p.transport,
		ErrorLog:  slog.NewLogLogger(log.Handler(), slog.LevelDebug),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Debug("cache request to the primary failed", "path", r.URL.Path, "err", err)
			http.Error(w, "the runplane primary is not reachable", http.StatusBadGateway)
		},
	}
	return p
}

type sandboxKey struct{}

func (p *CacheProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := p.sandboxAt(r.Context(), r.RemoteAddr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if id == "" {
		http.Error(w, "no sandbox has this address", http.StatusForbidden)
		return
	}
	p.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sandboxKey{}, id)))
}

// rewriteCache sends a request to /v1/cache on the primary. The cache
// server makes its signed URLs from X-Forwarded-Host and X-Forwarded-Proto,
// so they point back at this service port.
func rewriteCache(pr *httputil.ProxyRequest) {
	in, out := pr.In, pr.Out
	out.URL.Scheme = "http"
	out.URL.Host = "primary"
	out.URL.Path = "/v1/cache" + in.URL.Path
	if in.URL.RawPath != "" {
		out.URL.RawPath = "/v1/cache" + in.URL.RawPath
	}
	out.Host = ""
	out.Header.Set(cache.SandboxHeader, in.Context().Value(sandboxKey{}).(string))
	out.Header.Set("X-Forwarded-Host", in.Host)
	proto := "http"
	if in.TLS != nil {
		proto = "https"
	}
	out.Header.Set("X-Forwarded-Proto", proto)
}

// sandboxAt returns the ID of the operating sandbox at the address of a
// request, or "" if there is none. It asks the driver each time, because a
// cached address can belong to a new sandbox a moment later.
func (p *CacheProxy) sandboxAt(ctx context.Context, remoteAddr string) (string, error) {
	addrPort, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return "", nil
	}
	list, err := p.driver.List(ctx)
	if err != nil {
		return "", err
	}
	sb, _ := sandbox.FindByIP(list, addrPort.Addr())
	return sb.ID, nil
}

func (p *CacheProxy) dial(context.Context, string, string) (net.Conn, error) {
	p.mu.Lock()
	sess := p.sess
	p.mu.Unlock()
	if sess == nil {
		return nil, errors.New("not connected to the primary")
	}
	return sess.Open()
}

func (p *CacheProxy) attach(sess *yamux.Session) {
	p.mu.Lock()
	p.sess = sess
	p.mu.Unlock()
}

func (p *CacheProxy) detach(sess *yamux.Session) {
	p.mu.Lock()
	if p.sess == sess {
		p.sess = nil
	}
	p.mu.Unlock()
	p.transport.CloseIdleConnections()
}
