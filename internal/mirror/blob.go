package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// stallTimeout stops a blob download when upstream sends nothing for this
// long.
const stallTimeout = time.Minute

func (m *Mirror) serveBlob(w http.ResponseWriter, r *http.Request, name, digest string) {
	if r.Method == http.MethodHead {
		if f := m.store.open(blobKey(digest)); f != nil {
			m.serveCached(w, r, digest, f)
			return
		}
		m.misses.Add(1)
		m.headBlob(w, r, name, digest)
		return
	}
	f, d := m.openBlob(name, digest)
	if f != nil {
		m.serveCached(w, r, digest, f)
		return
	}
	m.misses.Add(1)
	defer m.release(d)
	m.streamBlob(w, r, d)
}

func (m *Mirror) serveCached(w http.ResponseWriter, r *http.Request, digest string, f *os.File) {
	defer f.Close()
	m.hits.Add(1)
	setBlobHeaders(w, digest)
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, "", time.Time{}, f)
	m.hitBytes.Add(cw.n)
}

// headBlob answers a HEAD request with the size from upstream. It does not
// download the blob.
func (m *Mirror) headBlob(w http.ResponseWriter, r *http.Request, name, digest string) {
	resp, err := m.up.do(r.Context(), http.MethodHead, name, "blobs/"+digest, "")
	if err != nil {
		m.fail(w, r, err, "BLOB_UNKNOWN")
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		m.fail(w, r, &upstreamError{resp.StatusCode}, "BLOB_UNKNOWN")
		return
	}
	setBlobHeaders(w, digest)
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(http.StatusOK)
}

// openBlob returns the cached blob, or else the download of the blob, which
// it starts if none runs. The caller releases the download.
func (m *Mirror) openBlob(name, digest string) (*os.File, *download) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f := m.store.open(blobKey(digest)); f != nil {
		return f, nil
	}
	d, ok := m.downloads[digest]
	if !ok {
		d = &download{digest: digest, refs: 1, changed: make(chan struct{})}
		m.downloads[digest] = d
		m.wg.Go(func() { m.download(d, name) })
	}
	d.refs++
	return nil, d
}

// release ends one use of d. The last use closes the temp file.
func (m *Mirror) release(d *download) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.refs--; d.refs == 0 && d.file != nil {
		d.file.Close()
	}
}

// streamBlob sends the blob to the client while the download runs.
func (m *Mirror) streamBlob(w http.ResponseWriter, r *http.Request, d *download) {
	ctx := r.Context()
	if r.Header.Get("Range") != "" {
		// Serve the range from the complete file after the download.
		end, err := d.wait(ctx, math.MaxInt64)
		if err != nil {
			m.fail(w, r, err, "BLOB_UNKNOWN")
			return
		}
		setBlobHeaders(w, d.digest)
		http.ServeContent(w, r, "", time.Time{}, io.NewSectionReader(d.file, 0, end))
		return
	}

	end, err := d.wait(ctx, 0)
	if err != nil {
		m.fail(w, r, err, "BLOB_UNKNOWN")
		return
	}
	size := d.size
	if size < 0 {
		size = end // without a known size, wait returns at the end only
	}
	setBlobHeaders(w, d.digest)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	buf := make([]byte, 64<<10)
	for off := int64(0); off < size; {
		if off == end {
			rc.Flush()
			if end, err = d.wait(ctx, off); err != nil {
				return // the short body tells the client that the blob failed
			}
		}
		n, err := d.file.ReadAt(buf[:min(int64(len(buf)), end-off)], off)
		if err != nil {
			return
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return
		}
		off += int64(n)
	}
}

// download fetches a blob for all clients that ask for it, and caches it if
// its digest is correct.
func (m *Mirror) download(d *download, name string) {
	defer m.release(d)
	if err := m.fetchBlob(d, name); err != nil {
		m.log.Warn("registry mirror cannot fetch a blob", "name", name, "digest", d.digest, "err", err)
		m.endDownload(d) // so that the next request tries again
		d.finish(err)
		return
	}
	// Until the commit, new requests read the finished temp file.
	d.finish(nil)
	if err := m.commit(d); err != nil {
		m.log.Error("registry mirror cannot cache a blob", "err", err)
	}
	m.endDownload(d)
}

