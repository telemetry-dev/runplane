package cache

import (
	"context"
	"time"

	"github.com/runplane/runplane/internal/store"
)

// Prune deletes the counts older than store.SampleRetention.
func (s *Stats) Prune(ctx context.Context, now time.Time) error {
	return s.st.PruneCacheStats(ctx, now.Add(-store.SampleRetention))
}
