package cache

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// maxOpenUploads is the count of unfinished uploads that one sandbox may
// have. Each one is an index row until it is finalized or stale.
const maxOpenUploads = 64

// uploads holds what only memory knows of the unfinished uploads: the bytes
// that the writes in progress may still add, and the sandbox of each upload.
// The rows of the uploads keep the bytes of their files. The uploads count
// against the limit like the entries. Without the count, many unfinished
// uploads could each take the whole limit.
type uploads struct {
	mu      sync.Mutex
	writing int64
	owner   map[string]string // sandbox ID by unfinished entry ID
}

func (u *uploads) inFlight() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.writing
}

// open adds an upload of the sandbox. It returns false if the sandbox has
// maxOpenUploads already.
func (u *uploads) open(id, sandboxID string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, owner := range u.owner {
		if owner == sandboxID {
			n++
		}
	}
	if n >= maxOpenUploads {
		return false
	}
	u.owner[id] = sandboxID
	return true
}

func (u *uploads) drop(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.owner, id)
}

// MaxBytes returns the byte limit of all entries and unfinished uploads,
// and so of one entry, that the server uses now. It is math.MaxInt64 when
// the cache has no limit.
func (s *Server) MaxBytes() int64 {
	if limit := s.cfg.Space().Cache; limit > 0 {
		return limit
	}
	return math.MaxInt64
}

// reserve takes up to want bytes of the limit for one write to an upload.
// It first removes the least recently used entries to make room. want <= 0
// means that the size is not known; the write then gets the room that is
// left. The caller gives the bytes back with release. It returns errTooLarge
// when no room is left.
func (s *Server) reserve(ctx context.Context, want int64) (int64, error) {
	extra := min(max(want, 0), maxRequestBytes)
	if want <= 0 || want > maxRequestBytes {
		want = maxRequestBytes
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := s.MaxBytes()
	size, err := s.cfg.Store.CacheSize(ctx)
	if err != nil {
		return 0, err
	}
	// The entries can make room only for what fits next to the other uploads.
	if size.Uploads+s.uploads.inFlight()+extra > limit {
		return 0, errTooLarge
	}
	if err := s.evict(ctx, extra); err != nil {
		return 0, err
	}
	if s.cfg.Space().Low() {
		return 0, errTooLarge
	}
	if size, err = s.cfg.Store.CacheSize(ctx); err != nil {
		return 0, err
	}
	u := &s.uploads
	u.mu.Lock()
	defer u.mu.Unlock()
	room := limit - size.Entries - size.Uploads - u.writing
	if room <= 0 {
		return 0, errTooLarge
	}
	n := min(want, room)
	u.writing += n
	return n, nil
}

// release gives back the bytes of reserve after a write. The caller first
// records the files of a write that worked (recordUpload).
func (s *Server) release(reserved int64) {
	u := &s.uploads
	u.mu.Lock()
	defer u.mu.Unlock()
	u.writing -= reserved
}

// recordUpload writes the bytes that the files of the upload id have now in
// its row, with the time of its last use.
func (s *Server) recordUpload(ctx context.Context, id string, used time.Time) error {
	_, staged := s.disk.staged(id)
	blob, _ := s.disk.size(id)
	return s.cfg.Store.SetCacheUpload(ctx, id, staged+blob, used)
}

// syncUploads records the files of the unfinished uploads that an earlier
// process left, because it can stop between a write and its record.
func (s *Server) syncUploads(ctx context.Context) {
	list, err := s.cfg.Store.ListUnfinishedCacheEntries(ctx, s.now())
	if err != nil {
		s.cfg.Logger.Error("cache: list unfinished uploads", "err", err)
		return
	}
	for _, e := range list {
		if err := s.recordUpload(ctx, e.ID, e.LastUsedAt); err != nil {
			s.cfg.Logger.Warn("cache: record upload", "id", e.ID, "err", err)
		}
	}
}

// evict removes the least recently used entries until the entries, the
// unfinished uploads, and extra more bytes fit in the limit, and the disk
// has the free part that datadisk.Space.Reclaim asks for.
func (s *Server) evict(ctx context.Context, extra int64) error {
	s.evictMu.Lock()
	defer s.evictMu.Unlock()
	space := s.cfg.Space()
	over := space.Reclaim()
	if space.Cache > 0 {
		size, err := s.cfg.Store.CacheSize(ctx)
		if err != nil {
			return err
		}
		over = max(over, size.Entries+size.Uploads+s.uploads.inFlight()+extra-space.Cache)
	}
	for over > 0 {
		oldest, err := s.cfg.Store.LeastRecentCacheEntries(ctx, 64)
		if err != nil {
			return fmt.Errorf("list oldest entries: %w", err)
		}
		if len(oldest) == 0 {
			return nil
		}
		for _, e := range oldest {
			if over <= 0 {
				break
			}
			if err := s.remove(ctx, e); err != nil {
				return err
			}
			over -= e.SizeBytes
			s.cfg.Logger.Info("cache: evict", "repository", e.Repository, "key", e.Key, "bytes", e.SizeBytes)
		}
	}
	return nil
}
