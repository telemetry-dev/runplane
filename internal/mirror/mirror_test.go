package mirror

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	typeSchema2  = "application/vnd.docker.distribution.manifest.v2+json"
	typeList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	typeOCIIndex = "application/vnd.oci.image.index.v1+json"
)

func TestPullCachesManifestsAndBlobs(t *testing.T) {
	hub := newHub(t)
	img := hub.push("library/alpine", "latest", "a")
	m, _ := newMirror(t, hub, Config{})
	srv := httptest.NewServer(m)

	// The first round fetches from upstream, the second uses the cache. The
	// short name must reach upstream as library/alpine.
	for _, name := range []string{"alpine", "library/alpine"} {
		resp, body := get(t, srv.URL+"/v2/"+name+"/manifests/latest")
		wantResponse(t, resp, body, 200, typeSchema2, sha(img.manifest), img.manifest)
		resp, body = get(t, srv.URL+"/v2/"+name+"/blobs/"+sha(img.layer))
		wantResponse(t, resp, body, 200, "application/octet-stream", sha(img.layer), img.layer)
		settle(t, m)
	}
	srv.Close() // waits for the handlers, which count the bytes after the client has them

	if n := hub.count("GET", "/v2/library/alpine/manifests/"+sha(img.manifest)); n != 1 {
		t.Errorf("upstream got %d manifest GETs, want 1", n)
	}
	if n := hub.count("GET", "/v2/library/alpine/blobs/"+sha(img.layer)); n != 1 {
		t.Errorf("upstream got %d blob GETs, want 1", n)
	}
	if n := hub.denied.Load(); n != 1 {
		t.Errorf("upstream answered 401 %d times, want 1 (the token is not reused)", n)
	}
	size := int64(len(img.manifest) + len(img.layer))
	got := m.Stats()
	if want := (Stats{Hits: 2, Misses: 2, HitBytes: size, MissBytes: size, SizeBytes: got.SizeBytes}); got != want {
		t.Errorf("Stats() = %+v, want %+v", got, want)
	}
	if got.SizeBytes < size {
		t.Errorf("SizeBytes = %d, want at least the %d bytes of content", got.SizeBytes, size)
	}
}

func TestCorruptBlobIsNotCached(t *testing.T) {
	hub := newHub(t)
	img := hub.push("library/alpine", "latest", "a")
	bad := bytes.Clone(img.layer)
	bad[len(bad)-1] ^= 1
	hub.setBlob(sha(img.layer), bad)
	_, url := newMirror(t, hub, Config{})
	blobURL := url + "/v2/library/alpine/blobs/" + sha(img.layer)

	// Depending on when the digest check fails, the client gets an error
	// status or a short body. It must never get the complete bad blob.
	for range 2 {
		resp, err := http.Get(blobURL)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && err == nil {
			t.Fatalf("client got a complete body of %d bytes for a bad blob", len(body))
		}
	}
	if n := hub.count("GET", "/v2/library/alpine/blobs/"+sha(img.layer)); n != 2 {
		t.Fatalf("upstream got %d blob GETs, want 2 (the bad blob is cached)", n)
	}

	hub.setBlob(sha(img.layer), img.layer)
	for range 2 {
		resp, body := get(t, blobURL)
		wantResponse(t, resp, body, 200, "application/octet-stream", sha(img.layer), img.layer)
	}
	if n := hub.count("GET", "/v2/library/alpine/blobs/"+sha(img.layer)); n != 3 {
		t.Errorf("upstream got %d blob GETs, want 3 (the good blob is not cached)", n)
	}
}

func TestCorruptManifestIsNotCached(t *testing.T) {
	hub := newHub(t)
	img := hub.push("library/alpine", "latest", "a")
	digest := sha(img.manifest)
	hub.setManifest(digest, typeSchema2, []byte(`{"schemaVersion":2}`))
	_, url := newMirror(t, hub, Config{})

	for range 2 {
		resp, body := get(t, url+"/v2/library/alpine/manifests/latest")
		if resp.StatusCode != 502 {
			t.Fatalf("status = %d, body %q; want 502", resp.StatusCode, body)
		}
	}
	if n := hub.count("GET", "/v2/library/alpine/manifests/"+digest); n != 2 {
		t.Errorf("upstream got %d manifest GETs, want 2 (the bad manifest is cached)", n)
	}
}

