// Package audit records who changed what: the users of the dashboard, and
// runplane itself for the commands that install.sh runs.
package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/store"
)

// Actor makes a change. The zero Actor is runplane itself.
type Actor struct {
	UserID string
	IP     string // of the request
}

// Log writes the entries of the audit log.
type Log struct {
	Store  *store.Store
	Logger *slog.Logger // nil: no log
}

// Record writes an entry. action is "<object>.<verb>", for example
// "pool.create". A failure goes to the log and does not fail the change,
// because the change is done already.
func (l Log) Record(ctx context.Context, by Actor, action, target string, detail map[string]any) {
	var raw json.RawMessage
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			l.logger().Error("encode audit detail", "action", action, "err", err)
		}
		raw = b
	}
	err := l.Store.InsertAudit(context.WithoutCancel(ctx), store.AuditEntry{
		ID: ids.New(), At: time.Now(), UserID: by.UserID, Action: action, Target: target, Detail: raw, IP: by.IP,
	})
	if err != nil {
		l.logger().Error("write audit entry", "action", action, "target", target, "err", err)
	}
}

func (l Log) logger() *slog.Logger {
	if l.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return l.Logger
}

// Changes gives the old and new JSON value of each field that differs
// between a and b, two values of one type, except the fields in skip.
func Changes(a, b any, skip ...string) map[string][2]any {
	fa, fb := jsonFields(a), jsonFields(b)
	changes := map[string][2]any{}
	for _, fields := range []map[string]json.RawMessage{fa, fb} {
		for k := range fields {
			if !slices.Contains(skip, k) && !bytes.Equal(fa[k], fb[k]) {
				changes[k] = [2]any{fa[k], fb[k]}
			}
		}
	}
	return changes
}

func jsonFields(v any) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	b, _ := json.Marshal(v) // the store models always encode
	json.Unmarshal(b, &fields)
	return fields
}
