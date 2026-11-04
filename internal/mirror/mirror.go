// Package mirror is a pull-through cache for Docker Hub. dockerd in each
// sandbox uses it as its registry mirror. It serves the read side of the
// registry API and keeps manifests and blobs on disk.
package mirror

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const dockerHub = "https://registry-1.docker.io"

// Config sets up a Mirror.
type Config struct {
	Dir      string // cache folder, for example /var/lib/runplane/mirror
	MaxBytes int64  // size limit of the cache; 0 = no limit
	Upstream string // registry URL; default https://registry-1.docker.io

	// Optional Docker Hub credentials (user name and personal access token)
	// for a higher pull limit. The mirror uses them for each pull, so that
	// sandboxes can also pull the private repositories of the account.
	Username string
	Token    string

	TagTTL time.Duration // how long a tag is trusted before a check with upstream; default 10 minutes

	// Allow reports if a client address can use the mirror. nil allows all.
	Allow func(ip netip.Addr) bool

	Client *http.Client // for upstream requests; default has a 1-minute response header timeout
	Logger *slog.Logger
}

// Stats counts the use of the mirror since the start. A request for a
// manifest or a blob is a hit if the mirror serves it from the cache.
type Stats struct {
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	HitBytes  int64 `json:"hit_bytes"`  // bytes sent to clients from the cache
	MissBytes int64 `json:"miss_bytes"` // bytes fetched from upstream
	SizeBytes int64 `json:"size_bytes"` // bytes in the cache now
}

// Add adds the counts of o to s.
func (s *Stats) Add(o Stats) {
	s.Hits += o.Hits
	s.Misses += o.Misses
	s.HitBytes += o.HitBytes
	s.MissBytes += o.MissBytes
	s.SizeBytes += o.SizeBytes
}

// Mirror is an http.Handler for /v2/.
type Mirror struct {
	up     *upstream
	store  *store
	tags   *tagIndex
	allow  func(netip.Addr) bool
	tagTTL time.Duration
	log    *slog.Logger

	ctx    context.Context // ends at Close
	cancel context.CancelFunc
	wg     sync.WaitGroup // upstream fetches

	mu        sync.Mutex
	downloads map[string]*download // by digest

	tagFlights      flights[string]
	manifestFlights flights[manifest]

	hits, misses, hitBytes, missBytes atomic.Int64
}

// New opens the cache in cfg.Dir. Call Close after the HTTP server stops.
func New(cfg Config) (*Mirror, error) {
	if cfg.Dir == "" {
		return nil, errors.New("mirror: no cache folder")
	}
	s, err := openStore(cfg.Dir, cfg.MaxBytes)
	if err != nil {
		return nil, fmt.Errorf("open mirror cache: %w", err)
	}
	tags, err := openTags(filepath.Join(cfg.Dir, "tags"))
	if err != nil {
		return nil, fmt.Errorf("open mirror cache: %w", err)
	}
	client := cfg.Client
	if client == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = time.Minute
		client = &http.Client{Transport: t}
	}
	m := &Mirror{
		up: &upstream{
			base:   strings.TrimSuffix(cmp.Or(cfg.Upstream, dockerHub), "/"),
			client: client,
			user:   cfg.Username,
			pass:   cfg.Token,
		},
		store:     s,
		tags:      tags,
		allow:     cfg.Allow,
		tagTTL:    cmp.Or(cfg.TagTTL, 10*time.Minute),
		log:       cmp.Or(cfg.Logger, slog.New(slog.DiscardHandler)),
		downloads: map[string]*download{},
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m, nil
}

// Close stops the upstream fetches and waits for them.
func (m *Mirror) Close() error {
	m.cancel()
	m.wg.Wait()
	return nil
}

func (m *Mirror) Stats() Stats {
	return Stats{
		Hits:      m.hits.Load(),
		Misses:    m.misses.Load(),
		HitBytes:  m.hitBytes.Load(),
		MissBytes: m.missBytes.Load(),
		SizeBytes: m.store.bytes(),
	}
}