func TestHead(t *testing.T) {
	hub := newHub(t)
	img := hub.push("library/alpine", "latest", "a")
	m, url := newMirror(t, hub, Config{})
	blobPath := "/v2/library/alpine/blobs/" + sha(img.layer)

	resp, body := do(t, "HEAD", url+"/v2/library/alpine/manifests/latest", nil)
	wantResponse(t, resp, body, 200, typeSchema2, sha(img.manifest), nil)
	wantLength(t, resp, len(img.manifest))

	resp, body = do(t, "HEAD", url+blobPath, nil)
	wantResponse(t, resp, body, 200, "application/octet-stream", sha(img.layer), nil)
	wantLength(t, resp, len(img.layer))
	if n := hub.count("GET", blobPath); n != 0 {
		t.Errorf("HEAD of a blob made %d upstream GETs, want 0", n)
	}

	get(t, url+blobPath)
	settle(t, m)
	resp, body = do(t, "HEAD", url+blobPath, nil)
	wantResponse(t, resp, body, 200, "application/octet-stream", sha(img.layer), nil)
	wantLength(t, resp, len(img.layer))
	if n := hub.count("HEAD", blobPath); n != 1 {
		t.Errorf("upstream got %d blob HEADs, want 1 (HEAD of a cached blob goes upstream)", n)
	}

	resp, _ = do(t, "HEAD", url+"/v2/library/alpine/blobs/sha256:"+strings.Repeat("0", 64), nil)
	if resp.StatusCode != 404 {
		t.Errorf("HEAD of an unknown blob: status = %d, want 404", resp.StatusCode)
	}
}

