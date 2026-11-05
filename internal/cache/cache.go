// Package cache is the GitHub Actions cache server for sandboxes. It serves
// the cache service v2 (Twirp github.actions.results.api.v1.CacheService),
// emulates the Azure Blob calls on the signed URLs that the service hands
// out, and sends all other results traffic, for example artifacts, to GitHub.
// A patched runner keeps the ACTIONS_RESULTS_URL of its .env file (see
// guest.NewConfig), so jobs send cache calls here.
package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/store"
)

var resultsURL = &url.URL{Scheme: "https", Host: "results-receiver.actions.githubusercontent.com"}

const (
	// staleUpload is the age at which the cleanup removes an unfinished upload.
	staleUpload = time.Hour
	// jobWait is how long a request of a known sandbox waits for its job.
	// runplane learns which job a runner took from the scale set message, and
	// GitHub can send that message 20 seconds or more after the job starts,
	// when the first cache step already runs.
	jobWait = time.Minute
)

type Config struct {
	Dir      string       // blob folder, for example /var/lib/runplane/cache
	Store    *store.Store // the cache_entries table; the blobs stay in Dir
	Resolver Resolver
	Stats    *Stats
	// MaxBytes returns the byte limit of all entries; a value <= 0 means no
	// limit. The server reads it at each check, so a settings change applies
	// at once.
	MaxBytes func() int64
	// DiskSpace returns the size and the free bytes of the disk of Dir. The
	// limit is at most a quarter of the disk.
	DiskSpace func() (total, free int64, err error)
	// MinFreePercent returns the part of the disk, 0 to 100, that the cache
	// leaves free: below it, the server takes no upload and removes the
	// least recently used entries.
	MinFreePercent func() float64
	SigningKey     []byte // signs blob URLs
	Logger         *slog.Logger
}

// Server is the cache server. As an http.Handler it serves requests that
// come from sandboxes directly; Proxied serves requests that a trusted proxy
// forwards.
type Server struct {
	cfg     Config
	disk    disk
	jobWait time.Duration
	proxy   *httputil.ReverseProxy
	now     func() time.Time

	mu      sync.Mutex // serializes reservations and finalizations
	evictMu sync.Mutex
	uploads uploads
}

func New(cfg Config) (*Server, error) {
	cfg.Logger = cmp.Or(cfg.Logger, slog.New(slog.DiscardHandler))
	s := &Server{
		cfg:     cfg,
		disk:    disk{root: cfg.Dir},
		jobWait: jobWait,
		proxy:   newProxy(resultsURL, cfg.Logger),
		now:     time.Now,
		uploads: uploads{onDisk: map[string]int64{}, owner: map[string]string{}},
	}
	if err := s.disk.init(); err != nil {
		return nil, err
	}
	return s, nil
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
			s.cfg.Logger.Error("cache: identify sandbox", "err", err)
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

const (
	// SandboxHeader names the sandbox of a request that a trusted proxy (the
	// agent cache endpoint) forwards. The server ignores it on other requests.
	SandboxHeader = "X-Runplane-Sandbox"
	// ServerHeader names the server of a cache request that an agent sends.
	// The primary sets it, so that the resolver can check that the sandbox of
	// the request belongs to that server.
	ServerHeader = "X-Runplane-Server"
)

// ErrNoSandbox means that no sandbox with a job matches a request.
var ErrNoSandbox = errors.New("no sandbox for this request")

// Identity is the sandbox and job behind a request, from runplane records.
// The cache scopes in the runtime token of a job do not count: runplane
// cannot check the signature of the token, so the job controls them.
type Identity struct {
	SandboxID     string
	Repository    string // owner/name of the job; empty if the sandbox has no job
	DefaultBranch string // for example "main"; empty if runplane does not know the repository
	// Ref is the ref of the run: "refs/heads/<branch>", "refs/tags/<tag>",
	// or "refs/pull/<n>/merge". Empty if runplane does not know it.
	Ref string
	// BaseRef is the base branch of a pull request, for example "refs/heads/main".
	BaseRef string
}

// Resolver finds the sandbox behind a request. header contains SandboxHeader
// only when the request came through a trusted proxy; then the sandbox ID
// identifies the sandbox, and remoteIP is the address of the proxy.
// Sandbox returns ErrNoSandbox if no sandbox matches.
type Resolver interface {
	Sandbox(ctx context.Context, remoteIP string, header http.Header) (Identity, error)
}

// readRefs returns the refs that the job may restore from, in search order:
// its ref, its base ref, and the default branch, like GitHub. The job saves
// only to Ref.
func (id Identity) readRefs() []string {
	var defaultRef string
	if id.DefaultBranch != "" {
		defaultRef = "refs/heads/" + strings.TrimPrefix(id.DefaultBranch, "refs/heads/")
	}
	return dedupe(slices.DeleteFunc([]string{id.Ref, id.BaseRef, defaultRef}, func(r string) bool { return r == "" }))
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
	return s.cfg.Resolver.Sandbox(r.Context(), ip, header)
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

// Run removes stale uploads and files that belong to no entry, and keeps the
// cache under its byte limit. It returns when ctx ends.
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
	stale, err := s.cfg.Store.ListUnfinishedCacheEntries(ctx, s.now().Add(-staleUpload))
	if err != nil {
		s.cfg.Logger.Error("cache: list unfinished uploads", "err", err)
	}
	for _, e := range stale {
		s.cfg.Logger.Info("cache: remove abandoned upload", "repository", e.Repository, "key", e.Key)
		if err := s.remove(ctx, e); err != nil {
			s.cfg.Logger.Error("cache: remove abandoned upload", "id", e.ID, "err", err)
		}
	}
	s.removeOrphans(ctx, "uploads")
	if err := s.evict(ctx, 0); err != nil {
		s.cfg.Logger.Error("cache: evict", "err", err)
	}
}

// removeOrphans removes files in a subfolder of the cache folder whose
// entry is gone, for example after a crash between two steps of a removal.
func (s *Server) removeOrphans(ctx context.Context, sub string) {
	for _, id := range s.disk.ids(sub) {
		if _, err := s.cfg.Store.GetCacheEntry(ctx, id); !errors.Is(err, ErrNotFound) {
			continue
		}
		if err := s.disk.remove(id); err != nil {
			s.cfg.Logger.Error("cache: remove orphan files", "id", id, "err", err)
		}
	}
}

// Delete removes one entry and its blob.
func (s *Server) Delete(ctx context.Context, id string) error {
	e, err := s.cfg.Store.GetCacheEntry(ctx, id)
	if err != nil {
		return fmt.Errorf("get entry: %w", err)
	}
	return s.remove(ctx, e)
}

// DeleteRepository removes all entries of a repository and returns the count.
func (s *Server) DeleteRepository(ctx context.Context, repository string) (int, error) {
	// The store lists all repositories for "".
	if repository == "" {
		return 0, nil
	}
	list, err := s.cfg.Store.ListCacheEntries(ctx, repository)
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
func (s *Server) remove(ctx context.Context, e store.CacheEntry) error {
	if _, err := s.cfg.Store.DeleteCacheEntries(ctx, "", e.ID); err != nil {
		return fmt.Errorf("delete entry: %w", err)
	}
	if err := s.disk.remove(e.ID); err != nil {
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
