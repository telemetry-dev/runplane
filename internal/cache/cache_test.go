package cache

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/store"
)

const version = "c0ffee"

func TestBlockUploadAndRestore(t *testing.T) {
	f := newFixture(t, 0)
	c := client{t: t, url: f.url}

	u := c.reserve("node-deps-abc", version)
	// Blocks arrive in any order; the block list sets the order of the blob.
	parts := []string{"first-", "second-", "third"}
	var list strings.Builder
	list.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	for i := range parts {
		list.WriteString("<Latest>" + blockID(i) + "</Latest>")
	}
	list.WriteString("</BlockList>")
	for i := len(parts) - 1; i >= 0; i-- {
		if status := c.put(u+"&comp=block&blockid="+url.QueryEscape(blockID(i)), []byte(parts[i]), nil); status != http.StatusCreated {
			t.Fatalf("Put Block %d: %d", i, status)
		}
	}
	if status := c.put(u+"&comp=blocklist", []byte(list.String()), nil); status != http.StatusCreated {
		t.Fatalf("Put Block List: %d", status)
	}
	out := c.finalize("node-deps-abc", version, len("first-second-third"))
	if out["ok"] != true {
		t.Fatalf("finalize: %v", out)
	}
	// BuildKit reads entry_id into a Go string; a JSON number fails there.
	if _, ok := out["entry_id"].(string); !ok {
		t.Errorf("entry_id = %#v, want a JSON string", out["entry_id"])
	}

	matched, data := c.restore(version, "node-deps-abc")
	if matched != "node-deps-abc" || string(data) != "first-second-third" {
		t.Errorf("exact restore = %q %q", matched, data)
	}
	matched, data = c.restore(version, "node-deps-xyz", "node-deps-")
	if matched != "node-deps-abc" || string(data) != "first-second-third" {
		t.Errorf("restore by restore key = %q %q", matched, data)
	}
	if matched, _ := c.lookup("other-version", "node-deps-abc"); matched != "" {
		t.Errorf("restore with a different version matched %q", matched)
	}
	if got := f.counts(t, "acme/app"); got != (store.CacheCounts{Hits: 2, Misses: 1, BytesRead: 36, BytesWritten: 18}) {
		t.Errorf("stats = %+v", got)
	}
	if staged, _ := os.ReadDir(filepath.Join(f.dir, "uploads")); len(staged) != 0 {
		t.Errorf("staged blocks left after commit: %v", staged)
	}
}

// blockID makes a block ID like the Azure SDK for JavaScript: base64 of a
// UUID and an index.
func blockID(i int) string {
	return base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "3f2a9c1e-5b7d-4e8f-9a0b-1c2d3e4f5a6b%012d", i))
}

func TestRestoreOrder(t *testing.T) {
	f := newFixture(t, 0)
	c := client{t: t, url: f.url}
	c.save("deps-linux", version, []byte("one"))
	f.clock.advance(time.Minute)
	c.save("deps-linux-2", version, []byte("two"))
	f.clock.advance(time.Minute)
	c.save("other-3", version, []byte("three"))

	tests := []struct {
		name        string
		key         string
		restoreKeys []string
		want        string
	}{
		{"exact key beats a newer prefix match", "deps-linux", nil, "deps-linux"},
		{"newest prefix match of the key", "deps-lin", nil, "deps-linux-2"},
		{"restore keys in order", "missing", []string{"nothing-", "other-", "deps-"}, "other-3"},
		{"no match", "missing", []string{"nothing-"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if matched, _ := c.lookup(version, tt.key, tt.restoreKeys...); matched != tt.want {
				t.Errorf("matched %q, want %q", matched, tt.want)
			}
		})
	}
}

func TestRangedDownload(t *testing.T) {
	f := newFixture(t, 0)
	c := client{t: t, url: f.url}
	c.save("k", version, []byte("0123456789abcdefghij"))
	_, u := c.lookup(version, "k")

	for _, header := range []string{"Range", "x-ms-range"} {
		status, data := c.get(u, http.Header{header: {"bytes=10-14"}})
		if status != http.StatusPartialContent || string(data) != "abcde" {
			t.Errorf("%s: %d %q, want 206 \"abcde\"", header, status, data)
		}
	}
	// BuildKit reads from an offset to the end.
	if status, data := c.get(u, http.Header{"x-ms-range": {"bytes=15-"}}); status != http.StatusPartialContent || string(data) != "fghij" {
		t.Errorf("open range: %d %q", status, data)
	}
}

