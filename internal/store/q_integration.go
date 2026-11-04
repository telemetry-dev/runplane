package store

import (
	"context"
	"fmt"
	"time"
)

// HasOpenSetupCode reports if a setup code exists that is not used and not expired.
func (s *Store) HasOpenSetupCode(ctx context.Context, now time.Time) (bool, error) {
	var ok bool
	err := s.r.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM setup_codes WHERE used_at IS NULL AND expires_at > ?)", ms(now)).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("find setup code: %w", err)
	}
	return ok, nil
}

// FinishCacheEntry marks the upload complete with its size, and counts the
// finish as the last use.
func (s *Store) FinishCacheEntry(ctx context.Context, id string, size int64, at time.Time) error {
	err := exec1(ctx, s.w, "UPDATE cache_entries SET finalized = 1, size_bytes = ?, last_used_at = ? WHERE id = ?",
		size, ms(at), id)
	if err != nil {
		return fmt.Errorf("finish cache entry: %w", err)
	}
	return nil
}

// SandboxJob is a live sandbox with the repository and the run data of its job.
type SandboxJob struct {
	SandboxID string
	ServerID  string
	// Repository is empty if the sandbox has no job.
	Repository    string
	DefaultBranch string // empty if runplane does not know the repository
	WorkflowRef   string
	RunRef
}

// The state term must stay equal to the WHERE clause of the sandboxes_live_ip
// index.
const sandboxJobQuery = `SELECT s.id, s.server_id, coalesce(j.repository, ''),
		coalesce((SELECT r.default_branch FROM repositories r WHERE r.full_name = j.repository LIMIT 1), ''),
		coalesce(j.workflow_ref, ''), coalesce(j.event, ''), coalesce(j.head_branch, ''), coalesce(j.pr_number, 0),
		coalesce(j.base_ref, '')
	FROM sandboxes s LEFT JOIN jobs j ON j.id = s.job_id
	WHERE s.state NOT IN ('gone', 'failed') AND `

func scanSandboxJob(sc scanner) (SandboxJob, error) {
	var b SandboxJob
	err := sc.Scan(&b.SandboxID, &b.ServerID, &b.Repository, &b.DefaultBranch, &b.WorkflowRef, &b.Event,
		&b.HeadBranch, &b.PRNumber, &b.BaseRef)
	return b, err
}

// LiveSandboxJob returns the sandbox with this ID if it still operates.
func (s *Store) LiveSandboxJob(ctx context.Context, id string) (SandboxJob, error) {
	b, err := scanSandboxJob(s.r.QueryRowContext(ctx, sandboxJobQuery+"s.id = ?", id))
	if err != nil {
		return SandboxJob{}, fmt.Errorf("get sandbox job: %w", dbErr(err))
	}
	return b, nil
}

// LiveSandboxJobByIP returns the newest operating sandbox of the server
// with this IP address.
func (s *Store) LiveSandboxJobByIP(ctx context.Context, serverID, ip string) (SandboxJob, error) {
	b, err := scanSandboxJob(s.r.QueryRowContext(ctx,
		sandboxJobQuery+"s.ip = ? AND s.server_id = ? ORDER BY s.created_at DESC LIMIT 1", ip, serverID))
	if err != nil {
		return SandboxJob{}, fmt.Errorf("get sandbox job: %w", dbErr(err))
	}
	return b, nil
}
