package cache

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/store"
)

// ErrNotFound means that no cache entry has the ID.
var ErrNotFound = store.ErrNotFound

// Stats records cache use for each repository in the cache_stats table. The
// server counts on request paths, so Add only adds to counts in memory, and
// Run writes them each minute.
type Stats struct {
	st  *store.Store
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	pending map[hourRepo]store.CacheCounts
}

type hourRepo struct {
	hour       int64 // unix hour
	repository string
}

func NewStats(st *store.Store, log *slog.Logger) *Stats {
	return &Stats{st: st, log: log, now: time.Now, pending: map[hourRepo]store.CacheCounts{}}
}

func (s *Stats) Add(repository string, c store.CacheCounts) {
	k := hourRepo{s.now().Unix() / 3600, repository}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[k] = sum(s.pending[k], c)
}

// Run writes the counts each minute, and a last time when ctx ends.
func (s *Stats) Run(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flush(context.WithoutCancel(ctx))
			return
		case <-tick.C:
			s.flush(ctx)
		}
	}
}

// flush writes the pending counts. Counts that fail to write wait for the
// next flush.
func (s *Stats) flush(ctx context.Context) {
	s.mu.Lock()
	batch := s.pending
	s.pending = map[hourRepo]store.CacheCounts{}
	s.mu.Unlock()
	for k, c := range batch {
		if err := s.st.AddCacheStats(ctx, time.Unix(k.hour*3600, 0), k.repository, c); err != nil {
			s.log.Warn("record cache stats", "repository", k.repository, "err", err)
			s.mu.Lock()
			s.pending[k] = sum(s.pending[k], c)
			s.mu.Unlock()
		}
	}
}

func sum(a, b store.CacheCounts) store.CacheCounts {
	return store.CacheCounts{
		Hits: a.Hits + b.Hits, Misses: a.Misses + b.Misses,
		BytesRead: a.BytesRead + b.BytesRead, BytesWritten: a.BytesWritten + b.BytesWritten,
	}
}