func TestBranchRules(t *testing.T) {
	f := newFixture(t, 0)
	main := client{t: t, url: f.url}
	pr1, pr2, pr3 := f.via(t, "sb-pr1"), f.via(t, "sb-pr2"), f.via(t, "sb-pr3")
	feature, develop, fork := f.via(t, "sb-feature"), f.via(t, "sb-develop"), f.via(t, "sb-fork")

	main.save("k", version, []byte("from main"))
	pr1.save("k", version, []byte("from pr 1"))
	pr1.save("pr1-only", version, []byte("pr 1 data"))
	feature.save("feature-only", version, []byte("feature data"))
	develop.save("develop-only", version, []byte("develop data"))

	tests := []struct {
		name string
		c    client
		key  string
		want string
	}{
		{"a pull request reads its own ref first", pr1, "k", "from pr 1"},
		{"a pull request reads the default branch", pr2, "k", "from main"},
		{"a pull request reads its base branch", pr3, "develop-only", "develop data"},
		{"a pull request cannot read a different pull request", pr2, "pr1-only", ""},
		{"the default branch cannot read a pull request", main, "pr1-only", ""},
		{"the default branch cannot read a feature branch", main, "feature-only", ""},
		{"a feature branch reads the default branch", feature, "k", "from main"},
		{"a job without a known ref reads the default branch", fork, "k", "from main"},
		{"a job without a known ref reads no other branch", fork, "feature-only", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, data := tt.c.restore(version, tt.key)
			if string(data) != tt.want {
				t.Errorf("restored %q, want %q", data, tt.want)
			}
		})
	}

	t.Run("a job without a known ref cannot save", func(t *testing.T) {
		status, out := fork.call("CreateCacheEntry", map[string]any{"key": "new", "version": version})
		msg, _ := out["message"].(string)
		if status != http.StatusOK || out["ok"] != false || !strings.HasPrefix(msg, "cache write denied:") {
			t.Errorf("got %d %v, want ok:false with a cache write denied message", status, out)
		}
	})
	t.Run("a job without a ref or a default branch gets a read denial", func(t *testing.T) {
		status, out := f.via(t, "sb-unknown").call("GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": version})
		msg, _ := out["msg"].(string)
		if status != http.StatusForbidden || !strings.HasPrefix(msg, "cache read denied:") {
			t.Errorf("got %d %v, want 403 with a cache read denied message", status, out)
		}
	})
}

func TestRepositoryIsolation(t *testing.T) {
	f := newFixture(t, 0)
	acme := client{t: t, url: f.url}
	acme.save("k", version, []byte("acme data"))
	_, acmeURL := acme.lookup(version, "k")

	other := f.via(t, "sb-other")
	if matched, _ := other.lookup(version, "k"); matched != "" {
		t.Errorf("other/lib restored the acme/app entry %q", matched)
	}
	// A signed URL of acme/app is useless in a sandbox of other/lib.
	if status, _ := other.get(strings.Replace(acmeURL, f.url, f.proxied, 1), nil); status != http.StatusForbidden {
		t.Errorf("download from another repository: %d, want 403", status)
	}

	t.Run("only a trusted proxy can name the sandbox", func(t *testing.T) {
		spoof := client{t: t, url: f.url, header: http.Header{SandboxHeader: {"sb-other"}}}
		spoof.save("spoofed", version, []byte("x"))
		e, err := f.st.GetCacheEntryByKey(context.Background(), "acme/app", "refs/heads/main", "spoofed", version)
		if err != nil {
			t.Fatalf("entry not saved for the sandbox of the source IP: %v", err)
		}
		if e.Repository != "acme/app" {
			t.Errorf("repository = %q", e.Repository)
		}
	})
}

