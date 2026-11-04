package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func get(t *testing.T, h http.Handler, path string, header ...string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestServesBuild(t *testing.T) {
	for _, shellName := range []string{"index.html", "_shell.html"} {
		t.Run(shellName, func(t *testing.T) {
			h := newHandler(fstest.MapFS{
				shellName:              {Data: []byte("<html>shell</html>")},
				"assets/app-3f2a9c.js": {Data: []byte("console.log(1)")},
				"favicon.svg":          {Data: []byte("<svg/>")},
				".keep":                {},
			})

			res := get(t, h, "/assets/app-3f2a9c.js")
			if res.StatusCode != 200 || body(t, res) != "console.log(1)" {
				t.Fatalf("asset: status %d", res.StatusCode)
			}
			if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
				t.Fatalf("asset Cache-Control %q, want immutable", cc)
			}

			res = get(t, h, "/favicon.svg")
			if res.Header.Get("Cache-Control") != "no-cache" || body(t, res) != "<svg/>" {
				t.Fatalf("favicon: Cache-Control %q", res.Header.Get("Cache-Control"))
			}

			// Client routes, the shell itself, and dot files get the shell.
			for _, path := range []string{"/", "/jobs/01jabc", "/settings/github?error=x", "/" + shellName, "/.keep"} {
				res := get(t, h, path)
				if res.StatusCode != 200 || body(t, res) != "<html>shell</html>" {
					t.Fatalf("%s: status %d, want the shell", path, res.StatusCode)
				}
				if res.Header.Get("Cache-Control") != "no-cache" {
					t.Fatalf("%s: Cache-Control %q, want no-cache", path, res.Header.Get("Cache-Control"))
				}
			}

			etag := get(t, h, "/").Header.Get("ETag")
			if res := get(t, h, "/jobs", "If-None-Match", etag); etag == "" || res.StatusCode != http.StatusNotModified {
				t.Fatalf("revalidation with ETag %q: status %d, want 304", etag, res.StatusCode)
			}
		})
	}
}

func TestWithoutBuild(t *testing.T) {
	h := newHandler(fstest.MapFS{".keep": {}})
	res := get(t, h, "/")
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body(t, res), "make web") {
		t.Fatalf("status %d, want 503 with the make web hint", res.StatusCode)
	}
}

func TestRejectsWrites(t *testing.T) {
	h := newHandler(fstest.MapFS{"index.html": {Data: []byte("shell")}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: status %d, want 405", rec.Code)
	}
}
