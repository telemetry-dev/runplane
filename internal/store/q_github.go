package store

import (
	"context"
	"fmt"
	"time"
)

// SetScaleSetRemote records the GitHub side of a scale set. It does not
// touch the status, the statistics, or the poll time.
func (s *Store) SetScaleSetRemote(ctx context.Context, id string, githubID, runnerGroupID int64, name string) error {
	err := exec1(ctx, s.w, "UPDATE scale_sets SET github_id = ?, runner_group_id = ?, name = ?, updated_at = ? WHERE id = ?",
		githubID, runnerGroupID, name, ms(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set scale set remote: %w", err)
	}
	return nil
}

// TouchScaleSetPoll records a listener poll that brought no statistics.
func (s *Store) TouchScaleSetPoll(ctx context.Context, id string, polledAt time.Time) error {
	err := exec1(ctx, s.w, "UPDATE scale_sets SET last_poll_at = ? WHERE id = ?", ms(polledAt), id)
	if err != nil {
		return fmt.Errorf("touch scale set poll: %w", err)
	}
	return nil
}

// SetRunnerBundleDates writes the release time and the end date of a runner
// version. A new row gets the pending status. A nil time keeps the stored
// value, and the build columns never change, so this cannot undo the work of
// the bundle builder.
func (s *Store) SetRunnerBundleDates(ctx context.Context, version, arch string, releasedAt, endDate *time.Time) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO runner_bundles (version, arch, status, released_at, end_date, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (version, arch) DO UPDATE SET
			released_at = coalesce(excluded.released_at, released_at),
			end_date = coalesce(excluded.end_date, end_date)`,
		version, arch, BundlePending, msPtr(releasedAt), msPtr(endDate), ms(time.Now()))
	if err != nil {
		return fmt.Errorf("set runner bundle dates: %w", dbErr(err))
	}
	return nil
}