func TestRequestsWithoutIdentity(t *testing.T) {
	f := newFixture(t, 0)
	for _, tt := range []struct {
		name   string
		c      client
		status int
	}{
		{"unknown sandbox", f.via(t, "sb-gone"), http.StatusForbidden},
		{"sandbox without a job", f.via(t, "sb-idle"), http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, method := range []string{"CreateCacheEntry", "FinalizeCacheEntryUpload", "GetCacheEntryDownloadURL"} {
				status, _ := tt.c.call(method, map[string]any{"key": "k", "version": version})
				if status != tt.status {
					t.Errorf("%s: %d, want %d", method, status, tt.status)
				}
			}
		})
	}
	if entries, _ := f.st.ListCacheEntries(context.Background(), "acme/app"); len(entries) != 0 {
		t.Errorf("entries made without identity: %v", entries)
	}
}

func TestSignedURLs(t *testing.T) {
	f := newFixture(t, 0)
	c := client{t: t, url: f.url}
	c.save("k", version, []byte("data"))
	_, download := c.lookup(version, "k")
	upload := c.reserve("k2", version)

	tests := []struct {
		name   string
		do     func() int
		status int
	}{
		{"valid download", func() int { s, _ := c.get(download, nil); return s }, http.StatusOK},
		{"changed signature", func() int {
			s, _ := c.get(strings.Replace(download, "sig=", "sig=x", 1), nil)
			return s
		}, http.StatusForbidden},
		{"download URL used to upload", func() int { return c.put(download, []byte("evil"), nil) }, http.StatusForbidden},
		{"upload URL used to download", func() int { s, _ := c.get(upload, nil); return s }, http.StatusForbidden},
		{"other entry ID with the same signature", func() int {
			e, _ := f.st.GetCacheEntryByKey(context.Background(), "acme/app", "refs/heads/main", "k2", version)
			s, _ := c.get(strings.Replace(download, blobPrefix, blobPrefix+e.ID+"?x=", 1), nil)
			return s
		}, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.do(); got != tt.status {
				t.Errorf("status %d, want %d", got, tt.status)
			}
		})
	}
	t.Run("expired", func(t *testing.T) {
		f.clock.advance(urlTTL + time.Second)
		if status, _ := c.get(download, nil); status != http.StatusForbidden {
			t.Errorf("status %d, want 403", status)
		}
	})
}

func TestReservations(t *testing.T) {
	f := newFixture(t, 0)
	c := client{t: t, url: f.url}
	create := func(key string) int {
		status, _ := c.call("CreateCacheEntry", map[string]any{"key": key, "version": version})
		return status
	}

	first := c.reserve("k", version)
	f.clock.advance(activeUpload - time.Minute)
	c.put(first+"&comp=block&blockid="+url.QueryEscape(blockID(0)), []byte("partial"), nil)
	f.clock.advance(activeUpload - time.Minute)
	if status := create("k"); status != http.StatusConflict {
		t.Errorf("reserve during an active upload: %d, want 409", status)
	}
	// An upload with no activity for some time belongs to a dead job.
	f.clock.advance(activeUpload + time.Second)
	c.reserve("k", version)
	if status := c.put(first, []byte("late"), nil); status != http.StatusNotFound {
		t.Errorf("upload to a replaced reservation: %d, want 404", status)
	}

	c.save("done", version, []byte("x"))
	if status := create("done"); status != http.StatusConflict {
		t.Errorf("reserve a finalized entry: %d, want 409", status)
	}
}

func TestFinalizeChecks(t *testing.T) {
	f := newFixture(t, 0)
	c := client{t: t, url: f.url}

	c.reserve("empty", version)
	if out := c.finalize("empty", version, 10); out["ok"] != false {
		t.Errorf("finalize without data: %v, want ok:false", out)
	}

	u := c.reserve("short", version)
	c.put(u, []byte("12345"), nil)
	if out := c.finalize("short", version, 10); out["ok"] != false {
		t.Errorf("finalize with missing bytes: %v, want ok:false", out)
	}
	// The failed upload is gone, so the job can save again at once.
	u = c.reserve("short", version)
	c.put(u, []byte("1234567890"), nil)
	if out := c.finalize("short", version, 10); out["ok"] != true {
		t.Errorf("second finalize: %v", out)
	}

	status, out := c.call("FinalizeCacheEntryUpload", map[string]any{"key": "never", "version": version, "size_bytes": 3})
	if status != http.StatusNotFound {
		t.Errorf("finalize without reservation: %d %v, want 404", status, out)
	}
}

