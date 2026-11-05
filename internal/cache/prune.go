package cache

import (
	"context"
	"time"

	"github.com/runplane/runplane/internal/store"
)

// Prune deletes the hourly counts older than store.SampleRetention.
func (s *Server) Prune(ctx context.Context, now time.Time) error {
	return s.cfg.Store.PruneCacheStats(ctx, now.Add(-store.SampleRetention))
}
