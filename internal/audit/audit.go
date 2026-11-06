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

// The actions of the audit log, as "<object>.<verb>".
const (
	SetupDone         = "setup.done"
	UserUpdate        = "user.update"
	UserDelete        = "user.delete"
	UserPassword      = "user.password"
	InviteCreate      = "invite.create"
	InviteLink        = "invite.link"
	InviteDelete      = "invite.delete"
	PoolCreate        = "pool.create"
	PoolUpdate        = "pool.update"
	PoolDelete        = "pool.delete"
	ServerToken       = "server.token"
	ServerInstall     = "server.install"
	ServerUpdate      = "server.update"
	ServerDelete      = "server.delete"
	ServerUpdateAgent = "server.update_agent"
	SandboxStop       = "sandbox.stop"
	SandboxShell      = "sandbox.shell"
	SandboxSSH        = "sandbox.ssh"
	CacheDelete       = "cache.delete"
	ImageCreate       = "image.create"
	ImageUpdate       = "image.update"
	ImageDelete       = "image.delete"
	ImageRefresh      = "image.refresh"
	SettingsUpdate    = "settings.update"
	JobRerun          = "job.rerun"
	BackupCreate      = "backup.create"
	BackupDownload    = "backup.download"
	UpdateApply       = "update.apply"
	GitHubManifest    = "github.manifest"
	GitHubAppCreate   = "github.app.create"
	GitHubSync        = "github.sync"
	GitHubDisconnect  = "github.disconnect"
	ChannelCreate     = "notification.create"
	ChannelUpdate     = "notification.update"
	ChannelDelete     = "notification.delete"
)

// GitHubInstallation is the action of a return from the GitHub page of the
// app installation, with the setup_action of GitHub: install, update, or
// request.
func GitHubInstallation(setupAction string) string { return "github.installation." + setupAction }

// Actions are all actions.
var Actions = []string{
	SetupDone, UserUpdate, UserDelete, UserPassword, InviteCreate, InviteLink, InviteDelete,
	PoolCreate, PoolUpdate, PoolDelete,
	ServerToken, ServerInstall, ServerUpdate, ServerDelete, ServerUpdateAgent,
	SandboxStop, SandboxShell, SandboxSSH, CacheDelete,
	ImageCreate, ImageUpdate, ImageDelete, ImageRefresh,
	SettingsUpdate, JobRerun, BackupCreate, BackupDownload, UpdateApply,
	GitHubManifest, GitHubAppCreate, GitHubInstallation("install"), GitHubInstallation("update"), GitHubInstallation("request"),
	GitHubSync, GitHubDisconnect,
	ChannelCreate, ChannelUpdate, ChannelDelete,
}

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

// Record writes an entry. action is one of Actions. A failure goes to the log and does not fail the change,
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
