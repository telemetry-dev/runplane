package store

import (
	"context"
	"time"
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

// UpdatePool writes all columns except id and created_at, and sets updated_at.
func (s *Store) UpdatePool(ctx context.Context, p Pool) error {
	return exec1(ctx, s.w, "update pool", `UPDATE pools SET name = ?, label = ?, arch = ?, vcpu = ?, memory_mb = ?,
			disk_gb = ?, image_id = ?, min_idle = ?, max_runners = ?, time_limit_minutes = ?, server_ids = ?, enabled = ?,
			updated_at = ?
		WHERE id = ?`,
		p.Name, p.Label, p.Arch, p.VCPU, p.MemoryMB, p.DiskGB, nullStr(p.ImageID), p.MinIdle, p.MaxRunners,
		p.TimeLimitMinutes, jsonCol(&p.ServerIDs), p.Enabled, ms(time.Now()), p.ID)
}

// DeletePool keeps the scale set rows: delete them on GitHub first.
func (s *Store) DeletePool(ctx context.Context, id string) error {
	return exec1(ctx, s.w, "delete pool", "DELETE FROM pools WHERE id = ?", id)
}