func TestManifestIndexKeepsMediaType(t *testing.T) {
	for _, mediaType := range []string{typeList, typeOCIIndex} {
		t.Run(mediaType, func(t *testing.T) {
			hub := newHub(t)
			child := hub.push("library/alpine", "child", "a")
			index := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[`+
				`{"mediaType":%q,"size":%d,"digest":%q,"platform":{"architecture":"arm64","os":"linux"}}]}`,
				mediaType, typeSchema2, len(child.manifest), sha(child.manifest)))
			hub.setManifest(sha(index), mediaType, index)
			hub.tag("library/alpine", "latest", sha(index))
			_, url := newMirror(t, hub, Config{})

			for range 2 { // from upstream, then from the cache
				resp, body := get(t, url+"/v2/library/alpine/manifests/latest")
				wantResponse(t, resp, body, 200, mediaType, sha(index), index)
			}
		})
	}
}

func TestTagTTL(t *testing.T) {
	const tagPath = "/v2/library/alpine/manifests/latest"

	t.Run("fresh tag stays on the cached digest", func(t *testing.T) {
		hub := newHub(t)
		old := hub.push("library/alpine", "latest", "a")
		_, url := newMirror(t, hub, Config{})

		get(t, url+tagPath)
		hub.push("library/alpine", "latest", "b")
		resp, body := get(t, url+tagPath)
		wantResponse(t, resp, body, 200, typeSchema2, sha(old.manifest), old.manifest)
		if n := hub.count("HEAD", tagPath); n != 1 {
			t.Errorf("upstream got %d tag checks, want 1", n)
		}
	})

	t.Run("expired tag follows upstream", func(t *testing.T) {
		hub := newHub(t)
		hub.push("library/alpine", "latest", "a")
		m, url := newMirror(t, hub, Config{})

		get(t, url+tagPath)
		img := hub.push("library/alpine", "latest", "b")
		expireTags(m)
		for range 2 {
			resp, body := get(t, url+tagPath)
			wantResponse(t, resp, body, 200, typeSchema2, sha(img.manifest), img.manifest)
			expireTags(m)
		}
		// The check of an unchanged tag uses HEAD only, which does not count
		// against the Docker Hub pull limit.
		if n := hub.count("GET", "/v2/library/alpine/manifests/"+sha(img.manifest)); n != 1 {
			t.Errorf("upstream got %d manifest GETs, want 1", n)
		}
	})

	t.Run("expired tag works while upstream is down", func(t *testing.T) {
		hub := newHub(t)
		img := hub.push("library/alpine", "latest", "a")
		m, url := newMirror(t, hub, Config{})

		get(t, url+tagPath)
		hub.setDown(true)
		expireTags(m)
		resp, body := get(t, url+tagPath)
		wantResponse(t, resp, body, 200, typeSchema2, sha(img.manifest), img.manifest)
	})
}

func TestConcurrentBlobFetchesShareOneDownload(t *testing.T) {
	hub := newHub(t)
	img := hub.push("library/alpine", "latest", "a")
	hub.gate = make(chan struct{})
	_, url := newMirror(t, hub, Config{})
	blobPath := "/v2/library/alpine/blobs/" + sha(img.layer)

	// The upstream response stops after half of the blob until the gate
	// opens. Each client gets its response headers before that, so all
	// clients ask while the first download runs.
	const clients = 5
	resps := make(chan *http.Response, clients)
	for range clients {
		go func() {
			resp, err := http.Get(url + blobPath)
			if err != nil {
				t.Error(err)
			}
			resps <- resp
		}()
	}
	var all []*http.Response
	for range clients {
		if resp := <-resps; resp != nil {
			all = append(all, resp)
		}
	}
	close(hub.gate)
	for _, resp := range all {
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || !bytes.Equal(body, img.layer) {
			t.Errorf("client got %d bytes and error %v, want the blob", len(body), err)
		}
	}
	if n := hub.count("GET", blobPath); n != 1 {
		t.Errorf("upstream got %d blob GETs, want 1", n)
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	hub := newHub(t)
	a, b, c := hub.addBlob("a", 100), hub.addBlob("b", 100), hub.addBlob("c", 100)
	m, url := newMirror(t, hub, Config{MaxBytes: 250})
	blobURL := url + "/v2/library/alpine/blobs/"

	for _, digest := range []string{a, b, a, c, a} {
		get(t, blobURL+digest)
		settle(t, m)
	}
	// c needed room, and b was the least recently used blob.
	for digest, want := range map[string]int{a: 1, b: 1, c: 1} {
		if n := hub.count("GET", "/v2/library/alpine/blobs/"+digest); n != want {
			t.Errorf("upstream got %d GETs of %s, want %d", n, digest[:15], want)
		}
	}
	get(t, blobURL+b)
	if n := hub.count("GET", "/v2/library/alpine/blobs/"+b); n != 2 {
		t.Errorf("upstream got %d GETs of the evicted blob, want 2", n)
	}
	if size := m.Stats().SizeBytes; size > 250 {
		t.Errorf("cache size = %d bytes, want at most 250", size)
	}
}

func TestCacheSurvivesRestart(t *testing.T) {
	hub := newHub(t)
	img := hub.push("library/alpine", "latest", "a")
	dir := t.TempDir()
	first, url := newMirror(t, hub, Config{Dir: dir})
	get(t, url+"/v2/library/alpine/manifests/latest")
	get(t, url+"/v2/library/alpine/blobs/"+sha(img.layer))
	first.Close()

	hub.setDown(true)
	_, url = newMirror(t, hub, Config{Dir: dir})
	resp, body := get(t, url+"/v2/library/alpine/manifests/latest")
	wantResponse(t, resp, body, 200, typeSchema2, sha(img.manifest), img.manifest)
	resp, body = get(t, url+"/v2/library/alpine/blobs/"+sha(img.layer))
	wantResponse(t, resp, body, 200, "application/octet-stream", sha(img.layer), img.layer)
}

func TestRangeOfBlob(t *testing.T) {
	hub := newHub(t)
	img := hub.push("library/alpine", "latest", "a")
	m, url := newMirror(t, hub, Config{})
	blobPath := "/v2/library/alpine/blobs/" + sha(img.layer)

	for _, when := range []string{"during the download", "from the cache"} {
		resp, body := do(t, "GET", url+blobPath, http.Header{"Range": {"bytes=10-19"}})
		if resp.StatusCode != 206 || !bytes.Equal(body, img.layer[10:20]) {
			t.Errorf("%s: status %d, body %q; want 206 and %q", when, resp.StatusCode, body, img.layer[10:20])
		}
		settle(t, m)
	}
	if n := hub.count("GET", blobPath); n != 1 {
		t.Errorf("upstream got %d blob GETs, want 1", n)
	}
}

func TestAllow(t *testing.T) {
	tests := []struct {
		name       string
		sandbox    string
		wantStatus int
	}{
		{"sandbox address", "127.0.0.1", 200},
		{"other address", "10.0.0.5", 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newHub(t)
			img := hub.push("library/alpine", "latest", "a")
			sandbox := netip.MustParseAddr(tt.sandbox)
			_, url := newMirror(t, hub, Config{Allow: func(ip netip.Addr) bool { return ip == sandbox }})

			for _, path := range []string{"/v2/", "/v2/alpine/manifests/latest", "/v2/alpine/blobs/" + sha(img.layer)} {
				if resp, _ := get(t, url+path); resp.StatusCode != tt.wantStatus {
					t.Errorf("GET %s: status = %d, want %d", path, resp.StatusCode, tt.wantStatus)
				}
			}
		})
	}
}

func TestParsePath(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	tests := []struct {
		path            string
		name, kind, ref string
		wantErrStatus   int
	}{
		{"/alpine/manifests/latest", "library/alpine", "manifests", "latest", 0},
		{"/grafana/grafana/manifests/11.2.0", "grafana/grafana", "manifests", "11.2.0", 0},
		{"/library/alpine/blobs/" + digest, "library/alpine", "blobs", digest, 0},
		{"/alpine/manifests/" + digest, "library/alpine", "manifests", digest, 0},
		{"/a/manifests/b/manifests/v1", "a/manifests/b", "manifests", "v1", 0},
		{"/alpine/tags/list", "", "", "", 404},
		{"/manifests/latest", "", "", "", 404},
		{"/Alpine/manifests/latest", "", "", "", 400},
		{"/alpine/blobs/latest", "", "", "", 400},
		{"/alpine/manifests/sha256:abc", "", "", "", 400},
		{"/alpine/manifests/sha512:" + strings.Repeat("ab", 64), "", "", "", 400},
	}
	for _, tt := range tests {
		name, kind, ref, err := parsePath(tt.path)
		status := 0
		if err != nil {
			status = err.(*apiError).status
		}
		if name != tt.name || kind != tt.kind || ref != tt.ref || status != tt.wantErrStatus {
			t.Errorf("parsePath(%q) = %q, %q, %q, status %d; want %q, %q, %q, status %d",
				tt.path, name, kind, ref, status, tt.name, tt.kind, tt.ref, tt.wantErrStatus)
		}
	}
}

// fakeHub is a registry with the token flow of Docker Hub: a request without
// a pull token for its repository gets 401 and a Bearer challenge.
type fakeHub struct {
	srv    *httptest.Server
	gate   chan struct{} // if set, a blob response waits for it after half of the body
	denied atomic.Int32  // registry requests without a token

	mu        sync.Mutex
	tags      map[string]string       // "name:tag" to digest
	manifests map[string]fakeManifest // by digest
	blobs     map[string][]byte       // by digest; the content can be wrong
	requests  []string                // "METHOD path" of the served registry requests
	down      bool
}

type fakeManifest struct {
	mediaType string
	body      []byte
}

type testImage struct {
	manifest, config, layer []byte
}

func newHub(t *testing.T) *fakeHub {
	h := &fakeHub{
		tags:      map[string]string{},
		manifests: map[string]fakeManifest{},
		blobs:     map[string][]byte{},
	}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

// push adds an image with one layer and points name:tag to it. seed makes
// the content different for each image.
func (h *fakeHub) push(name, tag, seed string) testImage {
	img := testImage{
		config: []byte(`{"architecture":"arm64","os":"linux","config":{"Cmd":["/bin/sh"]},"rootfs":{"type":"layers","diff_ids":["sha256:` + seed + `"]}}`),
		layer:  bytes.Repeat([]byte("layer "+seed+" "), 20000),
	}
	img.manifest = []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,`+
		`"config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":%d,"digest":%q},`+
		`"layers":[{"mediaType":"application/vnd.docker.image.rootfs.diff.tar.gzip","size":%d,"digest":%q}]}`,
		typeSchema2, len(img.config), sha(img.config), len(img.layer), sha(img.layer)))
	h.setBlob(sha(img.config), img.config)
	h.setBlob(sha(img.layer), img.layer)
	h.setManifest(sha(img.manifest), typeSchema2, img.manifest)
	h.tag(name, tag, sha(img.manifest))
	return img
}

