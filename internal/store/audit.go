package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type AuditEntry struct {
	ID     string          `json:"id"`
	At     time.Time       `json:"at"`
	UserID string          `json:"user_id"` // empty for runplane itself
	Action string          `json:"action"`  // for example "pool.create"
	Target string          `json:"target"`
	Detail json.RawMessage `json:"detail"` // JSON object, nil = {}
	IP     string          `json:"ip"`
}

func (s *Store) InsertAudit(ctx context.Context, e AuditEntry) error {
	detail := string(e.Detail)
	if detail == "" {
		detail = "{}"
	}
	_, err := s.w.ExecContext(ctx, `INSERT INTO audit_log (id, at, user_id, action, target, detail_json, ip)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, e.ID, ms(e.At), e.UserID, e.Action, e.Target, detail, e.IP)
	if err != nil {
		return fmt.Errorf("insert audit entry: %w", dbErr(err))
	}
	return nil
}

// ListAudit returns one page of entries, newest first, and the cursor of the
// next page ("" on the last page).
func (s *Store) ListAudit(ctx context.Context, cursor string, limit int) ([]AuditEntry, string, error) {
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	query := "SELECT id, at, user_id, action, target, detail_json, ip FROM audit_log"
	var args []any
	if cursor != "" {
		atMS, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("list audit log: %w", err)
		}
		query += " WHERE at < ? OR (at = ? AND id < ?)"
		args = append(args, atMS, atMS, id)
	}
	rows, err := s.r.QueryContext(ctx, query+" ORDER BY at DESC, id DESC LIMIT ?", append(args, limit+1)...)
	list, err := scanAll(rows, err, func(sc scanner) (AuditEntry, error) {
		var e AuditEntry
		var detail string
		err := sc.Scan(&e.ID, at(&e.At), &e.UserID, &e.Action, &e.Target, &detail, &e.IP)
		e.Detail = json.RawMessage(detail)
		return e, err
	})
	if err != nil {
		return nil, "", fmt.Errorf("list audit log: %w", err)
	}
	if len(list) <= limit {
		return list, "", nil
	}
	list = list[:limit]
	last := list[limit-1]
	return list, encodeCursor(ms(last.At), last.ID), nil
}
