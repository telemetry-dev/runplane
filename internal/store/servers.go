package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

// Server kinds and statuses.
const (
	ServerLocal = "local"
	ServerAgent = "agent"

	ServerOnline      = "online"
	ServerInstalling  = "installing"
	ServerUnreachable = "unreachable"
	ServerDraining    = "draining"
	ServerDisabled    = "disabled"
)

type Server struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Kind         string     `json:"kind"`
	Status       string     `json:"status"`
	StatusDetail string     `json:"status_detail"`
	ServerFacts             // filled by the host checks of the server
	SecretHash   string     `json:"-"` // agents only
	LastSeenAt   *time.Time `json:"last_seen_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ServerFacts are the host facts that a server reports.
type ServerFacts struct {
	Address            string       `json:"address"`
	Provider           string       `json:"provider"`
	OS                 string       `json:"os"`
	Kernel             string       `json:"kernel"`
	Arch               string       `json:"arch"` // x64 | arm64
	Mode               sandbox.Mode `json:"mode"`
	VCPU               int          `json:"vcpu"`
	MemoryMB           int          `json:"memory_mb"`
	DiskBytes          int64        `json:"disk_bytes"`
	DiskKind           string       `json:"disk_kind"`
	KVM                bool         `json:"kvm"`
	DockerVersion      string       `json:"docker_version"`
	SysboxVersion      string       `json:"sysbox_version"`
	FirecrackerVersion string       `json:"firecracker_version"`
	Version            string       `json:"version"` // runplane version
}

type ServerToken struct {
	ID        string
	TokenHash string
	Name      string // name of the new server
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	ServerID  string // the server of the token: made before an SSH install, or the one that used it
}

// ServerStat is one host sample.
type ServerStat struct {
	ServerID      string    `json:"server_id"`
	At            time.Time `json:"at"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryPercent float64   `json:"memory_percent"`
	DiskPercent   float64   `json:"disk_percent"`
}

const serverCols = `id, name, kind, status, status_detail, address, provider, os, kernel, arch, mode, vcpu, memory_mb,
	disk_bytes, disk_kind, kvm, docker_version, sysbox_version, firecracker_version, version, secret_hash,
	last_seen_at, created_at, updated_at`

func scanServer(sc scanner) (Server, error) {
	var v Server
	var secret sql.NullString
	f := &v.ServerFacts
	err := sc.Scan(&v.ID, &v.Name, &v.Kind, &v.Status, &v.StatusDetail, &f.Address, &f.Provider, &f.OS, &f.Kernel,
		&f.Arch, &f.Mode, &f.VCPU, &f.MemoryMB, &f.DiskBytes, &f.DiskKind, &f.KVM, &f.DockerVersion, &f.SysboxVersion,
		&f.FirecrackerVersion, &f.Version, &secret, atPtr(&v.LastSeenAt), at(&v.CreatedAt), at(&v.UpdatedAt))
	v.SecretHash = secret.String
	return v, err
}

func insertServer(ctx context.Context, q querier, v Server) error {
	f := v.ServerFacts
	_, err := q.ExecContext(ctx, "INSERT INTO servers ("+serverCols+") VALUES ("+placeholders(24)+")",
		v.ID, v.Name, v.Kind, v.Status, v.StatusDetail, f.Address, f.Provider, f.OS, f.Kernel, f.Arch, f.Mode,
		f.VCPU, f.MemoryMB, f.DiskBytes, f.DiskKind, f.KVM, f.DockerVersion, f.SysboxVersion, f.FirecrackerVersion,
		f.Version, nullStr(v.SecretHash), msPtr(v.LastSeenAt), ms(v.CreatedAt), ms(v.UpdatedAt))
	return dbErr(err)
}

// CreateServer returns ErrConflict for a second local server.
func (s *Store) CreateServer(ctx context.Context, v Server) error {
	if err := insertServer(ctx, s.w, v); err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	return nil
}

func (s *Store) GetServer(ctx context.Context, id string) (Server, error) {
	return s.getServer(ctx, "id = ?", id)
}

// GetLocalServer returns the primary, or ErrNotFound before the first start.
func (s *Store) GetLocalServer(ctx context.Context) (Server, error) {
	return s.getServer(ctx, "kind = ?", ServerLocal)
}

func (s *Store) GetServerBySecretHash(ctx context.Context, hash string) (Server, error) {
	return s.getServer(ctx, "secret_hash = ?", hash)
}

