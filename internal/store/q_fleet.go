package store

import (
	"context"
	"fmt"
	"time"
)

// SetServerStatusIf sets the status and the detail of a server only while
// its status is from. It reports whether the row changed.
func (s *Store) SetServerStatusIf(ctx context.Context, id, from, to, detail string) (bool, error) {
	res, err := s.w.ExecContext(ctx, `UPDATE servers SET status = ?, status_detail = ?, updated_at = ?
		WHERE id = ? AND status = ?`, to, detail, ms(time.Now()), id, from)
	if err != nil {
		return false, fmt.Errorf("set server status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set server status: %w", err)
	}
	return n > 0, nil
}

// DeleteServerTokens deletes the unused install tokens of a server.
func (s *Store) DeleteServerTokens(ctx context.Context, serverID string) error {
	if _, err := s.w.ExecContext(ctx, "DELETE FROM server_tokens WHERE server_id = ? AND used_at IS NULL", serverID); err != nil {
		return fmt.Errorf("delete server tokens: %w", err)
	}
	return nil
}

// SetRunnerBundleBuild records the result of a bundle build on one server:
// sha256, patched, status, path, and error. A failure does not change a
// bundle that is ready, because it can be ready on other servers. The dates
// stay.
func (s *Store) SetRunnerBundleBuild(ctx context.Context, b RunnerBundle) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO runner_bundles (version, arch, sha256, patched, status, path, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (version, arch) DO UPDATE SET sha256 = excluded.sha256, patched = excluded.patched,
			status = excluded.status, path = excluded.path, error = excluded.error
		WHERE excluded.status = 'ready' OR runner_bundles.status != 'ready'`,
		b.Version, b.Arch, b.SHA256, b.Patched, b.Status, b.Path, b.Error, ms(time.Now()))
	if err != nil {
		return fmt.Errorf("set runner bundle build: %w", dbErr(err))
	}
	return nil
}