// TestBuildKitRequestShape covers what TestBuildKitClient finds when it
// runs, for runs without network access.
func TestBuildKitRequestShape(t *testing.T) {
	f := newFixture(t, 0)
	c := client{t: t, url: f.url}
	req, _ := http.NewRequest(http.MethodPut, c.reserve("buildkit-index", version), strings.NewReader("layer"))
	resp := c.do(req)
	resp.Body.Close()
	if resp.Header.Get("x-ms-request-id") == "" {
		t.Error("Put Blob response has no x-ms-request-id; BuildKit dereferences it")
	}
	// BuildKit sends size_bytes as a JSON number and repeats the key in restore_keys.
	status, out := c.call("FinalizeCacheEntryUpload", map[string]any{"key": "buildkit-index", "version": version, "size_bytes": 5})
	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("finalize: %d %v", status, out)
	}
	status, out = c.call("GetCacheEntryDownloadURL", map[string]any{
		"key": "buildkit-index", "restore_keys": []string{"buildkit-index"}, "version": version,
	})
	if status != http.StatusOK || out["matched_key"] != "buildkit-index" {
		t.Errorf("lookup: %d %v", status, out)
	}
}

func TestEviction(t *testing.T) {
	f := newFixture(t, 250)
	c := client{t: t, url: f.url}
	data := bytes.Repeat([]byte("x"), 100)

	c.save("a", version, data)
	f.clock.advance(time.Minute)
	c.save("b", version, data)
	f.clock.advance(time.Minute)
	c.restore(version, "a") // a is now used more recently than b
	f.clock.advance(time.Minute)
	c.save("c", version, data)

	for key, want := range map[string]bool{"a": true, "b": false, "c": true} {
		if matched, _ := c.lookup(version, key); (matched != "") != want {
			t.Errorf("entry %q kept = %v, want %v", key, matched != "", want)
		}
	}
	if blobs, _ := os.ReadDir(filepath.Join(f.dir, "blobs")); len(blobs) != 2 {
		t.Errorf("%d blobs on disk, want 2", len(blobs))
	}
}

func TestUploadOverLimit(t *testing.T) {
	f := newFixture(t, 100)
	c := client{t: t, url: f.url}
	u := c.reserve("big", version)
	if status := c.put(u+"&comp=block&blockid="+url.QueryEscape(blockID(0)), bytes.Repeat([]byte("x"), 60), nil); status != http.StatusCreated {
		t.Fatalf("first block: %d", status)
	}
	if status := c.put(u+"&comp=block&blockid="+url.QueryEscape(blockID(1)), bytes.Repeat([]byte("x"), 60), nil); status != http.StatusRequestEntityTooLarge {
		t.Errorf("block past the limit: %d, want 413", status)
	}
	if status := c.put(u, bytes.Repeat([]byte("x"), 101), nil); status != http.StatusRequestEntityTooLarge {
		t.Errorf("blob past the limit: %d, want 413", status)
	}

	t.Run("limit lowered during the upload", func(t *testing.T) {
		c.save("small", version, []byte("12345"))
		u := c.reserve("late", version)
		c.put(u, bytes.Repeat([]byte("x"), 80), nil)
		f.limit.Store(50)
		if out := c.finalize("late", version, 80); out["ok"] != false {
			t.Errorf("finalize past the new limit: %v, want ok:false", out)
		}
		if matched, _ := c.lookup(version, "small"); matched != "small" {
			t.Error("the oversized entry evicted the other entries")
		}
	})
}

