package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

// sandboxRank orders the sandbox states. gone and failed share the last rank.
const sandboxRank = `CASE %s WHEN 'starting' THEN 0 WHEN 'idle' THEN 1 WHEN 'busy' THEN 2 WHEN 'stopping' THEN 3 ELSE 4 END`

// AdvanceSandboxState records a change at t to a state that comes after the
// current one, and returns the row and true. The first change to idle, busy,
// or a done state sets idle_at, busy_at, or stopped_at. For failed, reason
// goes to error; for stopping and gone, to exit_reason. The first reason
// stays. It returns the row and false, with no change, if the sandbox is
// already in that state or a later one. Events can arrive out of order, and
// a late "listening" must not make a busy sandbox idle again.
func (s *Store) AdvanceSandboxState(ctx context.Context, id string, state sandbox.State, reason string, t time.Time) (Sandbox, bool, error) {
	b, err := scanSandbox(s.w.QueryRowContext(ctx, `UPDATE sandboxes SET
			state = ?1,
			idle_at = CASE WHEN ?1 = 'idle' THEN coalesce(idle_at, ?2) ELSE idle_at END,
			busy_at = CASE WHEN ?1 = 'busy' THEN coalesce(busy_at, ?2) ELSE busy_at END,
			stopped_at = CASE WHEN ?1 IN ('gone', 'failed') THEN coalesce(stopped_at, ?2) ELSE stopped_at END,
			error = CASE WHEN ?1 = 'failed' AND error = '' THEN ?3 ELSE error END,
			exit_reason = CASE WHEN ?1 IN ('stopping', 'gone') AND exit_reason = '' THEN ?3 ELSE exit_reason END
		WHERE id = ?4 AND `+fmt.Sprintf(sandboxRank, "state")+` < `+fmt.Sprintf(sandboxRank, "?1")+`
		RETURNING `+sandboxCols, state, ms(t), reason, id))
	if err == nil {
		return b, true, nil
	}
	if err = dbErr(err); !errors.Is(err, ErrNotFound) {
		return Sandbox{}, false, fmt.Errorf("advance sandbox state: %w", err)
	}
	b, err = s.GetSandbox(ctx, id)
	return b, false, err
}

// SetSandboxDebug marks a sandbox that got a debug job after it started.
func (s *Store) SetSandboxDebug(ctx context.Context, id string) error {
	if err := exec1(ctx, s.w, "UPDATE sandboxes SET debug = 1 WHERE id = ?", id); err != nil {
		return fmt.Errorf("set sandbox debug: %w", err)
	}
	return nil
}

// CountPoolJobs counts the jobs of a pool queued since since that wait for a
// runner: queued (not rejected) and assigned.
func (s *Store) CountPoolJobs(ctx context.Context, poolID string, since time.Time) (queued, assigned int, err error) {
	err = s.r.QueryRowContext(ctx, `SELECT
			coalesce(sum(state = 'queued' AND reject_reason = ''), 0), coalesce(sum(state = 'assigned'), 0)
		FROM jobs WHERE pool_id = ? AND queued_at >= ?`, poolID, ms(since)).Scan(&queued, &assigned)
	if err != nil {
		return 0, 0, fmt.Errorf("count pool jobs: %w", err)
	}
	return queued, assigned, nil
}

// NotRunReason is the reject reason of a job that ExpireQueuedJobs closed.
const NotRunReason = "runplane did not run this job."

// ExpireQueuedJobs completes the jobs that are queued since before, as
// cancelled at t: runplane did not take them. A job without a reject reason
// gets NotRunReason. It returns the changed jobs.
func (s *Store) ExpireQueuedJobs(ctx context.Context, before, t time.Time) ([]Job, error) {
	rows, err := s.w.QueryContext(ctx, `UPDATE jobs SET state = 'completed', result = 'cancelled', finished_at = ?,
			reject_reason = CASE WHEN reject_reason = '' THEN ? ELSE reject_reason END
		WHERE state = 'queued' AND queued_at < ?
		RETURNING `+jobCols, ms(t), NotRunReason, ms(before))
	list, err := scanAll(rows, err, scanJob)
	if err != nil {
		return nil, fmt.Errorf("expire queued jobs: %w", err)
	}
	return list, nil
}