func (s *Store) getServer(ctx context.Context, where string, arg any) (Server, error) {
	v, err := scanServer(s.r.QueryRowContext(ctx, "SELECT "+serverCols+" FROM servers WHERE "+where, arg))
	if err != nil {
		return Server{}, fmt.Errorf("get server: %w", dbErr(err))
	}
	return v, nil
}

// ListServers returns the primary first.
func (s *Store) ListServers(ctx context.Context) ([]Server, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+serverCols+" FROM servers ORDER BY kind = 'local' DESC, name, id")
	list, err := scanAll(rows, err, scanServer)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	return list, nil
}

func (s *Store) SetServerName(ctx context.Context, id, name string) error {
	err := exec1(ctx, s.w, "UPDATE servers SET name = ?, updated_at = ? WHERE id = ?", name, ms(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set server name: %w", err)
	}
	return nil
}

func (s *Store) UpdateServerFacts(ctx context.Context, id string, f ServerFacts) error {
	err := exec1(ctx, s.w, `UPDATE servers SET address = ?, provider = ?, os = ?, kernel = ?, arch = ?, mode = ?,
			vcpu = ?, memory_mb = ?, disk_bytes = ?, disk_kind = ?, kvm = ?, docker_version = ?, sysbox_version = ?,
			firecracker_version = ?, version = ?, updated_at = ?
		WHERE id = ?`,
		f.Address, f.Provider, f.OS, f.Kernel, f.Arch, f.Mode, f.VCPU, f.MemoryMB, f.DiskBytes, f.DiskKind, f.KVM,
		f.DockerVersion, f.SysboxVersion, f.FirecrackerVersion, f.Version, ms(time.Now()), id)
	if err != nil {
		return fmt.Errorf("update server facts: %w", err)
	}
	return nil
}

func (s *Store) SetServerStatus(ctx context.Context, id, status, detail string) error {
	err := exec1(ctx, s.w, "UPDATE servers SET status = ?, status_detail = ?, updated_at = ? WHERE id = ?",
		status, detail, ms(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set server status: %w", err)
	}
	return nil
}

func (s *Store) TouchServer(ctx context.Context, id string, seen time.Time) error {
	if err := exec1(ctx, s.w, "UPDATE servers SET last_seen_at = ? WHERE id = ?", ms(seen), id); err != nil {
		return fmt.Errorf("touch server: %w", err)
	}
	return nil
}

func (s *Store) SetServerSecretHash(ctx context.Context, id, hash string) error {
	err := exec1(ctx, s.w, "UPDATE servers SET secret_hash = ?, updated_at = ? WHERE id = ?",
		nullStr(hash), ms(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set server secret: %w", err)
	}
	return nil
}

// DeleteServer also deletes its image builds and host samples, and removes
// its ID from the server_ids of each pool. A pool that loses its last server
// becomes disabled, because empty server_ids means all servers.
func (s *Store) DeleteServer(ctx context.Context, id string) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := exec1(ctx, tx, "DELETE FROM servers WHERE id = ?", id); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT p.id, p.server_ids FROM pools p
			WHERE EXISTS (SELECT 1 FROM json_each(p.server_ids) WHERE value = ?)`, id)
		type pool struct {
			id        string
			serverIDs []string
		}
		pools, err := scanAll(rows, err, func(sc scanner) (pool, error) {
			var p pool
			return p, sc.Scan(&p.id, jsonCol(&p.serverIDs))
		})
		if err != nil {
			return err
		}
		for _, p := range pools {
			rest := slices.DeleteFunc(p.serverIDs, func(v string) bool { return v == id })
			if _, err := tx.ExecContext(ctx, "UPDATE pools SET server_ids = ?, enabled = enabled AND ?, updated_at = ? WHERE id = ?",
				jsonArg(rest), len(rest) > 0, ms(time.Now()), p.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete server: %w", err)
	}
	return nil
}

func (s *Store) CreateServerToken(ctx context.Context, t ServerToken) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO server_tokens
		(id, token_hash, name, created_by, created_at, expires_at, used_at, server_id) VALUES (`+placeholders(8)+`)`,
		t.ID, t.TokenHash, t.Name, nullStr(t.CreatedBy), ms(t.CreatedAt), ms(t.ExpiresAt), msPtr(t.UsedAt), nullStr(t.ServerID))
	if err != nil {
		return fmt.Errorf("create server token: %w", dbErr(err))
	}
	return nil
}

// ConsumeServerToken marks the token used and records the server v, in one
// transaction. If the token has a server (made before an install over SSH),
// that row gets the status, the secret, and the facts of v and keeps its ID
// and name. Otherwise v is a new server with the name of the token. It
// returns ErrNotFound if the token is unknown, used, or expired at now.
func (s *Store) ConsumeServerToken(ctx context.Context, tokenHash string, now time.Time, v Server) (Server, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var id, name string
		var serverID sql.NullString
		err := tx.QueryRowContext(ctx, `UPDATE server_tokens SET used_at = ?
			WHERE token_hash = ? AND used_at IS NULL AND expires_at > ? RETURNING id, name, server_id`,
			ms(now), tokenHash, ms(now)).Scan(&id, &name, &serverID)
		if err != nil {
			return dbErr(err)
		}
		if serverID.Valid {
			f := v.ServerFacts
			v, err = scanServer(tx.QueryRowContext(ctx, `UPDATE servers SET status = ?, status_detail = ?,
					address = ?, provider = ?, os = ?, kernel = ?, arch = ?, mode = ?, vcpu = ?, memory_mb = ?,
					disk_bytes = ?, disk_kind = ?, kvm = ?, docker_version = ?, sysbox_version = ?,
					firecracker_version = ?, version = ?, secret_hash = ?, last_seen_at = ?, updated_at = ?
				WHERE id = ? RETURNING `+serverCols,
				v.Status, v.StatusDetail, f.Address, f.Provider, f.OS, f.Kernel, f.Arch, f.Mode, f.VCPU, f.MemoryMB,
				f.DiskBytes, f.DiskKind, f.KVM, f.DockerVersion, f.SysboxVersion, f.FirecrackerVersion, f.Version,
				nullStr(v.SecretHash), msPtr(v.LastSeenAt), ms(now), serverID.String))
			return dbErr(err)
		}
		v.Name = name
		if err := insertServer(ctx, tx, v); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE server_tokens SET server_id = ? WHERE id = ?", v.ID, id)
		return err
	})
	if err != nil {
		return Server{}, fmt.Errorf("use server token: %w", err)
	}
	return v, nil
}

