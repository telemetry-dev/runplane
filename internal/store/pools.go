package store

import (
	"context"
	"slices"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

// Pool is a runner label with a sandbox size and an image.
type Pool struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Label            string    `json:"label"`
	Arch             string    `json:"arch"` // x64 | arm64
	VCPU             int       `json:"vcpu"`
	MemoryMB         int       `json:"memory_mb"`
	DiskGB           int       `json:"disk_gb"`
	ImageID          string    `json:"image_id"` // empty = the default image
	MinIdle          int       `json:"min_idle"`
	MaxRunners       int       `json:"max_runners"`
	TimeLimitMinutes int       `json:"time_limit_minutes"` // 0 = default_time_limit_minutes
	ServerIDs        []string  `json:"server_ids"`         // empty = all servers
	Enabled          bool      `json:"enabled"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// CanUse tells if the sandboxes of the pool can run on the server. An empty
// ServerIDs is all servers of the arch.
func (p Pool) CanUse(serverID, arch string) bool {
	return p.Arch == arch && (len(p.ServerIDs) == 0 || slices.Contains(p.ServerIDs, serverID))
}

const poolCols = `id, name, label, arch, vcpu, memory_mb, disk_gb, image_id, min_idle, max_runners, time_limit_minutes,
	server_ids, enabled, created_at, updated_at`

func (p *Pool) cols() []any {
	return []any{&p.ID, &p.Name, &p.Label, &p.Arch, &p.VCPU, &p.MemoryMB, &p.DiskGB, text(&p.ImageID), &p.MinIdle,
		&p.MaxRunners, &p.TimeLimitMinutes, jsonCol(&p.ServerIDs), &p.Enabled, at(&p.CreatedAt), at(&p.UpdatedAt)}
}

// CreatePool returns ErrConflict if a pool has the same label.
func (s *Store) CreatePool(ctx context.Context, p Pool) error {
	return exec(ctx, s.w, "create pool", insert("pools", poolCols), p.cols()...)
}

func (s *Store) GetPool(ctx context.Context, id string) (Pool, error) {
	return queryOne(ctx, s.r, "get pool", scanRow[Pool], "SELECT "+poolCols+" FROM pools WHERE id = ?", id)
}

func (s *Store) ListPools(ctx context.Context) ([]Pool, error) {
	return queryAll(ctx, s.r, "list pools", scanRow[Pool], "SELECT "+poolCols+" FROM pools ORDER BY created_at, id")
}

// UpdatePool writes all columns except id and created_at.
func (s *Store) UpdatePool(ctx context.Context, p Pool) error {
	return exec1(ctx, s.w, "update pool", `UPDATE pools SET name = ?, label = ?, arch = ?, vcpu = ?, memory_mb = ?,
			disk_gb = ?, image_id = ?, min_idle = ?, max_runners = ?, time_limit_minutes = ?, server_ids = ?, enabled = ?,
			updated_at = ?
		WHERE id = ?`,
		p.Name, p.Label, p.Arch, p.VCPU, p.MemoryMB, p.DiskGB, nullStr(p.ImageID), p.MinIdle, p.MaxRunners,
		p.TimeLimitMinutes, jsonCol(&p.ServerIDs), p.Enabled, ms(p.UpdatedAt), p.ID)
}

// DeletePool keeps the scale set rows. The GitHub code sees that the pool is
// gone, deletes its scale sets on GitHub, and then deletes their rows.
func (s *Store) DeletePool(ctx context.Context, id string) error {
	return exec1(ctx, s.w, "delete pool", "DELETE FROM pools WHERE id = ?", id)
}

// PoolCounts are the runners of a pool by state, and its jobs that wait for
// a runner. The API pool and the pool.stats event have them.
type PoolCounts struct {
	Idle     int `json:"idle" doc:"Live count of idle sandboxes."`
	Busy     int `json:"busy" doc:"Live count of sandboxes that run a job."`
	Starting int `json:"starting" doc:"Live count of sandboxes that start."`
	Queued   int `json:"queued" doc:"Jobs that wait for a runner."`
	Assigned int `json:"assigned" doc:"Of queued, the jobs that GitHub assigned to the scale set of the pool."`
}

// PoolCounts returns the counts of each pool by pool ID. A pool with no
// runners and no waiting jobs has no entry. Queued counts the jobs that wait
// for a runner: their status is queued, and they have no reject reason.
// Assigned counts the jobs of Queued that GitHub assigned to the scale set.
func (s *Store) PoolCounts(ctx context.Context) (map[string]PoolCounts, error) {
	const op = "count pool runners and jobs"
	out := map[string]PoolCounts{}
	type count struct {
		poolID, state string
		n             int
	}
	scan := func(sc scanner) (count, error) {
		var c count
		err := sc.Scan(&c.poolID, &c.state, &c.n)
		return c, err
	}
	runners, err := queryAll(ctx, s.r, op, scan, `SELECT pool_id, state, count(*) FROM sandboxes WHERE `+liveState+`
		GROUP BY pool_id, state`)
	if err != nil {
		return nil, err
	}
	jobs, err := queryAll(ctx, s.r, op, scan, `SELECT pool_id, state, count(*) FROM jobs
		WHERE `+stateTerm(statusStates[StatusQueued])+` AND reject_reason = '' GROUP BY pool_id, state`)
	if err != nil {
		return nil, err
	}
	for _, c := range runners {
		p := out[c.poolID]
		switch sandbox.State(c.state) {
		case sandbox.StateIdle:
			p.Idle += c.n
		case sandbox.StateBusy:
			p.Busy += c.n
		case sandbox.StateStarting:
			p.Starting += c.n
		}
		out[c.poolID] = p
	}
	for _, c := range jobs {
		p := out[c.poolID]
		p.Queued += c.n
		if c.state == JobAssigned {
			p.Assigned += c.n
		}
		out[c.poolID] = p
	}
	return out, nil
}
