package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/store"
)

const twirpPrefix = "/twirp/github.actions.results.api.v1.CacheService/"

const (
	maxTwirpBody = 64 << 10
	maxKeyLen    = 512
	maxKeys      = 10 // the key and the restore keys
	// activeUpload is how long an unfinished upload blocks a new reservation
	// of the same entry after its last block.
	activeUpload = 10 * time.Minute
)

// The @actions/cache client sends JSON with proto field names and int64
// values as strings. BuildKit sends int64 values as numbers.
type (
	createRequest struct {
		Key     string `json:"key"`
		Version string `json:"version"`
	}
	createResponse struct {
		OK              bool   `json:"ok"`
		SignedUploadURL string `json:"signed_upload_url"`
		Message         string `json:"message"`
	}
	finalizeRequest struct {
		Key       string     `json:"key"`
		SizeBytes protoInt64 `json:"size_bytes"`
		Version   string     `json:"version"`
	}
	finalizeResponse struct {
		OK      bool   `json:"ok"`
		EntryID string `json:"entry_id"` // int64 in proto3 JSON
		Message string `json:"message"`
	}
	downloadRequest struct {
		Key         string   `json:"key"`
		RestoreKeys []string `json:"restore_keys"`
		Version     string   `json:"version"`
	}
	downloadResponse struct {
		OK                bool   `json:"ok"`
		SignedDownloadURL string `json:"signed_download_url"`
		MatchedKey        string `json:"matched_key"`
	}
)

// protoInt64 reads an int64 written as a JSON string or number.
type protoInt64 int64

func (n *protoInt64) UnmarshalJSON(b []byte) error {
	v, err := strconv.ParseInt(strings.Trim(string(b), `"`), 10, 64)
	if err != nil {
		return fmt.Errorf("read int64: %w", err)
	}
	*n = protoInt64(v)
	return nil
}

// twirpError is a Twirp error response. @actions/cache reads a msg that
// starts with "cache read denied:" as a miss, and an ok:false message that
// starts with "cache write denied:" as a refused save.
type twirpError struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

var twirpStatus = map[string]int{
	"invalid_argument":  http.StatusBadRequest,
	"malformed":         http.StatusBadRequest,
	"permission_denied": http.StatusForbidden,
	"not_found":         http.StatusNotFound,
	"bad_route":         http.StatusNotFound,
	"already_exists":    http.StatusConflict,
	"internal":          http.StatusInternalServerError,
}

func (e *twirpError) Error() string { return e.Code + ": " + e.Msg }

func twirpErr(code, format string, args ...any) *twirpError {
	return &twirpError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// noRefMessage refuses a save of a job whose ref runplane does not know,
// for example a pull request from a fork.
const noRefMessage = "cache write denied: runplane does not know the ref of this job"

// request is one authorized cache call.
type request struct {
	Identity
	base string // scheme and host for signed URLs
}

func (s *Server) serveTwirp(w http.ResponseWriter, r *http.Request, trusted bool) {
	method := strings.TrimPrefix(r.URL.Path, twirpPrefix)
	resp, err := s.twirp(r, method, trusted)
	if err != nil {
		var te *twirpError
		if !errors.As(err, &te) {
			s.cfg.Logger.Error("cache: "+method, "err", err)
			te = twirpErr("internal", "internal error")
		}
		writeJSON(w, twirpStatus[te.Code], te)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) twirp(r *http.Request, method string, trusted bool) (any, error) {
	if r.Method != http.MethodPost {
		return nil, twirpErr("bad_route", "use POST")
	}
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		return nil, twirpErr("bad_route", "only application/json is supported")
	}
	var body any
	switch method {
	case "CreateCacheEntry":
		body = &createRequest{}
	case "FinalizeCacheEntryUpload":
		body = &finalizeRequest{}
	case "GetCacheEntryDownloadURL":
		body = &downloadRequest{}
	default:
		return nil, twirpErr("bad_route", "no method %s", method)
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxTwirpBody)).Decode(body); err != nil {
		return nil, twirpErr("malformed", "read request: %v", err)
	}

	ident, err := s.identify(r, trusted)
	if errors.Is(err, ErrNoSandbox) {
		denied := "cache write denied"
		if method == "GetCacheEntryDownloadURL" {
			denied = "cache read denied"
		}
		return nil, twirpErr("permission_denied", "%s: the request comes from no known sandbox job", denied)
	}
	if err != nil {
		return nil, fmt.Errorf("identify sandbox: %w", err)
	}
	req := request{Identity: ident, base: baseURL(r, trusted)}

	ctx := r.Context()
	switch body := body.(type) {
	case *createRequest:
		return s.create(ctx, req, body)
	case *finalizeRequest:
		return s.finalize(ctx, req, body)
	default:
		return s.downloadURL(ctx, req, body.(*downloadRequest))
	}
}

