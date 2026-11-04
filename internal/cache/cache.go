// Package cache is the GitHub Actions cache server for sandboxes. It serves
// the cache service v2 (Twirp github.actions.results.api.v1.CacheService),
// emulates the Azure Blob calls on the signed URLs that the service hands
// out, and sends all other results traffic, for example artifacts, to GitHub.
//
// A patched runner keeps the ACTIONS_RESULTS_URL of its .env file (see
// RunnerEnv), so jobs send cache calls here. The server keeps the entries of
// each repository apart and applies the GitHub branch rules: a job restores
// from its ref, its base ref, and the default branch, and saves only to its
// ref.
package cache

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultResultsURL is the GitHub results service.
const DefaultResultsURL = "https://results-receiver.actions.githubusercontent.com"

// staleUpload is the age at which the cleanup removes an unfinished upload.
const staleUpload = time.Hour

type Config struct {
	Dir      string // blob folder, for example /var/lib/runplane/cache
	Index    Index
	Resolver Resolver
	Stats    Stats // nil: no statistics
	// JobWait is how long a request of a known sandbox waits for its job;
	// zero means 60 seconds. runplane learns which job a runner took from
	// the scale set message, and GitHub can send that message 20 seconds or
	// more after the job starts, when the first cache step already runs.
	JobWait time.Duration
	// MaxBytes returns the byte limit of all entries. The server reads it at
	// each check, so a settings change applies at once. Nil or a value <= 0:
	// no limit.
	MaxBytes func() int64
	// DiskSpace returns the size and the free bytes of the disk of Dir. Nil:
	// the server does not look at the disk. With it, the limit is at most a
	// quarter of the disk.
	DiskSpace func() (total, free int64, err error)
	// MinFreePercent returns the part of the disk, 0 to 100, that the cache
	// leaves free: below it, the server takes no upload and removes the
	// least recently used entries. Nil: 0.
	MinFreePercent func() float64
	// SigningKey signs blob URLs. Empty: a random key, so URLs stop working
	// when the process restarts.
	SigningKey []byte
	ResultsURL string // empty: DefaultResultsURL
	Logger     *slog.Logger
}

// Server is the cache server. As an http.Handler it serves requests that
// come from sandboxes directly; Proxied serves requests that a trusted proxy
// forwards.
type Server struct {
	index     Index
	resolver  Resolver
	stats     Stats
	maxBytes  func() int64
	jobWait   time.Duration
	diskSpace func() (total, free int64, err error)
	minFree   func() float64
	disk      disk
	signer    signer
	proxy     *httputil.ReverseProxy
	log       *slog.Logger
	now       func() time.Time

	mu      sync.Mutex // serializes reservations and finalizations
	evictMu sync.Mutex
	uploads uploads
}

func New(cfg Config) (*Server, error) {
	if cfg.Dir == "" || cfg.Index == nil || cfg.Resolver == nil {
		return nil, errors.New("cache: Dir, Index, and Resolver are required")
	}
	s := &Server{
		index:     cfg.Index,
		resolver:  cfg.Resolver,
		stats:     cfg.Stats,
		maxBytes:  cfg.MaxBytes,
		jobWait:   cmp.Or(cfg.JobWait, 60*time.Second),
		diskSpace: cfg.DiskSpace,
		minFree:   cfg.MinFreePercent,
		disk:      disk{root: cfg.Dir},
		signer:    signer{key: cfg.SigningKey},
		log:       cfg.Logger,
		now:       time.Now,
	}
	if s.stats == nil {
		s.stats = noStats{}
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if len(s.signer.key) == 0 {
		s.signer.key = make([]byte, 32)
		rand.Read(s.signer.key)
	}
	if cfg.ResultsURL == "" {
		cfg.ResultsURL = DefaultResultsURL
	}
	upstream, err := url.Parse(cfg.ResultsURL)
	if err != nil || upstream.Host == "" {
		return nil, fmt.Errorf("cache: results URL %q is not valid", cfg.ResultsURL)
	}
	s.proxy = newProxy(upstream, s.log)
	if err := s.disk.init(); err != nil {
		return nil, err
	}
	return s, nil
}

// RunnerEnv returns the runner .env values that send the cache calls of a
// patched runner to the cache server at serviceURL, for example
// http://10.0.0.1:8486. The cache server must be at the root path:
// @actions/cache drops the path of ACTIONS_RESULTS_URL.
func RunnerEnv(serviceURL string) map[string]string {
	return map[string]string{
		"ACTIONS_RESULTS_URL":      strings.TrimRight(serviceURL, "/") + "/",
		"ACTIONS_CACHE_SERVICE_V2": "true",
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.serve(w, r, false)
}

// Proxied returns the handler for requests from a trusted proxy, such as the
// agent cache endpoint. It reads the sandbox from SandboxHeader, and the
// host for signed URLs from X-Forwarded-Host and X-Forwarded-Proto.
func (s *Server) Proxied() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, true)
	})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, trusted bool) {
	switch {
	case strings.HasPrefix(r.URL.Path, twirpPrefix):
		s.serveTwirp(w, r, trusted)
	case strings.HasPrefix(r.URL.Path, blobPrefix):
		s.serveBlob(w, r, trusted)
	default:
		s.serveResults(w, r, trusted)
	}
}

