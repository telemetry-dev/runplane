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

// Action is the kind of change of an entry, as "<object>.<verb>".
type Action string

const (
	SetupDone         Action = "setup.done"
	UserUpdate        Action = "user.update"
	UserDelete        Action = "user.delete"
	UserPassword      Action = "user.password"
	UserPasswordReset Action = "user.password_reset"
	InviteCreate      Action = "invite.create"
	InviteLink        Action = "invite.link"
	InviteDelete      Action = "invite.delete"
	PoolCreate        Action = "pool.create"
	PoolUpdate        Action = "pool.update"
	PoolDelete        Action = "pool.delete"
	ServerToken       Action = "server.token"
	ServerInstall     Action = "server.install"
	ServerUpdate      Action = "server.update"
	ServerDelete      Action = "server.delete"
	ServerUpdateAgent Action = "server.update_agent"
	SandboxStop       Action = "sandbox.stop"
	SandboxShell      Action = "sandbox.shell"
	SandboxSSH        Action = "sandbox.ssh"
	CacheDelete       Action = "cache.delete"
	ImageCreate       Action = "image.create"
	ImageUpdate       Action = "image.update"
	ImageDelete       Action = "image.delete"
	ImageRefresh      Action = "image.refresh"
	SettingsUpdate    Action = "settings.update"
	JobRerun          Action = "job.rerun"
	BackupCreate      Action = "backup.create"
	BackupDownload    Action = "backup.download"
	UpdateApply       Action = "update.apply"
	GitHubManifest    Action = "github.manifest"
	GitHubAppCreate   Action = "github.app.create"
	// The returns from the GitHub page of the app installation, one for each
	// setup_action of GitHub.
	GitHubInstall        Action = "github.installation.install"
	GitHubInstallUpdate  Action = "github.installation.update"
	GitHubInstallRequest Action = "github.installation.request"
	GitHubSync           Action = "github.sync"
	GitHubPullRequest    Action = "github.pull_request.create"
	GitHubDisconnect     Action = "github.disconnect"
	ChannelCreate        Action = "notification.create"
	ChannelUpdate        Action = "notification.update"
	ChannelDelete        Action = "notification.delete"
)

var Actions = []Action{
	SetupDone, UserUpdate, UserDelete, UserPassword, UserPasswordReset, InviteCreate, InviteLink, InviteDelete,
	PoolCreate, PoolUpdate, PoolDelete,
	ServerToken, ServerInstall, ServerUpdate, ServerDelete, ServerUpdateAgent,
	SandboxStop, SandboxShell, SandboxSSH, CacheDelete,
	ImageCreate, ImageUpdate, ImageDelete, ImageRefresh,
	SettingsUpdate, JobRerun, BackupCreate, BackupDownload, UpdateApply,
	GitHubManifest, GitHubAppCreate, GitHubInstall, GitHubInstallUpdate, GitHubInstallRequest,
	GitHubSync, GitHubPullRequest, GitHubDisconnect,
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

// Record writes an entry. A failure goes to the log and does not fail the
// change, because the change is done already.
func (l Log) Record(ctx context.Context, by Actor, action Action, target string, detail map[string]any) {
	var raw json.RawMessage
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			l.logger().Error("encode audit detail", "action", action, "err", err)
		}
		raw = b
	}
	err := l.Store.InsertAudit(context.WithoutCancel(ctx), store.AuditEntry{
		ID: ids.New(), At: time.Now(), UserID: by.UserID, Action: string(action), Target: target, Detail: raw, IP: by.IP,
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
