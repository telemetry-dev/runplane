package store

import (
	"context"
	"database/sql"
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

type DoctorCheck struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"` // ok | warn | fail
	Detail  string `json:"detail"`
	Fix     string `json:"fix"`
	Command string `json:"command"`
}

// DoctorResult is the last doctor run on a server.
type DoctorResult struct {
	ServerID  string
	CheckedAt time.Time
	Checks    []DoctorCheck
}

const factCols = `address, provider, os, kernel, arch, mode, vcpu, memory_mb, disk_bytes, disk_kind, kvm,
	docker_version, sysbox_version, firecracker_version, version`

const serverCols = "id, name, kind, status, status_detail, " + factCols +
	", secret_hash, last_seen_at, created_at, updated_at"

func (v *Server) cols() []any {
	return slices.Concat([]any{&v.ID, &v.Name, &v.Kind, &v.Status, &v.StatusDetail}, v.ServerFacts.cols(),
		[]any{text(&v.SecretHash), atPtr(&v.LastSeenAt), at(&v.CreatedAt), at(&v.UpdatedAt)})
}

// cols returns the fields in the order of factCols.
func (f *ServerFacts) cols() []any {
	return []any{&f.Address, &f.Provider, &f.OS, &f.Kernel, &f.Arch, &f.Mode, &f.VCPU, &f.MemoryMB, &f.DiskBytes,
		&f.DiskKind, &f.KVM, &f.DockerVersion, &f.SysboxVersion, &f.FirecrackerVersion, &f.Version}
}

// CreateServer returns ErrConflict for a second local server.
func (s *Store) CreateServer(ctx context.Context, v Server) error {
	return exec(ctx, s.w, "create server", insert("servers", serverCols), v.cols()...)
}

func (s *Store) GetServer(ctx context.Context, id string) (Server, error) {
	return queryOne(ctx, s.r, "get server", scanRow[Server], "SELECT "+serverCols+" FROM servers WHERE id = ?", id)
}

// GetLocalServer returns the primary, or ErrNotFound before the first start.
func (s *Store) GetLocalServer(ctx context.Context) (Server, error) {
	return queryOne(ctx, s.r, "get server", scanRow[Server], "SELECT "+serverCols+" FROM servers WHERE kind = ?", ServerLocal)
}

// ListServers returns the primary first.
func (s *Store) ListServers(ctx context.Context) ([]Server, error) {
	return queryAll(ctx, s.r, "list servers", scanRow[Server],
		"SELECT "+serverCols+" FROM servers ORDER BY kind = 'local' DESC, name, id")
}

func (s *Store) SetServerName(ctx context.Context, id, name string) error {
	return exec1(ctx, s.w, "set server name", "UPDATE servers SET name = ?, updated_at = ? WHERE id = ?",
		name, ms(time.Now()), id)
}

func (s *Store) UpdateServerFacts(ctx context.Context, id string, f ServerFacts) error {
	return exec1(ctx, s.w, "update server facts", "UPDATE servers SET "+assign(factCols)+", updated_at = ? WHERE id = ?",
		append(f.cols(), ms(time.Now()), id)...)
}

func (s *Store) SetServerStatus(ctx context.Context, id, status, detail string) error {
	return exec1(ctx, s.w, "set server status", "UPDATE servers SET status = ?, status_detail = ?, updated_at = ? WHERE id = ?",
		status, detail, ms(time.Now()), id)
}

// SetServerStatusIf sets the status and the detail of a server only while
// its status is from. It reports whether the row changed.
func (s *Store) SetServerStatusIf(ctx context.Context, id, from, to, detail string) (bool, error) {
	return changed(ctx, s.w, "set server status", `UPDATE servers SET status = ?, status_detail = ?, updated_at = ?
		WHERE id = ? AND status = ?`, to, detail, ms(time.Now()), id, from)
}

func (s *Store) TouchServer(ctx context.Context, id string, seen time.Time) error {
	return exec1(ctx, s.w, "touch server", "UPDATE servers SET last_seen_at = ? WHERE id = ?", ms(seen), id)
}

func (s *Store) SetServerSecretHash(ctx context.Context, id, hash string) error {
	return exec1(ctx, s.w, "set server secret", "UPDATE servers SET secret_hash = ?, updated_at = ? WHERE id = ?",
		nullStr(hash), ms(time.Now()), id)
}

// SetServerRegion records the cloud region of a server. It reports whether
// the region changed.
func (s *Store) SetServerRegion(ctx context.Context, id, region string) (bool, error) {
	return changed(ctx, s.w, "set server region", "UPDATE servers SET region = ? WHERE id = ? AND region != ?",
		region, id, region)
}

func (s *Store) ServerRegions(ctx context.Context) (map[string]string, error) {
	return queryMap(ctx, s.r, "list server regions", func(sc scanner) (string, string, error) {
		var id, region string
		err := sc.Scan(&id, &region)
		return id, region, err
	}, "SELECT id, region FROM servers WHERE region != ''")
}

