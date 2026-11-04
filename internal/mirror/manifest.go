package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
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
	if ok && time.Since(cached.Checked) < m.tagTTL {
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
