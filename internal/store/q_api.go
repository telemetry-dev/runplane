package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// OverviewSamples counts what the median queue time and the cache hit rate
// of Overview come from for [since, until): the jobs with a queue time and
// the cache lookups. With no samples these numbers mean "no data", not 0.
func (s *Store) OverviewSamples(ctx context.Context, since, until time.Time) (queueTimes int, cacheLookups int64, err error) {
	err = s.r.QueryRowContext(ctx, `SELECT count(*) FROM jobs
		WHERE queued_at >= ? AND queued_at < ? AND coalesce(assigned_at, started_at) IS NOT NULL`,
		ms(since), ms(until)).Scan(&queueTimes)
	if err != nil {
		return 0, 0, fmt.Errorf("count overview samples: %w", err)
	}
	err = s.r.QueryRowContext(ctx, `SELECT coalesce(sum(hits + misses), 0) FROM cache_stats WHERE hour >= ? AND hour < ?`,
		since.Unix()/3600, (until.Unix()+3599)/3600).Scan(&cacheLookups)
	if err != nil {
		return 0, 0, fmt.Errorf("count overview samples: %w", err)
	}
	return queueTimes, cacheLookups, nil
}

// SetServerRegion records the cloud region of a server. It reports whether
// the region changed.
func (s *Store) SetServerRegion(ctx context.Context, id, region string) (bool, error) {
	res, err := s.w.ExecContext(ctx, "UPDATE servers SET region = ? WHERE id = ? AND region != ?", region, id, region)
	if err != nil {
		return false, fmt.Errorf("set server region: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set server region: %w", err)
	}
	return n > 0, nil
}

// ServerRegions returns the region of each server that has one.
func (s *Store) ServerRegions(ctx context.Context) (map[string]string, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT id, region FROM servers WHERE region != ''")
	list, err := scanAll(rows, err, func(sc scanner) ([2]string, error) {
		var v [2]string
		return v, sc.Scan(&v[0], &v[1])
	})
	if err != nil {
		return nil, fmt.Errorf("list server regions: %w", err)
	}
	regions := make(map[string]string, len(list))
	for _, v := range list {
		regions[v[0]] = v[1]
	}
	return regions, nil
}

// DoctorCheck is one stored doctor check.
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

// SaveDoctorResult replaces the doctor result of the server.
func (s *Store) SaveDoctorResult(ctx context.Context, r DoctorResult) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO server_doctor (server_id, checked_at, checks_json) VALUES (?, ?, ?)
		ON CONFLICT (server_id) DO UPDATE SET checked_at = excluded.checked_at, checks_json = excluded.checks_json`,
		r.ServerID, ms(r.CheckedAt), jsonArg(r.Checks))
	if err != nil {
		return fmt.Errorf("save doctor result: %w", dbErr(err))
	}
	return nil
}

// GetDoctorResult returns ErrNotFound if the doctor never ran on the server.
func (s *Store) GetDoctorResult(ctx context.Context, serverID string) (DoctorResult, error) {
	r := DoctorResult{ServerID: serverID}
	err := s.r.QueryRowContext(ctx, "SELECT checked_at, checks_json FROM server_doctor WHERE server_id = ?", serverID).
		Scan(at(&r.CheckedAt), jsonCol(&r.Checks))
	if err != nil {
		return DoctorResult{}, fmt.Errorf("get doctor result: %w", dbErr(err))
	}
	return r, nil
}

func (s *Store) GetServerToken(ctx context.Context, id string) (ServerToken, error) {
	var t ServerToken
	var by, server sql.NullString
	err := s.r.QueryRowContext(ctx, `SELECT id, token_hash, name, created_by, created_at, expires_at, used_at, server_id
		FROM server_tokens WHERE id = ?`, id).
		Scan(&t.ID, &t.TokenHash, &t.Name, &by, at(&t.CreatedAt), at(&t.ExpiresAt), atPtr(&t.UsedAt), &server)
	if err != nil {
		return ServerToken{}, fmt.Errorf("get server token: %w", dbErr(err))
	}
	t.CreatedBy, t.ServerID = by.String, server.String
	return t, nil
}

// NotificationDelivery is the last delivery to a channel.
type NotificationDelivery struct {
	SentAt *time.Time // nil before the first delivery
	Error  string     // empty if the delivery worked
}

// RecordNotificationDelivery records a delivery to a channel at t. sendErr
// is empty if it worked.
func (s *Store) RecordNotificationDelivery(ctx context.Context, channelID string, t time.Time, sendErr string) error {
	err := exec1(ctx, s.w, "UPDATE notification_channels SET last_sent_at = ?, last_error = ? WHERE id = ?",
		ms(t), sendErr, channelID)
	if err != nil {
		return fmt.Errorf("record notification delivery: %w", err)
	}
	return nil
}

// NotificationDeliveries returns the last delivery of each channel.
func (s *Store) NotificationDeliveries(ctx context.Context) (map[string]NotificationDelivery, error) {
	type row struct {
		id string
		d  NotificationDelivery
	}
	rows, err := s.r.QueryContext(ctx, "SELECT id, last_sent_at, last_error FROM notification_channels")
	list, err := scanAll(rows, err, func(sc scanner) (row, error) {
		var r row
		return r, sc.Scan(&r.id, atPtr(&r.d.SentAt), &r.d.Error)
	})
	if err != nil {
		return nil, fmt.Errorf("list notification deliveries: %w", err)
	}
	out := make(map[string]NotificationDelivery, len(list))
	for _, r := range list {
		out[r.id] = r.d
	}
	return out, nil
}
