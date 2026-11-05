package auth

import (
	"context"
	"time"
)

// Prune deletes the sessions, setup codes, and invites that expired at now.
func (s *Service) Prune(ctx context.Context, now time.Time) error {
	return s.store.DeleteExpiredAuth(ctx, now)
}
