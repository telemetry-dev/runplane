package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/store"
)

// actor is the user of the request, who makes its changes.
func actor(ctx context.Context) audit.Actor {
	return audit.Actor{UserID: currentUser(ctx).ID, IP: clientOf(ctx).ip}
}

// record writes an audit entry for a change that the handler makes itself.
// The services record their own changes.
func (h *Handler) record(ctx context.Context, action audit.Action, target string, detail map[string]any) {
	h.audit.Record(ctx, actor(ctx), action, target, detail)
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

// TransformSchema gives action the enum of audit.Actions. Entries of an
// older runplane can have other actions.
func (AuditEntry) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	action := s.Properties["action"]
	for _, a := range audit.Actions {
		action.Enum = append(action.Enum, string(a))
	}
	return s
}

type AuditPage struct {
	Items      []AuditEntry `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty" doc:"Cursor of the next page. Absent on the last page."`
}

type listAuditInput struct {
	Cursor string `query:"cursor" maxLength:"200"`
	Limit  int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}

func (h *Handler) listAudit(ctx context.Context, in *listAuditInput) (*body[AuditPage], error) {
	list, next, err := h.d.Store.ListAudit(ctx, in.Cursor, in.Limit)
	if errors.Is(err, store.ErrBadCursor) {
		return nil, invalid("query.cursor", "The cursor is not valid.", in.Cursor)
	}
	if err != nil {
		return nil, err
	}
	users, err := h.d.Store.ListUsers(ctx)
	if err != nil {
		return nil, err
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
	return &body[AuditPage]{page}, nil
}