// endDownload removes d from the downloads in progress.
func (m *Mirror) endDownload(d *download) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.downloads, d.digest)
}

// fetchBlob copies the blob from upstream into a temp file and checks its
// digest. On an error, it removes the temp file.
func (m *Mirror) fetchBlob(d *download, name string) (err error) {
	ctx, cancel := context.WithCancel(m.ctx)
	defer cancel()
	resp, err := m.up.do(ctx, http.MethodGet, name, "blobs/"+d.digest, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &upstreamError{resp.StatusCode}
	}
	f, err := m.store.temp()
	if err != nil {
		return fmt.Errorf("fetch blob: %w", err)
	}
	defer func() {
		if err != nil {
			os.Remove(f.Name())
		}
	}()
	d.begin(f, resp.ContentLength)

	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(d, hash), &stallReader{resp.Body, stall})
	m.missBytes.Add(n)
	if err != nil {
		return fmt.Errorf("fetch blob: %w", err)
	}
	if got := "sha256:" + hex.EncodeToString(hash.Sum(nil)); got != d.digest {
		return fmt.Errorf("upstream sent blob %s for %s", got, d.digest)
	}
	return nil
}

// commit moves the checked temp file of d into the cache.
func (m *Mirror) commit(d *download) error {
	if err := d.file.Sync(); err != nil {
		os.Remove(d.file.Name())
		return fmt.Errorf("sync blob: %w", err)
	}
	return m.store.add(blobKey(d.digest), d.file.Name(), d.written)
}

// download is one upstream fetch of a blob into a temp file. Each client that
// asks for the blob during the fetch reads the temp file while it grows.
type download struct {
	digest string
	refs   int // guarded by Mirror.mu

	mu      sync.Mutex
	file    *os.File
	size    int64 // from upstream; -1 if unknown
	written int64
	done    bool
	err     error
	changed chan struct{} // closed and replaced at each change
}

func (d *download) begin(f *os.File, size int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.file, d.size = f, size
}

// Write appends p to the temp file and lets the readers send it.
func (d *download) Write(p []byte) (int, error) {
	n, err := d.file.Write(p)
	d.mu.Lock()
	d.written += int64(n)
	d.notify()
	d.mu.Unlock()
	return n, err
}

func (d *download) finish(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done, d.err = true, err
	d.notify()
}

func (d *download) notify() {
	close(d.changed)
	d.changed = make(chan struct{})
}

// wait blocks until the download has safe bytes after off, or until it ends.
// It returns the end of the safe bytes. Before the digest check, the last
// byte is not safe, so that a client never gets a complete bad blob. Without
// a known size, no byte is safe before the end.
func (d *download) wait(ctx context.Context, off int64) (int64, error) {
	for {
		d.mu.Lock()
		var end int64
		switch {
		case d.done:
			end = d.written
		case d.size >= 0:
			end = min(d.written, d.size-1)
		}
		done, err, changed := d.done, d.err, d.changed
		d.mu.Unlock()
		if err != nil {
			return 0, err
		}
		if end > off || done {
			return end, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

// stallReader resets the stall timer at each read.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.timer.Reset(stallTimeout)
	return n, err
}

// countingWriter counts the body bytes that go to the client.
type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

// ReadFrom keeps the sendfile path of the ResponseWriter.
func (c *countingWriter) ReadFrom(r io.Reader) (int64, error) {
	n, err := io.Copy(c.ResponseWriter, r)
	c.n += n
	return n, err
}

func setBlobHeaders(w http.ResponseWriter, digest string) {
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Docker-Content-Digest", digest)
}

func blobKey(digest string) string {
	return blobsDir + "/" + strings.TrimPrefix(digest, "sha256:")
}