// DeleteServer also deletes its image builds and host samples, and removes
// its ID from the server_ids of each pool. A pool that loses its last server
// becomes disabled, because empty server_ids means all servers.
func (s *Store) DeleteServer(ctx context.Context, id string) error {
	return s.tx(ctx, "delete server", func(tx *sql.Tx) error {
		if err := exec1(ctx, tx, "delete row", "DELETE FROM servers WHERE id = ?", id); err != nil {
			return err
		}
		type pool struct {
			id        string
			serverIDs []string
		}
		pools, err := queryAll(ctx, tx, "list pools", func(sc scanner) (pool, error) {
			var p pool
			err := sc.Scan(&p.id, jsonCol(&p.serverIDs))
			return p, err
		}, `SELECT p.id, p.server_ids FROM pools p
			WHERE EXISTS (SELECT 1 FROM json_each(p.server_ids) WHERE value = ?)`, id)
		if err != nil {
			return err
		}
		for _, p := range pools {
			rest := slices.DeleteFunc(p.serverIDs, func(v string) bool { return v == id })
			if _, err := tx.ExecContext(ctx, "UPDATE pools SET server_ids = ?, enabled = enabled AND ?, updated_at = ? WHERE id = ?",
				jsonCol(&rest), len(rest) > 0, ms(time.Now()), p.id); err != nil {
				return err
			}
		}
		return nil
	})
}

const tokenCols = "id, token_hash, name, created_by, created_at, expires_at, used_at, server_id"

func (t *ServerToken) cols() []any {
	return []any{&t.ID, &t.TokenHash, &t.Name, text(&t.CreatedBy), at(&t.CreatedAt), at(&t.ExpiresAt), atPtr(&t.UsedAt),
		text(&t.ServerID)}
}

func (s *Store) CreateServerToken(ctx context.Context, t ServerToken) error {
	return exec(ctx, s.w, "create server token", insert("server_tokens", tokenCols), t.cols()...)
}

func (s *Store) GetServerToken(ctx context.Context, id string) (ServerToken, error) {
	return queryOne(ctx, s.r, "get server token", scanRow[ServerToken], "SELECT "+tokenCols+" FROM server_tokens WHERE id = ?", id)
}

// ConsumeServerToken marks the token used and records the server v, in one
// transaction. If the token has a server (made before an install over SSH),
// that row gets the status, the secret, and the facts of v and keeps its ID
// and name. Otherwise v is a new server with the name of the token. It
// returns ErrNotFound if the token is unknown, used, or expired at now.
func (s *Store) ConsumeServerToken(ctx context.Context, tokenHash string, now time.Time, v Server) (Server, error) {
	err := s.tx(ctx, "use server token", func(tx *sql.Tx) error {
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
			args := slices.Concat([]any{v.Status, v.StatusDetail}, f.cols(),
				[]any{nullStr(v.SecretHash), msPtr(v.LastSeenAt), ms(now), serverID.String})
			v, err = queryOne(ctx, tx, "update server", scanRow[Server], "UPDATE servers SET status = ?, status_detail = ?, "+
				assign(factCols)+", secret_hash = ?, last_seen_at = ?, updated_at = ? WHERE id = ? RETURNING "+serverCols, args...)
			return err
		}
		v.Name = name
		if err := exec(ctx, tx, "insert server", insert("servers", serverCols), v.cols()...); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE server_tokens SET server_id = ? WHERE id = ?", v.ID, id)
		return err
	})
	if err != nil {
		return Server{}, err
	}
	return v, nil
}

// DeleteServerTokens deletes the unused install tokens of a server.
func (s *Store) DeleteServerTokens(ctx context.Context, serverID string) error {
	return exec(ctx, s.w, "delete server tokens", "DELETE FROM server_tokens WHERE server_id = ? AND used_at IS NULL", serverID)
}

const statCols = "server_id, t_ms, cpu_percent, memory_percent, disk_percent"

func (st *ServerStat) cols() []any {
	return []any{&st.ServerID, at(&st.At), &st.CPUPercent, &st.MemoryPercent, &st.DiskPercent}
}

func (s *Store) AppendServerStats(ctx context.Context, stats []ServerStat) error {
	return s.tx(ctx, "append server stats", func(tx *sql.Tx) error {
		for _, st := range stats {
			_, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO server_stats ("+statCols+") VALUES (?, ?, ?, ?, ?)", st.cols()...)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) LatestServerStats(ctx context.Context) (map[string]ServerStat, error) {
	return queryMap(ctx, s.r, "list server stats", func(sc scanner) (string, ServerStat, error) {
		st, err := scanRow[ServerStat](sc)
		return st.ServerID, st, err
	}, "SELECT server_id, max(t_ms), cpu_percent, memory_percent, disk_percent FROM server_stats GROUP BY server_id")
}

func (s *Store) PruneServerStats(ctx context.Context, before time.Time) error {
	return exec(ctx, s.w, "prune server stats", "DELETE FROM server_stats WHERE t_ms < ?", ms(before))
}

const doctorCols = "server_id, checked_at, checks_json"

func (r *DoctorResult) cols() []any {
	return []any{&r.ServerID, at(&r.CheckedAt), jsonCol(&r.Checks)}
}

// SaveDoctorResult replaces the doctor result of the server.
func (s *Store) SaveDoctorResult(ctx context.Context, r DoctorResult) error {
	return exec(ctx, s.w, "save doctor result", upsert("server_doctor", doctorCols, "server_id"), r.cols()...)
}

// GetDoctorResult returns ErrNotFound if the doctor never ran on the server.
func (s *Store) GetDoctorResult(ctx context.Context, serverID string) (DoctorResult, error) {
	return queryOne(ctx, s.r, "get doctor result", scanRow[DoctorResult],
		"SELECT "+doctorCols+" FROM server_doctor WHERE server_id = ?", serverID)
}
