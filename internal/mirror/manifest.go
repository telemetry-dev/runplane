package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// manifestAccept asks upstream for each manifest type, whatever the client
// accepts. dockerd and containerd accept all of them.
const manifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

const (
	maxManifestBytes = 4 << 20
	fetchTimeout     = time.Minute
)

type manifest struct {
	mediaType string
	body      []byte
}

func (m *Mirror) serveManifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	ctx := r.Context()
	digest := ref
	if !strings.HasPrefix(ref, "sha256:") {
		var err error
		if digest, err = m.resolveTag(ctx, name, ref); err != nil {
			m.fail(w, r, err, "MANIFEST_UNKNOWN")
			return
		}
	}
	// HEAD also fetches the manifest, because the client asks for it next.
	mf, fetched, err := m.manifest(ctx, name, digest)
	if err != nil {
		m.fail(w, r, err, "MANIFEST_UNKNOWN")
		return
	}
	if fetched {
		m.misses.Add(1)
	} else {
		m.hits.Add(1)
		if r.Method == http.MethodGet {
			m.hitBytes.Add(int64(len(mf.body)))
		}
	}
	h := w.Header()
	h.Set("Content-Type", mf.mediaType)
	h.Set("Docker-Content-Digest", digest)
	h.Set("Content-Length", strconv.Itoa(len(mf.body)))
	if r.Method == http.MethodGet {
		w.Write(mf.body)
	}
}

// resolveTag returns the digest of name:tag. It trusts the cached digest for
// the tag TTL. Then it asks upstream with HEAD, which does not count against
// the Docker Hub pull limit. If upstream is down, it returns the stale digest.
func (m *Mirror) resolveTag(ctx context.Context, name, tag string) (string, error) {
	cached, ok := m.tags.get(name, tag)
	if ok && time.Since(cached.Checked) < tagTTL {
		return cached.Digest, nil
	}
	digest, err := m.tagFlights.do(ctx, &m.wg, name+":"+tag, func() (string, error) {
		ctx, cancel := context.WithTimeout(m.ctx, fetchTimeout)
		defer cancel()
		return m.headManifest(ctx, name, tag)
	})
	switch {
	case err == nil:
		return digest, nil
	case ok && ctx.Err() == nil && !notFound(err):
		m.log.Warn("registry mirror serves a stale tag", "name", name, "tag", tag, "err", err)
		return cached.Digest, nil
	default:
		return "", err
	}
}

func (m *Mirror) headManifest(ctx context.Context, name, tag string) (string, error) {
	resp, err := m.up.do(ctx, http.MethodHead, name, "manifests/"+tag, manifestAccept)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &upstreamError{resp.StatusCode}
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if !digestRe.MatchString(digest) {
		return "", fmt.Errorf("upstream sent no sha256 digest for %s:%s", name, tag)
	}
	if err := m.tags.put(name, tag, digest); err != nil {
		m.log.Error("registry mirror cannot save a tag", "err", err)
	}
	return digest, nil
}

// manifest returns the manifest with the digest from the cache, or else from
// upstream. fetched reports an upstream fetch.
func (m *Mirror) manifest(ctx context.Context, name, digest string) (mf manifest, fetched bool, err error) {
	if mf, ok := m.cachedManifest(digest); ok {
		return mf, false, nil
	}
	mf, err = m.manifestFlights.do(ctx, &m.wg, digest, func() (manifest, error) {
		ctx, cancel := context.WithTimeout(m.ctx, fetchTimeout)
		defer cancel()
		return m.fetchManifest(ctx, name, digest)
	})
	return mf, true, err
}

// cachedManifest reads a manifest file: the media type, a newline, and the
// body.
func (m *Mirror) cachedManifest(digest string) (manifest, bool) {
	f := m.store.open(manifestKey(digest))
	if f == nil {
		return manifest{}, false
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return manifest{}, false
	}
	mediaType, body, ok := bytes.Cut(data, []byte("\n"))
	return manifest{string(mediaType), body}, ok
}

func (m *Mirror) fetchManifest(ctx context.Context, name, digest string) (manifest, error) {
	resp, err := m.up.do(ctx, http.MethodGet, name, "manifests/"+digest, manifestAccept)
	if err != nil {
		return manifest{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return manifest{}, &upstreamError{resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	m.missBytes.Add(int64(len(body)))
	if len(body) > maxManifestBytes {
		return manifest{}, fmt.Errorf("manifest %s is larger than 4 MiB", digest)
	}
	sum := sha256.Sum256(body)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return manifest{}, fmt.Errorf("upstream sent manifest %s for %s", got, digest)
	}
	mf := manifest{resp.Header.Get("Content-Type"), body}
	if err := m.store.write(manifestKey(digest), []byte(mf.mediaType+"\n"), body); err != nil {
		m.log.Error("registry mirror cannot cache a manifest", "err", err)
	}
	return mf, nil
}

func manifestKey(digest string) string {
	return manifestsDir + "/" + strings.TrimPrefix(digest, "sha256:")
}

// tagIndex keeps the digest of each tag and the time of the last check with
// upstream. It writes each tag to a small file, so that a restart keeps the
// tags and a stale tag still works when upstream is down.
type tagIndex struct {
	dir string

	mu   sync.Mutex
	tags map[string]tagEntry // by "name:tag"
}

type tagEntry struct {
	Name    string    `json:"name"`
	Tag     string    `json:"tag"`
	Digest  string    `json:"digest"`
	Checked time.Time `json:"checked"`
}

func openTags(dir string) (*tagIndex, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("make tag folder: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read tag folder: %w", err)
	}
	t := &tagIndex{dir: dir, tags: map[string]tagEntry{}}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if !strings.HasSuffix(e.Name(), ".json") {
			os.Remove(path) // a temp file from a crash
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read tag: %w", err)
		}
		var te tagEntry
		// The next check with upstream writes a damaged file again.
		if json.Unmarshal(data, &te) == nil {
			t.tags[te.Name+":"+te.Tag] = te
		}
	}
	return t, nil
}

func (t *tagIndex) get(name, tag string) (tagEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	te, ok := t.tags[name+":"+tag]
	return te, ok
}

// put records that name:tag has the digest now.
func (t *tagIndex) put(name, tag, digest string) error {
	te := tagEntry{Name: name, Tag: tag, Digest: digest, Checked: time.Now().UTC()}
	data, err := json.Marshal(te)
	if err != nil {
		return err
	}
	key := name + ":" + tag
	t.mu.Lock()
	t.tags[key] = te
	t.mu.Unlock()

	// A hash for the file name, because a name can contain "/" and a file
	// for a name could collide with the folder for a longer name.
	sum := sha256.Sum256([]byte(key))
	f, err := os.CreateTemp(t.dir, "*.tmp")
	if err != nil {
		return fmt.Errorf("save tag: %w", err)
	}
	_, err = f.Write(data)
	if err := errors.Join(err, f.Close()); err != nil {
		return errors.Join(fmt.Errorf("save tag: %w", err), os.Remove(f.Name()))
	}
	if err := os.Rename(f.Name(), filepath.Join(t.dir, hex.EncodeToString(sum[:])+".json")); err != nil {
		return errors.Join(fmt.Errorf("save tag: %w", err), os.Remove(f.Name()))
	}
	return nil
}