func TestCleanup(t *testing.T) {
	f := newFixture(t, 0)
	c := client{t: t, url: f.url}
	ctx := context.Background()

	c.save("kept", version, []byte("x"))
	u := c.reserve("abandoned", version)
	c.put(u+"&comp=block&blockid="+url.QueryEscape(blockID(0)), []byte("partial"), nil)
	orphan := filepath.Join(f.dir, "blobs", "01jorphan0000000000000000")
	os.WriteFile(orphan, []byte("x"), 0o600)
	// A block that arrived while its upload was removed.
	os.MkdirAll(filepath.Join(f.dir, "uploads", "01jorphan0000000000000001"), 0o700)
	os.WriteFile(filepath.Join(f.dir, "uploads", "01jorphan0000000000000001", "block"), []byte("x"), 0o600)

	f.clock.advance(staleUpload - time.Minute)
	f.srv.cleanup(ctx)
	if _, err := f.st.GetCacheEntryByKey(ctx, "acme/app", "refs/heads/main", "abandoned", version); err != nil {
		t.Fatalf("upload removed before one hour: %v", err)
	}
	if staged, _ := os.ReadDir(filepath.Join(f.dir, "uploads")); len(staged) != 1 {
		t.Errorf("staging folders = %v, want only the active upload", staged)
	}

	f.clock.advance(2 * time.Minute)
	f.srv.removeOrphans(ctx, "blobs")
	f.srv.cleanup(ctx)
	if _, err := f.st.GetCacheEntryByKey(ctx, "acme/app", "refs/heads/main", "abandoned", version); !errors.Is(err, ErrNotFound) {
		t.Errorf("abandoned upload still in the index: %v", err)
	}
	if staged, _ := os.ReadDir(filepath.Join(f.dir, "uploads")); len(staged) != 0 {
		t.Errorf("staged blocks left: %v", staged)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan blob still on disk: %v", err)
	}
	if matched, _ := c.lookup(version, "kept"); matched != "kept" {
		t.Error("cleanup removed a finalized entry")
	}
}

func TestDamagedBlobIsAMiss(t *testing.T) {
	f := newFixture(t, 0)
	c := client{t: t, url: f.url}
	c.save("deps-old", version, []byte("old"))
	f.clock.advance(time.Minute)
	c.save("deps-new", version, []byte("new"))
	e, _ := f.st.GetCacheEntryByKey(context.Background(), "acme/app", "refs/heads/main", "deps-new", version)
	os.Truncate(filepath.Join(f.dir, e.Path), 1)

	// A download URL is a promise: BuildKit fails the build on a 404.
	if matched, data := c.restore(version, "deps-"); matched != "deps-old" || string(data) != "old" {
		t.Errorf("restored %q %q, want the older intact entry", matched, data)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t, 0)
	c := client{t: t, url: f.url}
	c.save("a", version, []byte("1"))
	c.save("b", version, []byte("2"))
	ctx := context.Background()

	n, err := f.srv.DeleteRepository(ctx, "acme/app")
	if err != nil || n != 2 {
		t.Fatalf("DeleteRepository = %d, %v", n, err)
	}
	if matched, _ := c.lookup(version, "a"); matched != "" {
		t.Error("entry restorable after delete")
	}
	if blobs, _ := os.ReadDir(filepath.Join(f.dir, "blobs")); len(blobs) != 0 {
		t.Errorf("blobs left after delete: %v", blobs)
	}
}

