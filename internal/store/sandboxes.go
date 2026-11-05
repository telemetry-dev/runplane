package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

type Sandbox struct {
	ID            string        `json:"id"`
	PoolID        string        `json:"pool_id"`
	ServerID      string        `json:"server_id"`
	ScaleSetID    string        `json:"scale_set_id"`
	Mode          sandbox.Mode  `json:"mode"`
	State         sandbox.State `json:"state"`
	RunnerID      int64         `json:"runner_id"`
	RunnerName    string        `json:"runner_name"`
	IP            string        `json:"ip"`
	ImageDigest   string        `json:"image_digest"`
	RunnerVersion string        `json:"runner_version"`
	JobID         string        `json:"job_id"`
	Debug         bool          `json:"debug"`
	HoldUntil     *time.Time    `json:"hold_until"`
	VCPU          int           `json:"vcpu"`
	MemoryMB      int           `json:"memory_mb"`
	DiskGB        int           `json:"disk_gb"`
	CreatedAt     time.Time     `json:"created_at"`
	IdleAt        *time.Time    `json:"idle_at"`
	BusyAt        *time.Time    `json:"busy_at"`
	StoppedAt     *time.Time    `json:"stopped_at"`
	ExitReason    string        `json:"exit_reason"`
	Error         string        `json:"error"`
}

type SandboxFilter struct {
	PoolID   string
	ServerID string
	States   []sandbox.State // empty = all
}

// SandboxCount is the count of live sandboxes for one pool, server, and state.
type SandboxCount struct {
	PoolID   string
	ServerID string
	State    sandbox.State
	Count    int
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

const sandboxCols = `id, pool_id, server_id, scale_set_id, mode, state, runner_id, runner_name, ip, image_digest,
	runner_version, job_id, debug, hold_until, vcpu, memory_mb, disk_gb, created_at, idle_at, busy_at, stopped_at, exit_reason,
	error`

func (b *Sandbox) cols() []any {
	return []any{&b.ID, &b.PoolID, &b.ServerID, &b.ScaleSetID, &b.Mode, &b.State, &b.RunnerID, &b.RunnerName,
		&b.IP, &b.ImageDigest, &b.RunnerVersion, &b.JobID, &b.Debug, atPtr(&b.HoldUntil), &b.VCPU, &b.MemoryMB,
		&b.DiskGB, at(&b.CreatedAt), atPtr(&b.IdleAt), atPtr(&b.BusyAt), atPtr(&b.StoppedAt), &b.ExitReason, &b.Error}
}

func (s *Store) InsertSandbox(ctx context.Context, b Sandbox) error {
	return exec(ctx, s.w, "insert sandbox", insert("sandboxes", sandboxCols), b.cols()...)
}

// SetSandboxIP records the address of a sandbox of the server serverID. It
// returns ErrNotFound for a sandbox of another server, so a server cannot
// give its address to a sandbox that it does not run.
func (s *Store) SetSandboxIP(ctx context.Context, serverID, id, ip string) error {
	return exec1(ctx, s.w, "set sandbox ip", "UPDATE sandboxes SET ip = ? WHERE id = ? AND server_id = ?", ip, id, serverID)
}

func (s *Store) SetSandboxJob(ctx context.Context, id, jobID string) error {
	return exec1(ctx, s.w, "set sandbox job", "UPDATE sandboxes SET job_id = ? WHERE id = ?", jobID, id)
}

func (s *Store) SetSandboxHold(ctx context.Context, id string, until time.Time) error {
	return exec1(ctx, s.w, "set sandbox hold", "UPDATE sandboxes SET hold_until = ? WHERE id = ?", ms(until), id)
}

// SetSandboxDebug marks a sandbox that got a debug job after it started.
func (s *Store) SetSandboxDebug(ctx context.Context, id string) error {
	return exec1(ctx, s.w, "set sandbox debug", "UPDATE sandboxes SET debug = 1 WHERE id = ?", id)
}

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
	b, err := queryOne(ctx, s.w, "advance sandbox state", scanRow[Sandbox], `UPDATE sandboxes SET
			state = ?1,
			idle_at = CASE WHEN ?1 = 'idle' THEN coalesce(idle_at, ?2) ELSE idle_at END,
			busy_at = CASE WHEN ?1 = 'busy' THEN coalesce(busy_at, ?2) ELSE busy_at END,
			stopped_at = CASE WHEN ?1 IN ('gone', 'failed') THEN coalesce(stopped_at, ?2) ELSE stopped_at END,
			error = CASE WHEN ?1 = 'failed' AND error = '' THEN ?3 ELSE error END,
			exit_reason = CASE WHEN ?1 IN ('stopping', 'gone') AND exit_reason = '' THEN ?3 ELSE exit_reason END
		WHERE id = ?4 AND `+fmt.Sprintf(sandboxRank, "state")+` < `+fmt.Sprintf(sandboxRank, "?1")+`
		RETURNING `+sandboxCols, state, ms(t), reason, id)
	if err == nil {
		return b, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Sandbox{}, false, err
	}
	b, err = s.GetSandbox(ctx, id)
	return b, false, err
}