// serveResults sends a request to the GitHub results service. The service
// port listens on all addresses, so without a check anyone could send
// requests to GitHub through this server. A sandbox, with or without a job,
// and this host may; a trusted proxy checks its own sandboxes. In dev mode,
// Docker sends the requests of sandboxes from the loopback address.
func (s *Server) serveResults(w http.ResponseWriter, r *http.Request, trusted bool) {
	if !trusted && !fromLoopback(r) {
		_, err := s.sandbox(r, false)
		if errors.Is(err, ErrNoSandbox) {
			http.Error(w, "the request comes from no known sandbox", http.StatusForbidden)
			return
		}
		if err != nil {
			s.log.Error("cache: identify sandbox", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	s.proxy.ServeHTTP(w, r)
}

func fromLoopback(r *http.Request) bool {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	addr, err := netip.ParseAddr(ip)
	return err == nil && addr.IsLoopback()
}

// identify returns the Identity of a request, or an error that wraps
// ErrNoSandbox when the request has none.
func (s *Server) identify(r *http.Request, trusted bool) (Identity, error) {
	deadline := time.Now().Add(s.jobWait)
	for {
		ident, err := s.sandbox(r, trusted)
		if err != nil {
			return Identity{}, err
		}
		if ident.Repository != "" {
			return ident, nil
		}
		if time.Now().After(deadline) {
			return Identity{}, fmt.Errorf("sandbox %s has no job: %w", ident.SandboxID, ErrNoSandbox)
		}
		select {
		case <-r.Context().Done():
			return Identity{}, r.Context().Err()
		case <-time.After(time.Second):
		}
	}
}

// sandbox returns the sandbox of a request, which can have no job yet.
func (s *Server) sandbox(r *http.Request, trusted bool) (Identity, error) {
	header := r.Header
	if !trusted && header.Get(SandboxHeader) != "" {
		header = header.Clone()
		header.Del(SandboxHeader)
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	return s.resolver.Sandbox(r.Context(), ip, header)
}

// baseURL returns the scheme and host that the client used, for signed URLs
// that point back to this server.
func baseURL(r *http.Request, trusted bool) string {
	scheme, host := "http", r.Host
	if r.TLS != nil {
		scheme = "https"
	}
	if trusted {
		if h, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ","); h != "" {
			host = strings.TrimSpace(h)
		}
		if p, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ","); p != "" {
			scheme = strings.TrimSpace(p)
		}
	}
	return scheme + "://" + host
}

// Run removes unfinished uploads older than one hour and files that belong
// to no entry, and keeps the cache under its byte limit. It returns when ctx
// ends.
func (s *Server) Run(ctx context.Context) error {
	s.removeOrphans(ctx, "blobs")
	s.countUploads(ctx)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		s.cleanup(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (s *Server) cleanup(ctx context.Context) {
	stale, err := s.index.Unfinished(ctx, s.now().Add(-staleUpload))
	if err != nil {
		s.log.Error("cache: list unfinished uploads", "err", err)
	}
	for _, e := range stale {
		s.log.Info("cache: remove abandoned upload", "repository", e.Repository, "key", e.Key)
		if err := s.remove(ctx, e); err != nil {
			s.log.Error("cache: remove abandoned upload", "id", e.ID, "err", err)
		}
	}
	s.removeOrphans(ctx, "uploads")
	if err := s.evict(ctx, 0); err != nil {
		s.log.Error("cache: evict", "err", err)
	}
}

// removeOrphans removes files in a subfolder of the cache folder whose
// entry is gone, for example after a crash between two steps of a removal.
func (s *Server) removeOrphans(ctx context.Context, sub string) {
	for _, id := range s.disk.ids(sub) {
		_, err := s.index.Get(ctx, id)
		if !errors.Is(err, ErrNotFound) {
			continue
		}
		if err := s.disk.remove(Entry{ID: id, Path: blobPath(id)}); err != nil {
			s.log.Error("cache: remove orphan files", "id", id, "err", err)
		}
	}
}

// Delete removes one entry and its blob.
func (s *Server) Delete(ctx context.Context, id string) error {
	e, err := s.index.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get entry: %w", err)
	}
	return s.remove(ctx, e)
}

// DeleteRepository removes all entries of a repository and returns the count.
func (s *Server) DeleteRepository(ctx context.Context, repository string) (int, error) {
	list, err := s.index.List(ctx, repository)
	if err != nil {
		return 0, fmt.Errorf("list entries: %w", err)
	}
	for i, e := range list {
		if err := s.remove(ctx, e); err != nil {
			return i, err
		}
	}
	return len(list), nil
}

// remove deletes the index row first: if the file removal fails, the cleanup
// finds an orphan file, and no entry points to a missing blob.
func (s *Server) remove(ctx context.Context, e Entry) error {
	if err := s.index.Delete(ctx, e.ID); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("delete entry: %w", err)
	}
	if err := s.disk.remove(e); err != nil {
		return fmt.Errorf("delete blob: %w", err)
	}
	s.uploads.drop(e.ID)
	return nil
}

// newProxy forwards results traffic that is not for the cache, for example
// artifact uploads, to GitHub with the job's own token.
func newProxy(upstream *url.URL, log *slog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Header.Del(SandboxHeader)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("cache: results proxy", "path", r.URL.Path, "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}
