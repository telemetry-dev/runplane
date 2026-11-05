package cache

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/datadisk"
	"github.com/runplane/runplane/internal/store"
)

// sandboxes resolves by sandbox ID when the header has one, else by IP.
type sandboxes map[string]Identity

func (s sandboxes) Sandbox(_ context.Context, ip string, h http.Header) (Identity, error) {
	key := ip
	if id := h.Get(SandboxHeader); id != "" {
		key = id
	}
	ident, ok := s[key]
	if !ok {
		return Identity{}, ErrNoSandbox
	}
	return ident, nil
}

type clock struct{ ns atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) advance(d time.Duration) { c.ns.Add(int64(d)) }

type fixture struct {
	limit   atomic.Int64
	disk    func() datadisk.Space // if set, the room of the cache instead of limit
	srv     *Server
	st      *store.Store
	clock   clock
	dir     string
	url     string // direct sandbox traffic
	proxied string // traffic from a trusted proxy
}

// newFixture starts a server whose requests from 127.0.0.1 belong to
// sandbox "sb-1" with a push job on main of acme/app. The other sandboxes
// send through the trusted proxy (see via). opts change the config.
func newFixture(t *testing.T, maxBytes int64, opts ...func(*Config)) *fixture {
	t.Helper()
	f := &fixture{st: openStore(t), dir: t.TempDir()}
	f.limit.Store(maxBytes)
	f.clock.ns.Store(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC).UnixNano())
	cfg := Config{
		Dir:   f.dir,
		Store: f.st,
		Resolver: sandboxes{
			"127.0.0.1":  acme("sb-1", "refs/heads/main", ""),
			"sb-feature": acme("sb-feature", "refs/heads/feature", ""),
			"sb-develop": acme("sb-develop", "refs/heads/develop", ""),
			"sb-pr1":     acme("sb-pr1", "refs/pull/1/merge", "refs/heads/main"),
			"sb-pr2":     acme("sb-pr2", "refs/pull/2/merge", "refs/heads/main"),
			"sb-pr3":     acme("sb-pr3", "refs/pull/3/merge", "refs/heads/develop"),
			// A pull request from a fork: runplane does not know its ref.
			"sb-fork":    acme("sb-fork", "", ""),
			"sb-unknown": {SandboxID: "sb-unknown", Repository: "acme/app"},
			"sb-other":   {SandboxID: "sb-other", Repository: "other/lib", DefaultBranch: "trunk", Ref: "refs/heads/trunk"},
			"sb-idle":    {SandboxID: "sb-idle"},
			"10.213.0.5": {SandboxID: "sb-idle"},
		},
		Space: func() datadisk.Space {
			if f.disk != nil {
				return f.disk()
			}
			return datadisk.Space{Cache: f.limit.Load()}
		},
		Now: f.clock.now,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.jobWait = 100 * time.Millisecond
	f.srv = srv
	direct := httptest.NewServer(srv)
	proxied := httptest.NewServer(srv.Proxied())
	t.Cleanup(direct.Close)
	t.Cleanup(proxied.Close)
	f.url, f.proxied = direct.URL, proxied.URL
	return f
}

