package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/store"
)

// audit records a change by the current user. action is "<object>.<verb>",
// for example "pool.create". A failure is logged and does not fail the
// request, because the change is already done.
func (h *Handler) audit(ctx context.Context, action, target string, detail map[string]any) {
	var raw json.RawMessage
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			h.d.Log.Error("encode audit detail", "action", action, "err", err)
		}
		raw = b
	}
	err := h.d.Store.InsertAudit(context.WithoutCancel(ctx), store.AuditEntry{
		ID: ids.New(), At: time.Now(), UserID: currentUser(ctx).ID, Action: action, Target: target,
		Detail: raw, IP: clientOf(ctx).ip,
	})
	if err != nil {
		h.d.Log.Error("write audit entry", "action", action, "target", target, "err", err)
	}
}

// fieldChanges gives the old and new JSON value of each field that differs
// between a and b, two values of one struct type, except the fields in skip.
func fieldChanges(a, b any, skip ...string) map[string][2]any {
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

// jobDetail adds the names of a job to the detail of an audit entry.
func jobDetail(detail map[string]any, j store.Job) map[string]any {
	detail["repository"] = j.Repository
	detail["workflow_file"] = j.WorkflowFile
	detail["job_name"] = j.JobName
	detail["run_number"] = j.RunNumber
	return detail
}

type AuditEntry struct {
	ID        string         `json:"id"`
	At        time.Time      `json:"at"`
	UserID    string         `json:"user_id" doc:"Empty for runplane itself."`
	UserName  string         `json:"user_name" doc:"Empty if the user is deleted."`
	UserEmail string         `json:"user_email"`
	Action    string         `json:"action" doc:"<object>.<verb>, for example \"pool.create\"."`
	Target    string         `json:"target" doc:"ID of the changed object."`
	Detail    map[string]any `json:"detail" doc:"Names and values of the change. pool.update, server.update, and settings.update have changes, with [old, new] for each changed field. settings.update also has keys, with all changed settings keys. Its changes never hold the S3 secret key. The entries of a job or a sandbox have runner_name, job_id, job_name, workflow_file, run_number, and repository where they apply."`
	IP        string         `json:"ip"`
}

type AuditPage struct {
	Items      []AuditEntry `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty" doc:"Cursor of the next page. Absent on the last page."`
}

type listAuditInput struct {
	Cursor string `query:"cursor" maxLength:"200"`
	Limit  int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}

func (h *Handler) listAudit(ctx context.Context, in *listAuditInput) (*struct{ Body AuditPage }, error) {
	list, next, err := h.d.Store.ListAudit(ctx, in.Cursor, in.Limit)
	if errors.Is(err, store.ErrBadCursor) {
		return nil, invalid("query.cursor", "The cursor is not valid.", in.Cursor)
	}
	if err != nil {
		return nil, h.internal(err)
	}
	users, err := h.d.Store.ListUsers(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	byID := make(map[string]store.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	page := AuditPage{Items: make([]AuditEntry, len(list)), NextCursor: next}
	for i, e := range list {
		detail := map[string]any{}
		if len(e.Detail) > 0 {
			if err := json.Unmarshal(e.Detail, &detail); err != nil {
				detail = map[string]any{}
			}
		}
		u := byID[e.UserID]
		page.Items[i] = AuditEntry{
			ID: e.ID, At: e.At, UserID: e.UserID, UserName: u.Name, UserEmail: u.Email,
			Action: e.Action, Target: e.Target, Detail: detail, IP: e.IP,
		}
	}
	return &struct{ Body AuditPage }{page}, nil
}
