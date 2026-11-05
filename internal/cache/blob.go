package cache

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/blob"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/store"
)

// blobPrefix is the path of the blob endpoints. The Azure SDK reads a URL on
// an IP address as /<account>/<container>/<blob> and a URL on a host name as
// /<container>/<blob>; this path is valid in both forms.
const blobPrefix = "/runplane/cache/"

const (
	// urlTTL is how long a signed blob URL stays valid. BuildKit asks for a new
	// download URL when one expires.
	urlTTL = time.Hour
	// Azure limits: one Put Blob request, and uncommitted blocks of one blob.
	maxRequestBytes   = 5000 << 20
	maxBlocks         = 100_000
	maxBlockListBytes = 8 << 20
)

// sign makes a blob URL in the style of an Azure SAS: sp is the permission
// (r or w), se the expiry in unix seconds, sig the HMAC.
func (s *Server) sign(base, perm, id string, expires time.Time) string {
	se := strconv.FormatInt(expires.Unix(), 10)
	q := url.Values{"sp": {perm}, "se": {se}, "sig": {s.mac(perm, id, se)}}
	return base + blobPrefix + id + "?" + q.Encode()
}

func (s *Server) signed(perm, id string, q url.Values) bool {
	se := q.Get("se")
	expires, err := strconv.ParseInt(se, 10, 64)
	if err != nil || q.Get("sp") != perm || s.now().Unix() > expires {
		return false
	}
	return hmac.Equal([]byte(q.Get("sig")), []byte(s.mac(perm, id, se)))
}

