package fleet

import (
	"context"
	"errors"
	"time"

	"github.com/runplane/runplane/internal/store"
)

// Prune deletes the install tokens that expired at now and the host samples
// older than store.SampleRetention.
func (f *Fleet) Prune(ctx context.Context, now time.Time) error {
	return errors.Join(
		f.st.DeleteExpiredServerTokens(ctx, now),
		f.st.PruneServerStats(ctx, now.Add(-store.SampleRetention)),
	)
}
