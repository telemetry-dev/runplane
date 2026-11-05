package cache

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/store"
)

// stats records the cache use of each repository in the cache_stats table.
// The server counts on request paths, so add only adds to counts in memory,
// and run writes them each minute.
type stats struct {
	st  Store
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	pending map[hourRepo]store.CacheCounts
}

type hourRepo struct {
	hour       time.Time
	repository string
}

func newStats(st Store, log *slog.Logger, now func() time.Time) *stats {
	return &stats{st: st, log: log, now: now, pending: map[hourRepo]store.CacheCounts{}}
}

func (s *stats) add(repository string, c store.CacheCounts) {
	k := hourRepo{s.now().UTC().Truncate(time.Hour), repository}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[k] = sum(s.pending[k], c)
}

// run writes the counts each minute, and a last time when ctx ends.
func (s *stats) run(ctx context.Context) {
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
func (s *stats) flush(ctx context.Context) {
	s.mu.Lock()
	batch := s.pending
	s.pending = map[hourRepo]store.CacheCounts{}
	s.mu.Unlock()
	for k, c := range batch {
		if err := s.st.AddCacheStats(ctx, k.hour, k.repository, c); err != nil {
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
