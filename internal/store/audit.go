package store

import (
	"cmp"
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

const auditCols = "id, at, user_id, action, target, detail_json, ip"

func (s *Store) InsertAudit(ctx context.Context, e AuditEntry) error {
	return exec(ctx, s.w, "insert audit entry", insert("audit_log", auditCols),
		e.ID, ms(e.At), e.UserID, e.Action, e.Target, cmp.Or(string(e.Detail), "{}"), e.IP)
}

// ListAudit returns one page of entries, newest first, and the cursor of the
// next page ("" on the last page).
func (s *Store) ListAudit(ctx context.Context, cursor string, limit int) ([]AuditEntry, string, error) {
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	query := "SELECT " + auditCols + " FROM audit_log"
	var args []any
	if cursor != "" {
		atMS, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("list audit log: %w", err)
		}
		query += " WHERE at < ? OR (at = ? AND id < ?)"
		args = append(args, atMS, atMS, id)
	}
	list, err := queryAll(ctx, s.r, "list audit log", func(sc scanner) (AuditEntry, error) {
		var e AuditEntry
		var detail string
		err := sc.Scan(&e.ID, at(&e.At), &e.UserID, &e.Action, &e.Target, &detail, &e.IP)
		e.Detail = json.RawMessage(detail)
		return e, err
	}, query+" ORDER BY at DESC, id DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return nil, "", err
	}
	if len(list) <= limit {
		return list, "", nil
	}
	list = list[:limit]
	last := list[limit-1]
	return list, encodeCursor(ms(last.At), last.ID), nil
}
