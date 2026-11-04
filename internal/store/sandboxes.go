package store

import (
	"context"
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
	RunnerID      int64         `json:"runner_id"` // 0 until GitHub registers the runner
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

const sandboxCols = `id, pool_id, server_id, scale_set_id, mode, state, runner_id, runner_name, ip, image_digest,
	runner_version, job_id, debug, hold_until, vcpu, memory_mb, disk_gb, created_at, idle_at, busy_at, stopped_at, exit_reason,
	error`

func scanSandbox(sc scanner) (Sandbox, error) {
	var b Sandbox
	err := sc.Scan(&b.ID, &b.PoolID, &b.ServerID, &b.ScaleSetID, &b.Mode, &b.State, &b.RunnerID, &b.RunnerName,
		&b.IP, &b.ImageDigest, &b.RunnerVersion, &b.JobID, &b.Debug, atPtr(&b.HoldUntil), &b.VCPU, &b.MemoryMB,
		&b.DiskGB, at(&b.CreatedAt), atPtr(&b.IdleAt), atPtr(&b.BusyAt), atPtr(&b.StoppedAt), &b.ExitReason, &b.Error)
	return b, err
}

func (s *Store) InsertSandbox(ctx context.Context, b Sandbox) error {
	_, err := s.w.ExecContext(ctx, "INSERT INTO sandboxes ("+sandboxCols+") VALUES ("+placeholders(23)+")",
		b.ID, b.PoolID, b.ServerID, b.ScaleSetID, b.Mode, b.State, b.RunnerID, b.RunnerName, b.IP, b.ImageDigest,
		b.RunnerVersion, b.JobID, b.Debug, msPtr(b.HoldUntil), b.VCPU, b.MemoryMB, b.DiskGB, ms(b.CreatedAt),
		msPtr(b.IdleAt), msPtr(b.BusyAt), msPtr(b.StoppedAt), b.ExitReason, b.Error)
	if err != nil {
		return fmt.Errorf("insert sandbox: %w", dbErr(err))
	}
	return nil
}

// SetSandboxState records a state change at t and returns the row. The first
// change to idle, busy, or a done state sets idle_at, busy_at, or stopped_at.
// For failed, reason goes to error; for stopping and gone, to exit_reason.
// The first reason stays, and a done sandbox keeps its state.
func (s *Store) SetSandboxState(ctx context.Context, id string, state sandbox.State, reason string, t time.Time) (Sandbox, error) {
	b, err := scanSandbox(s.w.QueryRowContext(ctx, `UPDATE sandboxes SET
			state = CASE WHEN state IN ('gone', 'failed') THEN state ELSE ?1 END,
			idle_at = CASE WHEN ?1 = 'idle' THEN coalesce(idle_at, ?2) ELSE idle_at END,
			busy_at = CASE WHEN ?1 = 'busy' THEN coalesce(busy_at, ?2) ELSE busy_at END,
			stopped_at = CASE WHEN ?1 IN ('gone', 'failed') THEN coalesce(stopped_at, ?2) ELSE stopped_at END,
			error = CASE WHEN ?1 = 'failed' AND error = '' THEN ?3 ELSE error END,
			exit_reason = CASE WHEN ?1 IN ('stopping', 'gone') AND exit_reason = '' THEN ?3 ELSE exit_reason END
		WHERE id = ?4
		RETURNING `+sandboxCols, state, ms(t), reason, id))
	if err != nil {
		return Sandbox{}, fmt.Errorf("set sandbox state: %w", dbErr(err))
	}
	return b, nil
}

// SetSandboxRunner records the runner that GitHub registered for the sandbox.
func (s *Store) SetSandboxRunner(ctx context.Context, id string, runnerID int64, runnerName string) error {
	err := exec1(ctx, s.w, "UPDATE sandboxes SET runner_id = ?, runner_name = ? WHERE id = ?", runnerID, runnerName, id)
	if err != nil {
		return fmt.Errorf("set sandbox runner: %w", err)
	}
	return nil
}

// SetSandboxIP records the address of a sandbox of the server serverID. It
// returns ErrNotFound for a sandbox of another server, so a server cannot
// give its address to a sandbox that it does not run.
func (s *Store) SetSandboxIP(ctx context.Context, serverID, id, ip string) error {
	if err := exec1(ctx, s.w, "UPDATE sandboxes SET ip = ? WHERE id = ? AND server_id = ?", ip, id, serverID); err != nil {
		return fmt.Errorf("set sandbox ip: %w", err)
	}
	return nil
}

func (s *Store) SetSandboxJob(ctx context.Context, id, jobID string) error {
	if err := exec1(ctx, s.w, "UPDATE sandboxes SET job_id = ? WHERE id = ?", jobID, id); err != nil {
		return fmt.Errorf("set sandbox job: %w", err)
	}
	return nil
}

func (s *Store) SetSandboxHold(ctx context.Context, id string, until time.Time) error {
	if err := exec1(ctx, s.w, "UPDATE sandboxes SET hold_until = ? WHERE id = ?", ms(until), id); err != nil {
		return fmt.Errorf("set sandbox hold: %w", err)
	}
	return nil
}

func (s *Store) GetSandbox(ctx context.Context, id string) (Sandbox, error) {
	b, err := scanSandbox(s.r.QueryRowContext(ctx, "SELECT "+sandboxCols+" FROM sandboxes WHERE id = ?", id))
	if err != nil {
		return Sandbox{}, fmt.Errorf("get sandbox: %w", dbErr(err))
	}
	return b, nil
}

// GetSandboxByRunnerName returns the newest sandbox with this runner name.
func (s *Store) GetSandboxByRunnerName(ctx context.Context, name string) (Sandbox, error) {
	b, err := scanSandbox(s.r.QueryRowContext(ctx,
		"SELECT "+sandboxCols+" FROM sandboxes WHERE runner_name = ? ORDER BY created_at DESC LIMIT 1", name))
	if err != nil {
		return Sandbox{}, fmt.Errorf("get sandbox: %w", dbErr(err))
	}
	return b, nil
}

// ListSandboxes returns the sandboxes that match f, newest first.
func (s *Store) ListSandboxes(ctx context.Context, f SandboxFilter) ([]Sandbox, error) {
	var where []string
	var args []any
	if f.PoolID != "" {
		where, args = append(where, "pool_id = ?"), append(args, f.PoolID)
	}
	if f.ServerID != "" {
		where, args = append(where, "server_id = ?"), append(args, f.ServerID)
	}
	if len(f.States) > 0 {
		where = append(where, "state IN ("+placeholders(len(f.States))+")")
		for _, st := range f.States {
			args = append(args, st)
		}
	}
	q := "SELECT " + sandboxCols + " FROM sandboxes"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.r.QueryContext(ctx, q+" ORDER BY created_at DESC, id DESC", args...)
	list, err := scanAll(rows, err, scanSandbox)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	return list, nil
}

// ListLiveSandboxes returns the sandboxes that are not gone or failed.
func (s *Store) ListLiveSandboxes(ctx context.Context) ([]Sandbox, error) {
	return s.ListSandboxes(ctx, SandboxFilter{States: []sandbox.State{
		sandbox.StateStarting, sandbox.StateIdle, sandbox.StateBusy, sandbox.StateStopping,
	}})
}

// SandboxCount is the count of live sandboxes for one pool, server, and state.
type SandboxCount struct {
	PoolID   string
	ServerID string
	State    sandbox.State
	Count    int
}

func (s *Store) CountLiveSandboxes(ctx context.Context) ([]SandboxCount, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT pool_id, server_id, state, count(*) FROM sandboxes
		WHERE state NOT IN ('gone', 'failed') GROUP BY pool_id, server_id, state`)
	list, err := scanAll(rows, err, func(sc scanner) (SandboxCount, error) {
		var c SandboxCount
		return c, sc.Scan(&c.PoolID, &c.ServerID, &c.State, &c.Count)
	})
	if err != nil {
		return nil, fmt.Errorf("count sandboxes: %w", err)
	}
	return list, nil
}