func (s *Server) create(ctx context.Context, req request, in *createRequest) (any, error) {
	if err := checkKeys(in.Key, in.Version); err != nil {
		return nil, err
	}
	if req.Ref == "" {
		return createResponse{Message: noRefMessage}, nil
	}
	now := s.now()
	id := ids.New()
	e := store.CacheEntry{
		ID:         id,
		Repository: req.Repository,
		Ref:        req.Ref,
		Key:        in.Key,
		Version:    in.Version,
		CreatedAt:  now,
		LastUsedAt: now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.cfg.Store.GetCacheEntryByKey(ctx, e.Repository, e.Ref, e.Key, e.Version)
	switch {
	case err == nil && (old.Finalized || now.Sub(old.LastUsedAt) < activeUpload):
		return nil, twirpErr("already_exists", "cache entry with the same key, version, and scope already exists")
	case err == nil:
		if err := s.remove(ctx, old); err != nil {
			return nil, err
		}
	case !errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("find entry: %w", err)
	}
	if !s.uploads.open(id, req.SandboxID) {
		return createResponse{Message: "cache write denied: too many uploads of this job are in progress"}, nil
	}
	err = s.cfg.Store.CreateCacheEntry(ctx, e)
	if err != nil {
		s.uploads.drop(id)
	}
	if errors.Is(err, store.ErrConflict) {
		return nil, twirpErr("already_exists", "cache entry with the same key, version, and scope already exists")
	}
	if err != nil {
		return nil, fmt.Errorf("create entry: %w", err)
	}
	return createResponse{OK: true, SignedUploadURL: s.sign(req.base, "w", id, now.Add(urlTTL))}, nil
}

func (s *Server) finalize(ctx context.Context, req request, in *finalizeRequest) (any, error) {
	if err := checkKeys(in.Key, in.Version); err != nil {
		return nil, err
	}
	if req.Ref == "" {
		return finalizeResponse{Message: noRefMessage}, nil
	}
	resp, err := s.commit(ctx, req, in)
	if err == nil && resp.OK {
		if err := s.evict(ctx, 0); err != nil {
			s.cfg.Logger.Error("cache: evict", "err", err)
		}
	}
	return resp, err
}

func (s *Server) commit(ctx context.Context, req request, in *finalizeRequest) (finalizeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.cfg.Store.GetCacheEntryByKey(ctx, req.Repository, req.Ref, in.Key, in.Version)
	if errors.Is(err, store.ErrNotFound) {
		return finalizeResponse{}, twirpErr("not_found", "no upload for this cache entry")
	}
	if err != nil {
		return finalizeResponse{}, fmt.Errorf("find entry: %w", err)
	}
	if e.Finalized {
		return finalizeResponse{OK: true, EntryID: entryID(e)}, nil
	}

	size, err := s.disk.size(e.ID)
	var problem string
	switch {
	case errors.Is(err, errNotCommitted):
		problem = "the upload has no committed blob"
	case err != nil:
		return finalizeResponse{}, err
	case in.SizeBytes > 0 && size != int64(in.SizeBytes):
		problem = fmt.Sprintf("the upload has %d bytes, not %d", size, in.SizeBytes)
	case size > s.MaxBytes():
		problem = fmt.Sprintf("the entry has %d bytes, more than the cache limit of %d bytes", size, s.MaxBytes())
	}
	if problem != "" {
		// Drop the upload, so that the job can try again at once.
		if err := s.remove(ctx, e); err != nil {
			s.cfg.Logger.Warn("cache: remove failed upload", "id", e.ID, "err", err)
		}
		return finalizeResponse{Message: "cache upload failed: " + problem}, nil
	}
	if err := s.cfg.Store.FinishCacheEntry(ctx, e.ID, size, s.now()); err != nil {
		return finalizeResponse{}, fmt.Errorf("finalize entry: %w", err)
	}
	s.uploads.drop(e.ID)
	s.stats.add(e.Repository, store.CacheCounts{BytesWritten: size})
	s.cfg.Logger.Info("cache: saved", "repository", e.Repository, "ref", e.Ref, "key", e.Key, "bytes", size)
	return finalizeResponse{OK: true, EntryID: entryID(e)}, nil
}

func (s *Server) downloadURL(ctx context.Context, req request, in *downloadRequest) (any, error) {
	keys := dedupe(append([]string{in.Key}, in.RestoreKeys...))
	if len(keys) > maxKeys {
		return nil, twirpErr("invalid_argument", "more than %d keys", maxKeys)
	}
	for _, k := range keys {
		if err := checkKeys(k, in.Version); err != nil {
			return nil, err
		}
	}
	if len(req.readRefs()) == 0 {
		return nil, twirpErr("permission_denied", "cache read denied: runplane knows no ref of this job")
	}
	e, err := s.match(ctx, req, keys, in.Version)
	if errors.Is(err, store.ErrNotFound) {
		s.stats.add(req.Repository, store.CacheCounts{Misses: 1})
		return downloadResponse{}, nil
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if err := s.cfg.Store.TouchCacheEntry(ctx, e.ID, now); err != nil {
		s.cfg.Logger.Warn("cache: touch entry", "id", e.ID, "err", err)
	}
	s.stats.add(req.Repository, store.CacheCounts{Hits: 1})
	return downloadResponse{
		OK:                true,
		SignedDownloadURL: s.sign(req.base, "r", e.ID, now.Add(urlTTL)),
		MatchedKey:        e.Key,
	}, nil
}

// match finds the entry to restore like GitHub: refs in order; in each ref,
// the keys in order; for each key, an exact match, then the newest entry
// whose key starts with it. An entry with a missing or damaged blob is
// removed, and the search continues.
func (s *Server) match(ctx context.Context, req request, keys []string, version string) (store.CacheEntry, error) {
	for range 10 {
		e, err := s.lookup(ctx, req, keys, version)
		if err != nil {
			return store.CacheEntry{}, err
		}
		size, err := s.disk.size(e.ID)
		if err == nil && size == e.SizeBytes {
			return e, nil
		}
		s.cfg.Logger.Warn("cache: remove entry with damaged blob", "id", e.ID, "size", size, "want", e.SizeBytes, "err", err)
		if err := s.remove(ctx, e); err != nil {
			return store.CacheEntry{}, err
		}
	}
	return store.CacheEntry{}, errors.New("too many entries with damaged blobs")
}

func (s *Server) lookup(ctx context.Context, req request, keys []string, version string) (store.CacheEntry, error) {
	for _, ref := range req.readRefs() {
		for _, key := range keys {
			for _, prefix := range []bool{false, true} {
				e, err := s.cfg.Store.MatchCacheEntry(ctx, req.Repository, version, ref, key, prefix)
				if !errors.Is(err, store.ErrNotFound) {
					return e, err
				}
			}
		}
	}
	return store.CacheEntry{}, store.ErrNotFound
}

// dedupe drops repeated strings and keeps the first of each.
func dedupe(list []string) []string {
	var out []string
	for _, s := range list {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func checkKeys(key, version string) error {
	switch {
	case key == "" || version == "":
		return twirpErr("invalid_argument", "key and version must not be empty")
	case len(key) > maxKeyLen || len(version) > maxKeyLen:
		return twirpErr("invalid_argument", "key and version must be at most %d bytes", maxKeyLen)
	}
	return nil
}

// entryID returns the int64 entry ID of the protocol. Clients only log it.
func entryID(e store.CacheEntry) string {
	return strconv.FormatInt(e.CreatedAt.UnixMilli(), 10)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
