package backup

import (
	"context"
	"time"
)

// Daily calls job each day at 03:00 UTC until ctx ends.
func Daily(ctx context.Context, job func(context.Context)) {
	for {
		t := time.NewTimer(time.Until(nextRun(time.Now())))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			job(ctx)
		}
	}
}

// nextRun returns the first 03:00 UTC after now.
func nextRun(now time.Time) time.Time {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