func TestResultsProxy(t *testing.T) {
	type call struct{ path, body, auth, sandbox string }
	calls := make(chan call, 10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls <- call{r.URL.Path, string(b), r.Header.Get("Authorization"), r.Header.Get(SandboxHeader)}
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	f := newFixture(t, 0)
	u, _ := url.Parse(upstream.URL)
	f.srv.proxy = newProxy(u, f.srv.cfg.Logger)

	const path = "/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact"
	req, _ := http.NewRequest(http.MethodPost, f.proxied+path, strings.NewReader(`{"name":"dist"}`))
	req.Header.Set("Authorization", "Bearer job-token")
	req.Header.Set(SandboxHeader, "sb-1")
	resp := client{t: t}.do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot || string(body) != `{"ok":true}` {
		t.Errorf("response %d %q", resp.StatusCode, body)
	}
	if got, want := <-calls, (call{path, `{"name":"dist"}`, "Bearer job-token", ""}); got != want {
		t.Errorf("upstream got %+v, want %+v", got, want)
	}

	// The service port listens on all addresses. Only sandboxes and the host
	// itself may send requests to GitHub through it, else anyone could hide
	// behind the server.
	tests := []struct {
		name   string
		remote string
		header http.Header
		want   int
	}{
		{"sandbox without a job", "10.213.0.5:40000", nil, http.StatusTeapot},
		{"this host", "127.0.0.1:40000", nil, http.StatusTeapot},
		{"internet host", "203.0.113.9:40000", nil, http.StatusForbidden},
		{"internet host that names a sandbox", "203.0.113.9:40000", http.Header{SandboxHeader: {"10.213.0.5"}}, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
			req.RemoteAddr = tt.remote
			for k, v := range tt.header {
				req.Header[k] = v
			}
			rec := httptest.NewRecorder()
			f.srv.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status %d, want %d", rec.Code, tt.want)
			}
			if sent := len(calls) > 0; sent != (tt.want == http.StatusTeapot) {
				t.Errorf("request sent to GitHub: %v", sent)
			}
			for len(calls) > 0 {
				<-calls
			}
		})
	}
}

// lateJob is a sandbox whose job runplane learns after linked is set, as
// with GitHub, which sends the job message after the job starts.
type lateJob struct{ linked atomic.Bool }

func (l *lateJob) Sandbox(context.Context, string, http.Header) (Identity, error) {
	if !l.linked.Load() {
		return Identity{SandboxID: "sb-1"}, nil
	}
	return acme("sb-1", "refs/heads/main", ""), nil
}

// The first cache step of a job runs before runplane knows the job of the
// sandbox. The request must wait for the job, not fail with 403.
func TestCacheRequestWaitsForTheJob(t *testing.T) {
	tests := []struct {
		name    string
		linkIn  time.Duration // 0 = never
		wantErr error
	}{
		{"job known after 200 ms", 200 * time.Millisecond, nil},
		{"job never known", 0, ErrNoSandbox},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &lateJob{}
			s := &Server{cfg: Config{Resolver: r}, jobWait: time.Second}
			if tt.linkIn > 0 {
				time.AfterFunc(tt.linkIn, func() { r.linked.Store(true) })
			}
			ident, err := s.identify(httptest.NewRequest("POST", "/twirp/x", nil), false)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && ident.Repository != "acme/app" {
				t.Errorf("identity %+v, want the repository of the job", ident)
			}
		})
	}
}

func TestStatsAddPerHour(t *testing.T) {
	st := openStore(t)
	s := NewStats(st, slog.New(slog.DiscardHandler))
	t0 := time.Date(2026, 11, 4, 10, 15, 0, 0, time.UTC)
	now := t0
	s.now = func() time.Time { return now }
	s.Add("acme/api", store.CacheCounts{Hits: 1, BytesRead: 100})
	now = t0.Add(30 * time.Minute)
	s.Add("acme/api", store.CacheCounts{Misses: 1})
	s.Add("acme/web", store.CacheCounts{BytesWritten: 7})
	now = t0.Add(time.Hour)
	s.Add("acme/api", store.CacheCounts{Hits: 2})

	// Run writes the counts when it ends.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx)
	// A later flush adds to the rows that exist.
	s.Add("acme/api", store.CacheCounts{Hits: 1})
	s.flush(context.Background())

	tests := []struct {
		since time.Time
		want  map[string]store.CacheCounts
	}{
		{t0, map[string]store.CacheCounts{"acme/api": {Hits: 4, Misses: 1, BytesRead: 100}, "acme/web": {BytesWritten: 7}}},
		{t0.Add(time.Hour), map[string]store.CacheCounts{"acme/api": {Hits: 3}}},
	}
	for _, tt := range tests {
		got, err := st.CacheStatsByRepository(context.Background(), tt.since)
		if err != nil || !maps.Equal(got, tt.want) {
			t.Errorf("counts since %s = %v, %v; want %v", tt.since, got, err, tt.want)
		}
	}
}