func (s *Store) AppendServerStats(ctx context.Context, stats []ServerStat) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for _, st := range stats {
			_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO server_stats
				(server_id, t_ms, cpu_percent, memory_percent, disk_percent) VALUES (?, ?, ?, ?, ?)`,
				st.ServerID, ms(st.At), st.CPUPercent, st.MemoryPercent, st.DiskPercent)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("append server stats: %w", err)
	}
	return nil
}

// ListServerStats returns the samples of a server from since, oldest first.
func (s *Store) ListServerStats(ctx context.Context, serverID string, since time.Time) ([]ServerStat, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT server_id, t_ms, cpu_percent, memory_percent, disk_percent
		FROM server_stats WHERE server_id = ? AND t_ms >= ? ORDER BY t_ms`, serverID, ms(since))
	list, err := scanAll(rows, err, scanServerStat)
	if err != nil {
		return nil, fmt.Errorf("list server stats: %w", err)
	}
	return list, nil
}

// LatestServerStats returns the newest sample of each server.
func (s *Store) LatestServerStats(ctx context.Context) (map[string]ServerStat, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT server_id, max(t_ms), cpu_percent, memory_percent, disk_percent
		FROM server_stats GROUP BY server_id`)
	list, err := scanAll(rows, err, scanServerStat)
	if err != nil {
		return nil, fmt.Errorf("list server stats: %w", err)
	}
	latest := make(map[string]ServerStat, len(list))
	for _, st := range list {
		latest[st.ServerID] = st
	}
	return latest, nil
}

func scanServerStat(sc scanner) (ServerStat, error) {
	var st ServerStat
	err := sc.Scan(&st.ServerID, at(&st.At), &st.CPUPercent, &st.MemoryPercent, &st.DiskPercent)
	return st, err
}

// PruneServerStats deletes the samples older than before.
func (s *Store) PruneServerStats(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.w.ExecContext(ctx, "DELETE FROM server_stats WHERE t_ms < ?", ms(before))
	if err != nil {
		return 0, fmt.Errorf("prune server stats: %w", err)
	}
	return res.RowsAffected()
}