func (s *Store) GetSandbox(ctx context.Context, id string) (Sandbox, error) {
	return queryOne(ctx, s.r, "get sandbox", scanRow[Sandbox], "SELECT "+sandboxCols+" FROM sandboxes WHERE id = ?", id)
}

// GetSandboxByRunnerName returns the newest sandbox with this runner name.
func (s *Store) GetSandboxByRunnerName(ctx context.Context, name string) (Sandbox, error) {
	return queryOne(ctx, s.r, "get sandbox", scanRow[Sandbox],
		"SELECT "+sandboxCols+" FROM sandboxes WHERE runner_name = ? ORDER BY created_at DESC LIMIT 1", name)
}

// ListSandboxes returns the sandboxes that match f, newest first.
func (s *Store) ListSandboxes(ctx context.Context, f SandboxFilter) ([]Sandbox, error) {
	var conds []string
	var args []any
	if f.PoolID != "" {
		conds, args = append(conds, "pool_id = ?"), append(args, f.PoolID)
	}
	if f.ServerID != "" {
		conds, args = append(conds, "server_id = ?"), append(args, f.ServerID)
	}
	if len(f.States) > 0 {
		conds = append(conds, "state IN ("+placeholders(len(f.States))+")")
		for _, st := range f.States {
			args = append(args, st)
		}
	}
	return queryAll(ctx, s.r, "list sandboxes", scanRow[Sandbox],
		"SELECT "+sandboxCols+" FROM sandboxes"+where(conds)+" ORDER BY created_at DESC, id DESC", args...)
}

// ListLiveSandboxes returns the sandboxes that are not gone or failed.
func (s *Store) ListLiveSandboxes(ctx context.Context) ([]Sandbox, error) {
	return s.ListSandboxes(ctx, SandboxFilter{States: []sandbox.State{
		sandbox.StateStarting, sandbox.StateIdle, sandbox.StateBusy, sandbox.StateStopping,
	}})
}

// FindLiveSandboxes returns the sandboxes that are not gone or failed and
// whose ID starts with prefix, newest first.
func (s *Store) FindLiveSandboxes(ctx context.Context, prefix string) ([]Sandbox, error) {
	return queryAll(ctx, s.r, "find sandboxes", scanRow[Sandbox], "SELECT "+sandboxCols+` FROM sandboxes
		WHERE state IN ('starting', 'idle', 'busy', 'stopping') AND substr(id, 1, length(?1)) = ?1
		ORDER BY created_at DESC, id DESC`, prefix)
}

func (s *Store) CountLiveSandboxes(ctx context.Context) ([]SandboxCount, error) {
	return queryAll(ctx, s.r, "count sandboxes", func(sc scanner) (SandboxCount, error) {
		var c SandboxCount
		return c, sc.Scan(&c.PoolID, &c.ServerID, &c.State, &c.Count)
	}, `SELECT pool_id, server_id, state, count(*) FROM sandboxes
		WHERE state NOT IN ('gone', 'failed') GROUP BY pool_id, server_id, state`)
}

// DeleteSandboxesBefore deletes the gone and failed sandboxes that stopped before before.
func (s *Store) DeleteSandboxesBefore(ctx context.Context, before time.Time) error {
	return exec(ctx, s.w, "delete old sandboxes",
		"DELETE FROM sandboxes WHERE state IN ('gone', 'failed') AND stopped_at < ?", ms(before))
}

// The state term must stay equal to the WHERE clause of the sandboxes_live_ip
// index.
const sandboxJobQuery = `SELECT s.id, s.server_id, coalesce(j.repository, ''),
		coalesce((SELECT r.default_branch FROM repositories r WHERE r.full_name = j.repository LIMIT 1), ''),
		coalesce(j.workflow_ref, ''), coalesce(j.event, ''), coalesce(j.head_branch, ''), coalesce(j.pr_number, 0),
		coalesce(j.base_ref, '')
	FROM sandboxes s LEFT JOIN jobs j ON j.id = s.job_id
	WHERE s.state NOT IN ('gone', 'failed') AND `

func (b *SandboxJob) cols() []any {
	return []any{&b.SandboxID, &b.ServerID, &b.Repository, &b.DefaultBranch, &b.WorkflowRef, &b.Event, &b.HeadBranch,
		&b.PRNumber, &b.BaseRef}
}

// LiveSandboxJob returns the sandbox with this ID if it still operates.
func (s *Store) LiveSandboxJob(ctx context.Context, id string) (SandboxJob, error) {
	return queryOne(ctx, s.r, "get sandbox job", scanRow[SandboxJob], sandboxJobQuery+"s.id = ?", id)
}

// LiveSandboxJobByIP returns the newest operating sandbox of the server
// with this IP address.
func (s *Store) LiveSandboxJobByIP(ctx context.Context, serverID, ip string) (SandboxJob, error) {
	return queryOne(ctx, s.r, "get sandbox job", scanRow[SandboxJob],
		sandboxJobQuery+"s.ip = ? AND s.server_id = ? ORDER BY s.created_at DESC LIMIT 1", ip, serverID)
}
