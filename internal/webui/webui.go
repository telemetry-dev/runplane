// Package webui serves the frontend build that "make web" copies into dist.
package webui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the files of the frontend build. Files under /assets/ have
// hashed names, so browsers may keep them for a year. Every other path that
// is not a file gets the SPA shell, so the client router can handle it.
func Handler() http.Handler {
	sub, _ := fs.Sub(dist, "dist")
	return newHandler(sub)
}

type handler struct {
	files map[string]file
	shell *file // nil if the frontend is not built
}

type file struct {
	data []byte
	etag string
}

// shellName is the SPA shell of the build: prerender.outputPath in
// web/vite.config.ts.
const shellName = "index.html"

func newHandler(fsys fs.FS) *handler {
	h := &handler{files: map[string]file{}}
	fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		data, _ := fs.ReadFile(fsys, name)
		sum := sha256.Sum256(data)
		h.files[name] = file{data: data, etag: `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`}
		return nil
	})
	if f, ok := h.files[shellName]; ok {
		h.shell = &f
	}
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	f, ok := h.files[name]
	if !ok || name == shellName {
		h.serveShell(w, r)
		return
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("ETag", f.etag)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(f.data))
}

func (h *handler) serveShell(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	if h.shell == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(notBuilt)
		return
	}
	w.Header().Set("ETag", h.shell.etag)
	http.ServeContent(w, r, shellName, time.Time{}, bytes.NewReader(h.shell.data))
}

var notBuilt = []byte(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>runplane</title></head>
<body style="font-family: system-ui, sans-serif; max-width: 40rem; margin: 4rem auto; padding: 0 1rem">
<h1>The dashboard is not built</h1>
<p>This runplane binary has no frontend. Run <code>make web</code>, then build runplane again.</p>
<p>The API is available at <a href="/api/status">/api/status</a>.</p>
</body>
</html>
`)