func (h *fakeHub) addBlob(seed string, size int) string {
	b := bytes.Repeat([]byte(seed), size)
	h.setBlob(sha(b), b)
	return sha(b)
}

func (h *fakeHub) setBlob(digest string, content []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.blobs[digest] = content
}

func (h *fakeHub) setManifest(digest, mediaType string, body []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.manifests[digest] = fakeManifest{mediaType, body}
}

func (h *fakeHub) tag(name, tag, digest string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tags[name+":"+tag] = digest
}

func (h *fakeHub) setDown(down bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.down = down
}

func (h *fakeHub) count(method, path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.requests {
		if r == method+" "+path {
			n++
		}
	}
	return n
}

func (h *fakeHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		h.serveToken(w, r)
		return
	}
	h.mu.Lock()
	down := h.down
	h.mu.Unlock()
	if down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	i := max(strings.LastIndex(path, "/manifests/"), strings.LastIndex(path, "/blobs/"))
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	name := path[:i]
	kind, ref, _ := strings.Cut(path[i+1:], "/")
	scope := "repository:" + name + ":pull"
	if r.Header.Get("Authorization") != "Bearer token-for-"+scope {
		h.denied.Add(1)
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="%s/token",service="registry.docker.io",scope="%s"`, h.srv.URL, scope))
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	h.mu.Lock()
	h.requests = append(h.requests, r.Method+" "+r.URL.Path)
	digest := ref
	if d, ok := h.tags[name+":"+ref]; ok {
		digest = d
	}
	mf, haveManifest := h.manifests[digest]
	blob, haveBlob := h.blobs[digest]
	h.mu.Unlock()

	switch {
	case kind == "manifests" && haveManifest:
		w.Header().Set("Content-Type", mf.mediaType)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", strconv.Itoa(len(mf.body)))
		w.Write(mf.body)
	case kind == "blobs" && haveBlob:
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
		if r.Method == http.MethodHead {
			return
		}
		if h.gate == nil {
			w.Write(blob)
			return
		}
		w.Write(blob[:len(blob)/2])
		w.(http.Flusher).Flush()
		<-h.gate
		w.Write(blob[len(blob)/2:])
	default:
		writeError(w, http.StatusNotFound, strings.ToUpper(strings.TrimSuffix(kind, "s"))+"_UNKNOWN", "unknown")
	}
}

func (h *fakeHub) serveToken(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("service") != "registry.docker.io" {
		http.Error(w, "unknown service", http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"token":      "token-for-" + r.URL.Query().Get("scope"),
		"expires_in": 300,
		"issued_at":  time.Now().UTC().Format(time.RFC3339),
	})
}

func newMirror(t *testing.T, hub *fakeHub, cfg Config) (*Mirror, string) {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	if cfg.Allow == nil {
		cfg.Allow = func(netip.Addr) bool { return true }
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m.up.base = hub.srv.URL
	t.Cleanup(func() { m.Close() })
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return m, srv.URL
}

// expireTags makes the tags of m due for a check with upstream.
func expireTags(m *Mirror) {
	m.tags.mu.Lock()
	defer m.tags.mu.Unlock()
	for k, te := range m.tags.tags {
		te.Checked = time.Time{}
		m.tags.tags[k] = te
	}
}

// settle waits until m has moved the blobs of finished downloads into the
// cache. Clients get the last byte of a blob before that.
func settle(t *testing.T, m *Mirror) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		n := len(m.downloads)
		m.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d downloads did not finish", n)
		}
		time.Sleep(time.Millisecond)
	}
}

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	return do(t, "GET", url, nil)
}

func do(t *testing.T, method, url string, header http.Header) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, url, err)
	}
	return resp, body
}

func wantResponse(t *testing.T, resp *http.Response, body []byte, status int, mediaType, digest string, content []byte) {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("%s %s: status = %d, body %q; want %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, body, status)
	}
	if got := resp.Header.Get("Content-Type"); got != mediaType {
		t.Errorf("%s: Content-Type = %q, want %q", resp.Request.URL.Path, got, mediaType)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != digest {
		t.Errorf("%s: Docker-Content-Digest = %q, want %q", resp.Request.URL.Path, got, digest)
	}
	if !bytes.Equal(body, content) {
		t.Errorf("%s: body has %d bytes that differ from the %d bytes upstream", resp.Request.URL.Path, len(body), len(content))
	}
}

func wantLength(t *testing.T, resp *http.Response, n int) {
	t.Helper()
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(n) {
		t.Errorf("%s: Content-Length = %q, want %d", resp.Request.URL.Path, got, n)
	}
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
