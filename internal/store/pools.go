package store

import (
	"context"
	"database/sql"
	"fmt"
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

const poolCols = "id, name, label, arch, vcpu, memory_mb, disk_gb, image_id, min_idle, max_runners, time_limit_minutes, server_ids, enabled, created_at, updated_at"

func scanPool(sc scanner) (Pool, error) {
	var p Pool
	var image sql.NullString
	err := sc.Scan(&p.ID, &p.Name, &p.Label, &p.Arch, &p.VCPU, &p.MemoryMB, &p.DiskGB, &image, &p.MinIdle,
		&p.MaxRunners, &p.TimeLimitMinutes, jsonCol(&p.ServerIDs), &p.Enabled, at(&p.CreatedAt), at(&p.UpdatedAt))
	p.ImageID = image.String
	return p, err
}

// CreatePool returns ErrConflict if a pool has the same label.
func (s *Store) CreatePool(ctx context.Context, p Pool) error {
	_, err := s.w.ExecContext(ctx, "INSERT INTO pools ("+poolCols+") VALUES ("+placeholders(15)+")",
		p.ID, p.Name, p.Label, p.Arch, p.VCPU, p.MemoryMB, p.DiskGB, nullStr(p.ImageID), p.MinIdle, p.MaxRunners,
		p.TimeLimitMinutes, jsonArg(p.ServerIDs), p.Enabled, ms(p.CreatedAt), ms(p.UpdatedAt))
	if err != nil {
		return fmt.Errorf("create pool: %w", dbErr(err))
	}
	return nil
}

func (s *Store) GetPool(ctx context.Context, id string) (Pool, error) {
	p, err := scanPool(s.r.QueryRowContext(ctx, "SELECT "+poolCols+" FROM pools WHERE id = ?", id))
	if err != nil {
		return Pool{}, fmt.Errorf("get pool: %w", dbErr(err))
	}
	return p, nil
}

// GetPoolByLabel matches the label without regard to case, as GitHub does.
func (s *Store) GetPoolByLabel(ctx context.Context, label string) (Pool, error) {
	p, err := scanPool(s.r.QueryRowContext(ctx, "SELECT "+poolCols+" FROM pools WHERE label = ?", label))
	if err != nil {
		return Pool{}, fmt.Errorf("get pool: %w", dbErr(err))
	}
	return p, nil
}

func (s *Store) ListPools(ctx context.Context) ([]Pool, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+poolCols+" FROM pools ORDER BY created_at, id")
	list, err := scanAll(rows, err, scanPool)
	if err != nil {
		return nil, fmt.Errorf("list pools: %w", err)
	}
	return list, nil
}

// UpdatePool writes all columns except id and created_at, and sets updated_at.
func (s *Store) UpdatePool(ctx context.Context, p Pool) error {
	err := exec1(ctx, s.w, `UPDATE pools SET name = ?, label = ?, arch = ?, vcpu = ?, memory_mb = ?, disk_gb = ?,
			image_id = ?, min_idle = ?, max_runners = ?, time_limit_minutes = ?, server_ids = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		p.Name, p.Label, p.Arch, p.VCPU, p.MemoryMB, p.DiskGB, nullStr(p.ImageID), p.MinIdle, p.MaxRunners,
		p.TimeLimitMinutes, jsonArg(p.ServerIDs), p.Enabled, ms(time.Now()), p.ID)
	if err != nil {
		return fmt.Errorf("update pool: %w", err)
	}
	return nil
}

// DeletePool keeps the scale set rows: delete them on GitHub first.
func (s *Store) DeletePool(ctx context.Context, id string) error {
	if err := exec1(ctx, s.w, "DELETE FROM pools WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete pool: %w", err)
	}
	return nil
}