func (s *Server) mac(perm, id, se string) string {
	h := hmac.New(sha256.New, s.cfg.SigningKey)
	fmt.Fprintf(h, "%s\n%s\n%s", perm, id, se)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// serveBlob emulates the Azure Blob calls that the cache clients make on
// signed URLs: Put Blob, Put Block, Put Block List, and Get Blob with a range.
func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request, trusted bool) {
	id := strings.TrimPrefix(r.URL.Path, blobPrefix)
	perm := "r"
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodPut:
		perm = "w"
	default:
		blobError(w, http.StatusMethodNotAllowed, "UnsupportedHttpVerb", "method not allowed")
		return
	}
	if !s.signed(perm, id, r.URL.Query()) {
		blobError(w, http.StatusForbidden, "AuthenticationFailed", "the URL signature is not valid or has expired")
		return
	}
	e, err := s.cfg.Store.GetCacheEntry(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		blobError(w, http.StatusNotFound, "BlobNotFound", "the cache entry does not exist")
		return
	}
	if err != nil {
		s.cfg.Logger.Error("cache: read entry", "id", id, "err", err)
		blobError(w, http.StatusInternalServerError, "InternalError", "internal error")
		return
	}
	ident, err := s.identify(r, trusted)
	if err != nil && !errors.Is(err, ErrNoSandbox) {
		s.cfg.Logger.Error("cache: identify sandbox", "err", err)
		blobError(w, http.StatusInternalServerError, "InternalError", "internal error")
		return
	}
	if err != nil || ident.Repository != e.Repository {
		blobError(w, http.StatusForbidden, "AuthorizationFailure", "this sandbox may not use this cache entry")
		return
	}
	if r.Method == http.MethodPut {
		s.upload(w, r, e)
	} else {
		s.download(w, r, e)
	}
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request, e store.CacheEntry) {
	if e.Finalized {
		blobError(w, http.StatusConflict, "BlobImmutable", "the cache entry is finalized")
		return
	}
	q := r.URL.Query()
	want := r.ContentLength
	if q.Get("comp") == "blocklist" {
		// The commit copies the staged blocks into the blob.
		_, staged := s.disk.staged(e.ID)
		want = max(staged, 1)
	}
	reserved, err := s.reserve(r.Context(), want)
	if errors.Is(err, errTooLarge) {
		blobError(w, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge", "the cache has no room for this upload")
		return
	}
	if err != nil {
		s.cfg.Logger.Error("cache: reserve upload space", "id", e.ID, "err", err)
		blobError(w, http.StatusInternalServerError, "InternalError", "internal error")
		return
	}
	defer s.release(e.ID, reserved)
	switch q.Get("comp") {
	case "":
		err = s.disk.putBlob(e.ID, r.Body, reserved)
	case "block":
		blockID := q.Get("blockid")
		if blockID == "" {
			blobError(w, http.StatusBadRequest, "InvalidQueryParameterValue", "blockid is missing")
			return
		}
		blocks, size := s.disk.staged(e.ID)
		if blocks >= maxBlocks {
			blobError(w, http.StatusConflict, "BlockCountExceedsLimit", "the upload has too many blocks")
			return
		}
		err = s.disk.putBlock(e.ID, blockID, r.Body, min(reserved, s.Limit()-size))
	case "blocklist":
		var blocks []string
		blocks, err = parseBlockList(io.LimitReader(r.Body, maxBlockListBytes))
		if err != nil {
			blobError(w, http.StatusBadRequest, "InvalidXmlDocument", err.Error())
			return
		}
		err = s.disk.commitBlocks(e.ID, blocks)
	default:
		blobError(w, http.StatusBadRequest, "InvalidQueryParameterValue", "comp is not supported")
		return
	}
	switch {
	case errors.Is(err, errTooLarge):
		blobError(w, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge", "the upload is larger than the cache limit")
		return
	case errors.Is(err, errNoBlock), errors.Is(err, errEmptyCommit):
		blobError(w, http.StatusBadRequest, "InvalidBlockList", err.Error())
		return
	case err != nil:
		s.cfg.Logger.Error("cache: write upload", "id", e.ID, "err", err)
		blobError(w, http.StatusInternalServerError, "InternalError", "internal error")
		return
	}
	if err := s.cfg.Store.TouchCacheEntry(r.Context(), e.ID, s.now()); err != nil {
		s.cfg.Logger.Warn("cache: touch entry", "id", e.ID, "err", err)
	}
	h := w.Header()
	h.Set("ETag", `"`+ids.New()+`"`)
	h.Set("Last-Modified", s.now().UTC().Format(http.TimeFormat))
	setAzureHeaders(h)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request, e store.CacheEntry) {
	if !e.Finalized {
		blobError(w, http.StatusNotFound, "BlobNotFound", "the cache entry is not finalized")
		return
	}
	f, err := s.disk.open(e.ID)
	if err != nil {
		s.cfg.Logger.Warn("cache: open blob", "id", e.ID, "err", err)
		blobError(w, http.StatusNotFound, "BlobNotFound", "the cache entry has no data")
		return
	}
	defer f.Close()
	// The Azure SDKs send the range in x-ms-range.
	if rg := r.Header.Get("x-ms-range"); rg != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Range", rg)
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("ETag", `"`+e.ID+`"`)
	h.Set("x-ms-blob-type", "BlockBlob")
	setAzureHeaders(h)
	if n := blob.Serve(w, r, e.CreatedAt, f); n > 0 {
		s.cfg.Stats.Add(e.Repository, store.CacheCounts{BytesRead: n})
	}
}

// parseBlockList reads the block IDs of a Put Block List body in order. The
// server keeps only one copy of each block, so Latest, Committed, and
// Uncommitted all name the staged block.
func parseBlockList(r io.Reader) ([]string, error) {
	var list struct {
		Blocks []struct {
			ID string `xml:",chardata"`
		} `xml:",any"`
	}
	if err := xml.NewDecoder(r).Decode(&list); err != nil {
		return nil, fmt.Errorf("read block list: %w", err)
	}
	ids := make([]string, len(list.Blocks))
	for i, b := range list.Blocks {
		ids[i] = strings.TrimSpace(b.ID)
	}
	return ids, nil
}

// setAzureHeaders sets the Azure response headers that clients read.
// BuildKit's Azure SDK fails on responses without x-ms-request-id.
func setAzureHeaders(h http.Header) {
	h.Set("x-ms-request-id", ids.New())
	h.Set("x-ms-version", "2025-01-05")
}

func blobError(w http.ResponseWriter, status int, code, msg string) {
	h := w.Header()
	h.Set("Content-Type", "application/xml")
	h.Set("x-ms-error-code", code)
	setAzureHeaders(h)
	w.WriteHeader(status)
	var esc strings.Builder
	xml.EscapeText(&esc, []byte(msg))
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, esc.String())
}