func (m *Mirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if !m.allowed(r) {
		writeError(w, http.StatusForbidden, "DENIED", "only sandboxes can use the registry mirror")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "the registry mirror is read-only")
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/v2")
	if !ok {
		writeError(w, http.StatusNotFound, "UNSUPPORTED", "not a registry path")
		return
	}
	if path == "" || path == "/" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
		return
	}
	name, kind, ref, err := parsePath(path)
	if err != nil {
		m.fail(w, r, err, "")
		return
	}
	if kind == manifestsDir {
		m.serveManifest(w, r, name, ref)
	} else {
		m.serveBlob(w, r, name, ref)
	}
}

func (m *Mirror) allowed(r *http.Request) bool {
	if m.allow == nil {
		return true
	}
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	return err == nil && m.allow(ap.Addr().Unmap())
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	tagRe    = regexp.MustCompile(`^\w[\w.-]{0,127}$`)
	digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// parsePath splits a path such as /alpine/manifests/latest (after /v2) into
// the repository name, the kind ("manifests" or "blobs"), and the reference.
// It adds "library/" to the names of official images, as Docker Hub does.
func parsePath(path string) (name, kind, ref string, err error) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	n := len(parts)
	if n < 3 || (parts[n-2] != manifestsDir && parts[n-2] != blobsDir) {
		return "", "", "", &apiError{http.StatusNotFound, "UNSUPPORTED", "the registry mirror serves only manifests and blobs"}
	}
	name, kind, ref = strings.Join(parts[:n-2], "/"), parts[n-2], parts[n-1]
	if len(name) > 255 || !nameRe.MatchString(name) {
		return "", "", "", &apiError{http.StatusBadRequest, "NAME_INVALID", "invalid repository name"}
	}
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}
	if !digestRe.MatchString(ref) && (kind == blobsDir || !tagRe.MatchString(ref)) {
		return "", "", "", &apiError{http.StatusBadRequest, "DIGEST_INVALID", "invalid tag or sha256 digest"}
	}
	return name, kind, ref, nil
}

// apiError is an error with its registry error code.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

// fail answers with the registry error for err. unknown is the error code for
// content that upstream does not have.
func (m *Mirror) fail(w http.ResponseWriter, r *http.Request, err error, unknown string) {
	if r.Context().Err() != nil {
		return // the client left
	}
	var ae *apiError
	var ue *upstreamError
	switch {
	case errors.As(err, &ae):
		writeError(w, ae.status, ae.code, ae.message)
	case notFound(err):
		writeError(w, http.StatusNotFound, unknown, "not found upstream")
	case errors.As(err, &ue) && ue.status == http.StatusTooManyRequests:
		m.log.Warn("docker hub pull limit reached", "path", r.URL.Path)
		writeError(w, http.StatusTooManyRequests, "TOOMANYREQUESTS", "Docker Hub pull limit reached")
	default:
		m.log.Warn("registry mirror upstream failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusBadGateway, "UNKNOWN", err.Error())
	}
}

// notFound reports if upstream does not have the content or does not give
// access to it. Docker Hub answers 401 for a repository that does not exist.
func notFound(err error) bool {
	var ue *upstreamError
	return errors.As(err, &ue) &&
		(ue.status == http.StatusNotFound || ue.status == http.StatusUnauthorized || ue.status == http.StatusForbidden)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	type item struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	json.NewEncoder(w).Encode(struct {
		Errors []item `json:"errors"`
	}{[]item{{code, message}}})
}

// flights runs one call for each key at a time. Callers that ask for a key
// while its call runs get the same result.
type flights[T any] struct {
	mu    sync.Mutex
	calls map[string]*flight[T]
}

type flight[T any] struct {
	done chan struct{}
	val  T
	err  error
}

// do starts fn in wg if no call for key runs. It waits for the result or for
// the end of ctx. fn does not stop at the end of ctx.
func (fs *flights[T]) do(ctx context.Context, wg *sync.WaitGroup, key string, fn func() (T, error)) (T, error) {
	fs.mu.Lock()
	f, ok := fs.calls[key]
	if !ok {
		if fs.calls == nil {
			fs.calls = map[string]*flight[T]{}
		}
		f = &flight[T]{done: make(chan struct{})}
		fs.calls[key] = f
		wg.Go(func() {
			f.val, f.err = fn()
			fs.mu.Lock()
			delete(fs.calls, key)
			fs.mu.Unlock()
			close(f.done)
		})
	}
	fs.mu.Unlock()
	select {
	case <-f.done:
		return f.val, f.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}
