package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

// Deps are the parts that the API uses. Store, Auth, Bus, Box, and Config are
// required. Each service is optional: without it, its routes answer 503.
//
// A service error is shown to the user as the detail of a 500 problem, so
// services should return errors that make sense to an admin. A service can
// also return a huma.StatusError to choose the status.
type Deps struct {
	Store  *store.Store
	Auth   *auth.Service
	Bus    *events.Bus
	Box    *secret.Box
	Log    *slog.Logger
	Config config.Config

	Scheduler     Scheduler
	GitHub        GitHub
	Servers       Servers
	Images        Images
	Caches        Caches
	Backups       Backups
	Updater       Updater
	Notifier      Notifier
	Logs          Logs
	Shell         Shell
	HostChecker   HostChecker
	InstallScript InstallScript
	SettingsHook  SettingsHook
	HTTPS         HTTPS
}

type Scheduler interface {
	// PoolsChanged tells the scheduler that a pool was made, changed, or deleted.
	PoolsChanged(ctx context.Context)
	StopSandbox(ctx context.Context, id string) error
	// RerunJob asks GitHub to run the job again. With debug, the new attempt
	// keeps its sandbox after the job for a debug shell.
	RerunJob(ctx context.Context, jobID string, debug bool) error
}

// GitHub has the GitHub App flows. base is the browser URL of runplane, for
// example "https://ci.example.com", without a trailing slash.
type GitHub interface {
	// Manifest returns the GitHub URL that the browser posts the form to and
	// the manifest JSON for the form field "manifest".
	Manifest(ctx context.Context, base, accountType, organization string) (actionURL, manifest string, err error)
	// Callback converts the manifest code into the app and returns the URL
	// for the browser, normally the app installation page on GitHub.
	Callback(ctx context.Context, base, code, state string) (redirect string, err error)
	// Setup records an installation, syncs its repositories, and makes the
	// scale sets. It returns the URL for the browser. installationID is 0
	// when setupAction is "request".
	Setup(ctx context.Context, base string, installationID int64, setupAction string) (redirect string, err error)
	// Sync reads the installations, repositories, and suspensions now.
	Sync(ctx context.Context) error
	// Disconnect stops the listeners, deletes the scale sets, and removes the app.
	Disconnect(ctx context.Context) error
}

type Servers interface {
	// NewToken makes a one-time install token for a new agent and the
	// command that installs the agent with it. An empty name means the host
	// name of the agent.
	NewToken(ctx context.Context, base, name, createdBy string) (ServerToken, error)
	// InstallSSH starts an install over SSH and returns the ID of the new
	// server. The install continues in the background; the server status
	// and status detail show its progress.
	InstallSSH(ctx context.Context, base string, req SSHInstall) (serverID string, err error)
	// SSHPublicKey returns the runplane SSH key in authorized_keys format.
	SSHPublicKey(ctx context.Context) (string, error)
	// Doctor runs the doctor checks on the server.
	Doctor(ctx context.Context, serverID string) ([]Check, error)
	// Remove stops the sandboxes of an agent and disconnects it. The API
	// then deletes the server row.
	Remove(ctx context.Context, serverID string) error
	// UpdateState tells whether the server runs the release of the
	// primary: one of the Update* values.
	UpdateState(v store.Server) string
	// UpdateAgent makes a connected agent install the release of the
	// primary. The update runs in the background, and UpdateState shows it.
	UpdateAgent(ctx context.Context, serverID string) error
}

type Images interface {
	// Refresh pulls the image again on all servers.
	Refresh(ctx context.Context, imageID string) error
	// Remove deletes the files of the image on all servers. The API then
	// deletes the image row.
	Remove(ctx context.Context, imageID string) error
}

type Caches interface {
	// Delete removes the cache entry id, or all entries of repository if id
	// is empty, with their blobs. It returns the count of removed entries.
	Delete(ctx context.Context, repository, id string) (int, error)
	// MirrorStats returns the counts of the mirror. The API sets StartedAt.
	MirrorStats(ctx context.Context) (MirrorStats, error)
	// MaxBytes returns the limit that the cache server uses now: the
	// cache_max_gb setting, at most a quarter of the disk of the cache.
	MaxBytes() int64
}

type Backups interface {
	// Run makes a backup now and returns its ID.
	Run(ctx context.Context) (backupID string, err error)
}

type Updater interface {
	// Check reads the latest release. It sets State while an update that
	// did not come through the API (auto update) runs.
	Check(ctx context.Context) (UpdateStatus, error)
	// Apply installs the latest release. The process restarts afterwards.
	Apply(ctx context.Context) error
}

type Notifier interface {
	// Test sends a test message to a channel.
	Test(ctx context.Context, base, kind, url string) error
}

type Logs interface {
	// SandboxLog returns the console and runner log of a sandbox. With
	// follow, the reader stays open until the sandbox stops or ctx ends.
	SandboxLog(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	// JobLog returns the log of a job: the stored GitHub log after the job,
	// the sandbox log while it runs. step 0 means all steps. The sandbox log
	// has no step marks, so it always has all steps: live is true for it.
	JobLog(ctx context.Context, jobID string, step int, follow bool) (log io.ReadCloser, live bool, err error)
}

type Shell interface {
	// Open starts a login shell as the runner user in the sandbox.
	Open(ctx context.Context, sandboxID string, size sandbox.TermSize) (sandbox.Terminal, error)
}

type HostChecker interface {
	// Check reads this server and runs the doctor checks for the setup page.
	Check(ctx context.Context) (HostCheck, error)
}

type InstallScript interface {
	// Script returns install.sh with the primary URL and the agent
	// certificate fingerprint filled in for the request.
	Script(r *http.Request) []byte
}

type SettingsHook interface {
	// SettingsChanged runs after an admin changes the settings.
	SettingsChanged(ctx context.Context, old, new store.Settings)
}