// counts returns the cache statistics of a repository.
func (f *fixture) counts(t *testing.T, repository string) store.CacheCounts {
	f.srv.stats.flush(t.Context())
	counts, err := f.st.CacheStatsByRepository(t.Context(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return counts[repository]
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func acme(id, ref, baseRef string) Identity {
	return Identity{SandboxID: id, Repository: "acme/app", DefaultBranch: "main", Ref: ref, BaseRef: baseRef}
}

// via returns a client of a sandbox whose requests come through the trusted
// proxy. A job can write any cache scopes into its runtime token, because
// runplane cannot check the signature. So each of these tokens claims main,
// and the branch rules must hold anyway.
func (f *fixture) via(t *testing.T, sandboxID string) client {
	return client{t: t, url: f.proxied, token: token(scope{"refs/heads/main", 3}), header: http.Header{SandboxHeader: {sandboxID}}}
}

type scope struct {
	ref  string
	perm int
}

// token makes an unsigned runtime token with cache scopes in the "ac" claim,
// like GitHub's. The server must not trust these scopes.
func token(scopes ...scope) string {
	var ac []string
	for _, s := range scopes {
		ac = append(ac, fmt.Sprintf(`{"Scope":%q,"Permission":%d}`, s.ref, s.perm))
	}
	now := time.Now().Unix()
	claims, _ := json.Marshal(map[string]any{
		"ac":  "[" + strings.Join(ac, ",") + "]",
		"nbf": now - 60,
		"exp": now + 3600,
	})
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(claims) + ".c2ln"
}

// client speaks the cache protocol like @actions/cache.
type client struct {
	t      *testing.T
	url    string
	token  string
	header http.Header
}

func (c client) do(req *http.Request) *http.Response {
	c.t.Helper()
	for k, v := range c.header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp
}

// call makes a Twirp call and returns the status and the decoded JSON body.
func (c client) call(method string, in map[string]any) (int, map[string]any) {
	c.t.Helper()
	body, _ := json.Marshal(in)
	req, _ := http.NewRequest(http.MethodPost, c.url+"/twirp/github.actions.results.api.v1.CacheService/"+method, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp := c.do(req)
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.t.Fatalf("%s: decode response: %v", method, err)
	}
	return resp.StatusCode, out
}

// reserve calls CreateCacheEntry and returns the upload URL.
func (c client) reserve(key, version string) string {
	c.t.Helper()
	status, out := c.call("CreateCacheEntry", map[string]any{"key": key, "version": version})
	if status != http.StatusOK || out["ok"] != true {
		c.t.Fatalf("CreateCacheEntry %q: %d %v", key, status, out)
	}
	return out["signed_upload_url"].(string)
}

func (c client) finalize(key, version string, size int) map[string]any {
	c.t.Helper()
	status, out := c.call("FinalizeCacheEntryUpload", map[string]any{
		"key": key, "version": version, "size_bytes": strconv.Itoa(size),
	})
	if status != http.StatusOK {
		c.t.Fatalf("FinalizeCacheEntryUpload %q: %d %v", key, status, out)
	}
	return out
}

// save stores data in one Put Blob request.
func (c client) save(key, version string, data []byte) {
	c.t.Helper()
	u := c.reserve(key, version)
	if status := c.put(u, data, http.Header{"X-Ms-Blob-Type": {"BlockBlob"}}); status != http.StatusCreated {
		c.t.Fatalf("Put Blob: %d", status)
	}
	if out := c.finalize(key, version, len(data)); out["ok"] != true {
		c.t.Fatalf("FinalizeCacheEntryUpload %q: %v", key, out)
	}
}

func (c client) put(u string, body []byte, h http.Header) int {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodPut, u, bytes.NewReader(body))
	for k, v := range h {
		req.Header[k] = v
	}
	resp := c.do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// lookup calls GetCacheEntryDownloadURL and returns the matched key and the
// download URL; both are empty on a miss.
func (c client) lookup(version, key string, restoreKeys ...string) (string, string) {
	c.t.Helper()
	in := map[string]any{"key": key, "version": version}
	if len(restoreKeys) > 0 {
		in["restore_keys"] = restoreKeys
	}
	status, out := c.call("GetCacheEntryDownloadURL", in)
	if status != http.StatusOK {
		c.t.Fatalf("GetCacheEntryDownloadURL %q: %d %v", key, status, out)
	}
	if out["ok"] != true {
		return "", ""
	}
	return out["matched_key"].(string), out["signed_download_url"].(string)
}

// restore looks up and downloads an entry. It returns "" and nil on a miss.
func (c client) restore(version, key string, restoreKeys ...string) (string, []byte) {
	c.t.Helper()
	matched, u := c.lookup(version, key, restoreKeys...)
	if matched == "" {
		return "", nil
	}
	status, data := c.get(u, nil)
	if status != http.StatusOK {
		c.t.Fatalf("download %q: %d", matched, status)
	}
	return matched, data
}

func (c client) get(u string, h http.Header) (int, []byte) {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	for k, v := range h {
		req.Header[k] = v
	}
	resp := c.do(req)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}
