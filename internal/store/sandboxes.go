package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// LinkJobSandbox records that the sandbox runs the job: the job gets the
// sandbox and its server, and the sandbox gets the job. It returns the two
// rows after the change, or ErrNotFound if one of them does not exist.
func (s *Store) LinkJobSandbox(ctx context.Context, jobID, sandboxID string) (Job, Sandbox, error) {
	var j Job
	var b Sandbox
	err := s.tx(ctx, "link job and sandbox", func(tx *sql.Tx) error {
		var err error
		b, err = queryOne(ctx, tx, "set sandbox job", scanRow[Sandbox],
			"UPDATE sandboxes SET job_id = ? WHERE id = ? RETURNING "+sandboxCols, jobID, sandboxID)
		if err != nil {
			return err
		}
		j, err = queryOne(ctx, tx, "set job sandbox", scanRow[Job],
			"UPDATE jobs SET sandbox_id = ?, server_id = ? WHERE id = ? RETURNING "+jobCols, b.ID, b.ServerID, jobID)
		return err
	})
	return j, b, err
}

func (s *Store) SetSandboxHold(ctx context.Context, id string, until time.Time) error {
	return exec1(ctx, s.w, "set sandbox hold", "UPDATE sandboxes SET hold_until = ? WHERE id = ?", ms(until), id)
}

// SetSandboxDebug marks a sandbox that got a debug job after it started.
func (s *Store) SetSandboxDebug(ctx context.Context, id string) error {
	return exec1(ctx, s.w, "set sandbox debug", "UPDATE sandboxes SET debug = 1 WHERE id = ?", id)
}

// The SQL terms of the sandbox states come from the order in package
// sandbox. A query that must use the partial index sandboxes_live_ip has
// liveState as its state term, the WHERE clause of the index.
var (
	liveState = "state NOT IN (" + stateList(sandbox.DoneStates()) + ")"
	doneState = "state IN (" + stateList(sandbox.DoneStates()) + ")"
)

// stateList returns "'a', 'b'" for the states.
func stateList(list []sandbox.State) string {
	quoted := make([]string, len(list))
	for i, st := range list {
		quoted[i] = "'" + string(st) + "'"
	}
	return strings.Join(quoted, ", ")
}

// rankOf returns a CASE term with the sandbox.State Rank of the state in expr.
func rankOf(expr string) string {
	var b strings.Builder
	b.WriteString("CASE " + expr)
	for _, st := range append(sandbox.LiveStates(), sandbox.DoneStates()...) {
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", st, st.Rank())
	}
	b.WriteString(" ELSE -1 END")
	return b.String()
}

// AdvanceSandboxState records a change at t to a state that comes after the
// current one, and returns the row and true. The first change to idle, busy,
// or a done state sets idle_at, busy_at, or stopped_at. For failed, reason
// goes to error; for stopping and gone, to exit_reason. The first reason
// stays. It returns the row and false, with no change, if the sandbox is
// already in that state or a later one. Events can arrive out of order, and
// a late "listening" must not make a busy sandbox idle again.
func (s *Store) AdvanceSandboxState(ctx context.Context, id string, state sandbox.State, reason string, t time.Time) (Sandbox, bool, error) {
	if state.Rank() < 0 {
		return Sandbox{}, false, fmt.Errorf("advance sandbox state: unknown state %q", state)
	}
	b, err := queryOne(ctx, s.w, "advance sandbox state", scanRow[Sandbox], `UPDATE sandboxes SET
			state = ?1,
			idle_at = CASE WHEN ?1 = 'idle' THEN coalesce(idle_at, ?2) ELSE idle_at END,
			busy_at = CASE WHEN ?1 = 'busy' THEN coalesce(busy_at, ?2) ELSE busy_at END,
			stopped_at = CASE WHEN ?1 IN (`+stateList(sandbox.DoneStates())+`) THEN coalesce(stopped_at, ?2) ELSE stopped_at END,
			error = CASE WHEN ?1 = 'failed' AND error = '' THEN ?3 ELSE error END,
			exit_reason = CASE WHEN ?1 IN ('stopping', 'gone') AND exit_reason = '' THEN ?3 ELSE exit_reason END
		WHERE id = ?4 AND `+rankOf("state")+` < `+rankOf("?1")+`
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

func (s *Store) ListLiveSandboxes(ctx context.Context) ([]Sandbox, error) {
	return s.ListSandboxes(ctx, SandboxFilter{States: sandbox.LiveStates()})
}

// FindLiveSandboxes returns the live sandboxes whose ID starts with prefix,
// newest first.
func (s *Store) FindLiveSandboxes(ctx context.Context, prefix string) ([]Sandbox, error) {
	return queryAll(ctx, s.r, "find sandboxes", scanRow[Sandbox], "SELECT "+sandboxCols+` FROM sandboxes
		WHERE `+liveState+` AND substr(id, 1, length(?1)) = ?1
		ORDER BY created_at DESC, id DESC`, prefix)
}

// DeleteSandboxesBefore deletes the done sandboxes that stopped before before.
func (s *Store) DeleteSandboxesBefore(ctx context.Context, before time.Time) error {
	return exec(ctx, s.w, "delete old sandboxes",
		"DELETE FROM sandboxes WHERE "+doneState+" AND stopped_at < ?", ms(before))
}

var sandboxJobQuery = `SELECT s.id, s.server_id, coalesce(j.repository, ''),
		coalesce((SELECT r.default_branch FROM repositories r WHERE r.full_name = j.repository LIMIT 1), ''),
		coalesce(j.workflow_ref, ''), coalesce(j.event, ''), coalesce(j.head_branch, ''), coalesce(j.pr_number, 0),
		coalesce(j.base_ref, '')
	FROM sandboxes s LEFT JOIN jobs j ON j.id = s.job_id
	WHERE s.` + liveState + ` AND `

func (b *SandboxJob) cols() []any {
	return []any{&b.SandboxID, &b.ServerID, &b.Repository, &b.DefaultBranch, &b.WorkflowRef, &b.Event, &b.HeadBranch,
		&b.PRNumber, &b.BaseRef}
}

// LiveSandboxJob returns the sandbox with this ID if it still operates.
func (s *Store) LiveSandboxJob(ctx context.Context, id string) (SandboxJob, error) {
	return queryOne(ctx, s.r, "get sandbox job", scanRow[SandboxJob], sandboxJobQuery+"s.id = ?", id)
}
