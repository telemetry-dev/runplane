package cache

import (
	"context"
	"fmt"
	"math"
	"sync"
)

const (
	// evictMargin is how far above MinFreePercent the eviction frees the
	// disk, so that it does not run again at each upload.
	evictMargin = 5
	// maxOpenUploads is the count of unfinished uploads that one sandbox may
	// have. Each one is an index row until it is finalized or stale.
	maxOpenUploads = 64
)

// uploads counts the bytes of the unfinished uploads, which count against
// the limit like the entries: their files on disk and the writes in
// progress. Without the count, many unfinished uploads could each take the
// whole limit.
type uploads struct {
	mu      sync.Mutex
	onDisk  map[string]int64  // bytes of the files of each unfinished entry, by entry ID
	writing int64             // bytes that the writes in progress may still add
	owner   map[string]string // sandbox ID by unfinished entry ID
}

func (u *uploads) bytes() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := u.writing
	for _, b := range u.onDisk {
		n += b
	}
	return n
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

// drop stops the count of an upload that is finalized or removed.
func (u *uploads) drop(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.onDisk, id)
	delete(u.owner, id)
}

// Limit returns the byte limit of all entries and unfinished uploads, and
// so of one entry: MaxBytes, and at most a quarter of the disk.
func (s *Server) Limit() int64 {
	limit := int64(math.MaxInt64)
	if n := s.cfg.MaxBytes(); n > 0 {
		limit = n
	}
	if total, _, err := s.cfg.DiskSpace(); err == nil && total > 0 {
		limit = min(limit, total/4)
	}
	return limit
}

// diskShort returns the bytes that the disk lacks to have MinFreePercent
// plus margin free, or 0.
func (s *Server) diskShort(margin float64) int64 {
	total, free, err := s.cfg.DiskSpace()
	if err != nil || total <= 0 {
		return 0
	}
	return max(0, int64(float64(total)*(s.cfg.MinFreePercent()+margin)/100)-free)
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
	// The entries can make room only for what fits next to the other uploads.
	if s.uploads.bytes()+extra > s.Limit() {
		return 0, errTooLarge
	}
	if err := s.evict(ctx, extra); err != nil {
		return 0, err
	}
	if s.diskShort(0) > 0 {
		return 0, errTooLarge
	}
	entries, err := s.cfg.Store.CacheSize(ctx)
	if err != nil {
		return 0, err
	}
	u := &s.uploads
	u.mu.Lock()
	defer u.mu.Unlock()
	room := s.Limit() - entries - u.writing
	for _, n := range u.onDisk {
		room -= n
	}
	if room <= 0 {
		return 0, errTooLarge
	}
	n := min(want, room)
	u.writing += n
	return n, nil
}

// release gives back the bytes of reserve after a write to the upload id,
// and counts its files as they are now.
func (s *Server) release(id string, reserved int64) {
	_, staged := s.disk.staged(id)
	blob, _ := s.disk.size(id)
	u := &s.uploads
	u.mu.Lock()
	defer u.mu.Unlock()
	u.writing -= reserved
	if staged+blob > 0 {
		u.onDisk[id] = staged + blob
	}
}

// countUploads counts the files of the unfinished uploads that an earlier
// process left.
func (s *Server) countUploads(ctx context.Context) {
	list, err := s.cfg.Store.ListUnfinishedCacheEntries(ctx, s.now())
	if err != nil {
		s.cfg.Logger.Error("cache: list unfinished uploads", "err", err)
		return
	}
	for _, e := range list {
		s.release(e.ID, 0)
	}
}

// evict removes the least recently used entries until the entries, the
// unfinished uploads, and extra more bytes fit in the limit, and the disk
// has MinFreePercent plus evictMargin free.
func (s *Server) evict(ctx context.Context, extra int64) error {
	s.evictMu.Lock()
	defer s.evictMu.Unlock()
	total, err := s.cfg.Store.CacheSize(ctx)
	if err != nil {
		return err
	}
	over := int64(0)
	if limit := s.Limit(); limit != math.MaxInt64 {
		over = total + s.uploads.bytes() + extra - limit
	}
	over = max(over, s.diskShort(evictMargin))
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
