package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DeleteJobsBefore deletes the jobs queued before before, and their
// samples. It returns the IDs of the deleted jobs, so the caller can remove
// their log files.
func (s *Store) DeleteJobsBefore(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := s.w.QueryContext(ctx, "DELETE FROM jobs WHERE queued_at < ? RETURNING id", ms(before))
	list, err := scanAll(rows, err, scanString)
	if err != nil {
		return nil, fmt.Errorf("delete old jobs: %w", err)
	}
	return list, nil
}

// ExpireJobLogs sets log_state to expired for the stored logs of the jobs
// that finished before before. It returns the IDs of these jobs, so the
// caller can remove their log files.
func (s *Store) ExpireJobLogs(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := s.w.QueryContext(ctx, `UPDATE jobs SET log_state = 'expired'
		WHERE log_state = 'stored' AND finished_at < ? RETURNING id`, ms(before))
	list, err := scanAll(rows, err, scanString)
	if err != nil {
		return nil, fmt.Errorf("expire job logs: %w", err)
	}
	return list, nil
}

// DeleteSandboxesBefore deletes the gone and failed sandboxes that stopped before before.
func (s *Store) DeleteSandboxesBefore(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.w.ExecContext(ctx,
		"DELETE FROM sandboxes WHERE state IN ('gone', 'failed') AND stopped_at < ?", ms(before))
	if err != nil {
		return 0, fmt.Errorf("delete old sandboxes: %w", err)
	}
	return res.RowsAffected()
}

// DeleteExpired deletes the sessions, manifest states, setup codes, invites,
// and server tokens that expired at now. It keeps the used setup codes,
// accepted invites, and used server tokens as a record.
func (s *Store) DeleteExpired(ctx context.Context, now time.Time) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			"DELETE FROM sessions WHERE expires_at <= ?",
			"DELETE FROM manifest_states WHERE expires_at <= ?",
			"DELETE FROM setup_codes WHERE used_at IS NULL AND expires_at <= ?",
			"DELETE FROM invites WHERE accepted_at IS NULL AND expires_at <= ?",
			"DELETE FROM server_tokens WHERE used_at IS NULL AND expires_at <= ?",
		} {
			if _, err := tx.ExecContext(ctx, q, ms(now)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete expired rows: %w", err)
	}
	return nil
}

func scanString(sc scanner) (string, error) {
	var v string
	return v, sc.Scan(&v)
}
