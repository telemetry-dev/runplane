// Package api serves the runplane HTTP API, the SSE stream, the debug shell,
// the install script, and the embedded frontend.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/fleet"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/tlsutil"
	"github.com/runplane/runplane/internal/update"
	"github.com/runplane/runplane/internal/version"
	"github.com/runplane/runplane/internal/webui"
)

// Handler serves the HTTP requests of the primary.
type Handler struct {
	d       Deps
	api     huma.API
	handler http.Handler

	// done closes when the HTTP server shuts down, so that streams end.
	done      chan struct{}
	closeOnce sync.Once

	started time.Time // the start of this process, near enough
}

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
	Mirrors       Mirrors
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

// New registers all routes. It does not use d until a request comes, so
// OpenAPI can build the spec without a database.
func New(d Deps) *Handler {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	h := &Handler{d: d, done: make(chan struct{}), started: time.Now().UTC()}
	mux := http.NewServeMux()
	h.api = humago.New(mux, humaConfig())
	h.api.UseMiddleware(h.authorize)
	h.routes()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, http.StatusNotFound, "No API route has this path.")
	})
	mux.Handle("/", webui.Handler())
	h.handler = h.logRequests(h.guard(mux))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.handler.ServeHTTP(w, r) }

// Close ends the SSE streams and followed logs. Call it when the HTTP server
// starts to shut down (http.Server.RegisterOnShutdown), because these
// requests never end by themselves.
func (h *Handler) Close() { h.closeOnce.Do(func() { close(h.done) }) }

// OpenAPI returns the OpenAPI 3.1 spec as indented JSON.
func OpenAPI() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(New(Deps{}).api.OpenAPI())
	return b.Bytes(), err
}

type Scheduler interface {
	// PoolsChanged tells the scheduler that a pool was made, changed, or deleted.
	PoolsChanged()
	StopSandbox(ctx context.Context, id string) error
	// RerunJob asks GitHub to run the job again. With debug, the new attempt
	// keeps its sandbox after the job for a debug shell.
	RerunJob(ctx context.Context, jobID string, debug bool) error
}

// GitHub has the GitHub App flows.
type GitHub interface {
	// Manifest returns the GitHub URL that the browser posts the form to and
	// the manifest JSON for the form field "manifest". base is the browser
	// URL of runplane, for example "https://ci.example.com", without a
	// trailing slash.
	Manifest(ctx context.Context, base, accountType, organization string) (actionURL, manifest string, err error)
	// Callback converts the manifest code into the app and returns the URL
	// for the browser, normally the app installation page on GitHub.
	Callback(ctx context.Context, code, state string) (redirect string, err error)
	// Setup records an installation, syncs its repositories, and makes the
	// scale sets. It returns the URL for the browser. installationID is 0
	// when setupAction is "request".
	Setup(ctx context.Context, installationID int64, setupAction string) (redirect string, err error)
	// Sync reads the installations, repositories, and suspensions now.
	Sync(ctx context.Context) error
	// Disconnect stops the listeners, deletes the scale sets, and removes the app.
	Disconnect(ctx context.Context) error
}

// Servers is the fleet of servers: *fleet.Fleet.
type Servers interface {
	// NewToken makes a one-time install token for a new agent and the
	// command that installs the agent with it.
	NewToken(ctx context.Context, base, name, createdBy string) (fleet.Token, error)
	// InstallSSH starts an install over SSH and returns the ID of the new
	// server. The install continues in the background; the server status
	// and status detail show its progress.
	InstallSSH(ctx context.Context, base string, req fleet.SSHInstall) (serverID string, err error)
	SSHPublicKey(ctx context.Context) (string, error)
	// Doctor returns fleet.ErrNotConnected for a server without a
	// connection.
	Doctor(ctx context.Context, serverID string) ([]host.Check, error)
	// Limits returns the limits of the driver of a connected server.
	Limits(serverID string) (sandbox.Limits, bool)
	// RemoveServer stops the sandboxes of an agent and disconnects it. The
	// API then deletes the server row. It returns fleet.ErrLocalServer for
	// the primary.
	RemoveServer(ctx context.Context, serverID string) error
	UpdateState(v store.Server) fleet.UpdateState
	// UpdateAgent makes a connected agent install the release of the
	// primary. The update runs in the background, and UpdateState shows it.
	UpdateAgent(ctx context.Context, serverID string) error
}

type Images interface {
	// RefreshImage pulls the image again on all servers.
	RefreshImage(ctx context.Context, imageID string) error
	// RemoveImage deletes the files of the image on all servers. The API
	// then deletes the image row.
	RemoveImage(ctx context.Context, imageID string) error
}

type Caches interface {
	// Delete removes the cache entry id with its blob. It returns
	// store.ErrNotFound if no entry has the ID.
	Delete(ctx context.Context, id string) error
	// DeleteRepository removes the entries of a repository with their blobs,
	// and returns the count.
	DeleteRepository(ctx context.Context, repository string) (int, error)
	// MaxBytes returns the byte limit that the cache server uses now.
	MaxBytes() int64
}

type Mirrors interface {
	// MirrorStats sums the counts of the registry mirrors of all servers.
	MirrorStats(ctx context.Context) mirror.Stats
}

type Backups interface {
	// Create makes a backup now.
	Create(ctx context.Context) (backupID string, err error)
}

type Updater interface {
	// Check returns the latest release.
	Check(ctx context.Context) (update.Status, error)
	// State tells if an update of this process runs, also an auto update.
	State() update.State
	// Apply installs the latest release, and the process restarts. It
	// returns update.ErrNoUpdate when no newer release exists, and
	// update.ErrBusy while an update runs.
	Apply(ctx context.Context) (update.Status, error)
}

type Notifier interface {
	Test(ctx context.Context, base, kind, url string) error
}

// Logs gives the logs of the sandboxes and jobs. Its errors are the errors
// of joblog.
type Logs interface {
	// Sandbox returns the console and runner log of a sandbox. With follow,
	// the reader stays open until the sandbox stops or ctx ends.
	Sandbox(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	// Job returns the log of a job: the stored GitHub log after the job,
	// the sandbox log while it runs. step 0 means all steps. The sandbox log
	// has no step marks, so it always has all steps: live is true for it.
	Job(ctx context.Context, jobID string, step int, follow bool) (log io.ReadCloser, live bool, err error)
}

type Shell interface {
	// Open starts a login shell as the runner user in the sandbox.
	Open(ctx context.Context, sandboxID string, size sandbox.TermSize) (sandbox.Terminal, error)
}

type HostChecker interface {
	// Check reads this server and runs the doctor checks for the setup page.
	Check(ctx context.Context) (fleet.HostCheck, error)
}

type InstallScript interface {
	// Script returns install.sh with the values of the primary, for the
	// host[:port] that the request named.
	Script(host string) []byte
}

type SettingsHook interface {
	// SettingsChanged runs after an admin changes the settings.
	SettingsChanged(ctx context.Context, old, new store.Settings)
}

// HTTPS reports the ACME certificate of the domain setting. Without it, the
// settings have no https field.
type HTTPS interface {
	Status(ctx context.Context) tlsutil.CertStatus
}

// Handlers send [] for empty lists, never null.
func init() { huma.DefaultArrayNullable = false }

func humaConfig() huma.Config {
	return huma.Config{
		OpenAPI: &huma.OpenAPI{
			OpenAPI: "3.1.0",
			Info: &huma.Info{
				Title:       "runplane",
				Version:     version.Version,
				Description: "API of the runplane dashboard. Mutating requests need the header `X-Runplane-CSRF: 1`.",
			},
			Components: &huma.Components{
				Schemas: huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer),
				SecuritySchemes: map[string]*huma.SecurityScheme{
					"session": {Type: "apiKey", In: "cookie", Name: sessionCookie},
				},
			},
		},
		OpenAPIPath:   "/api/openapi",
		Formats:       huma.DefaultFormats,
		DefaultFormat: "application/json",
	}
}

func (h *Handler) routes() {
	handle(h, public, "GET /api/status", "get-status", "Read the instance status", h.getStatus)
	handle(h, public, "POST /api/setup", "setup", "Make the first admin with the setup code", h.setup)
	handle(h, public, "GET /api/setup/host", "get-setup-host", "Check this server during setup", h.getSetupHost)
	handle(h, auth.ActionRead, "GET /api/session", "get-session", "Read the current user", h.getSession)
	handle(h, public, "POST /api/session", "login", "Log in", h.login)
	handle(h, public, "DELETE /api/session", "logout", "Log out", h.logout)
	handle(h, auth.ActionRead, "PATCH /api/me", "update-me", "Change the name or the password of the current user", h.updateMe)

	handle(h, auth.ActionRead, "GET /api/overview", "get-overview", "Read the overview numbers, running jobs, servers, and chart", h.getOverview)

	handle(h, auth.ActionRead, "GET /api/github", "get-github", "Read the GitHub App, installations, and listener status", h.getGitHub)
	handle(h, auth.ActionAdmin, "POST /api/github/manifest", "create-github-manifest", "Start the GitHub App manifest flow", h.createManifest)
	handle(h, public, "GET /api/github/callback", "github-callback", "Finish the manifest flow (browser redirect from GitHub)", h.githubCallback, redirectResponse)
	handle(h, public, "GET /api/github/setup", "github-setup", "Record an installation (browser redirect from GitHub)", h.githubSetup, redirectResponse)
	handle(h, auth.ActionAdmin, "POST /api/github/sync", "sync-github", "Sync the installations now", h.syncGitHub)
	handle(h, auth.ActionAdmin, "DELETE /api/github", "disconnect-github", "Disconnect GitHub", h.disconnectGitHub)

	handle(h, auth.ActionRead, "GET /api/pools", "list-pools", "List the pools with live sandbox counts", h.listPools)
	handle(h, auth.ActionAdmin, "POST /api/pools", "create-pool", "Make a pool", h.createPool)
	handle(h, auth.ActionRead, "GET /api/pools/placement", "get-pool-placement", "Count the servers where a sandbox of a size can start", h.getPlacement)
	handle(h, auth.ActionRead, "GET /api/pools/{id}", "get-pool", "Read a pool", h.getPool)
	handle(h, auth.ActionAdmin, "PATCH /api/pools/{id}", "update-pool", "Change a pool", h.updatePool)
	handle(h, auth.ActionAdmin, "DELETE /api/pools/{id}", "delete-pool", "Delete a pool", h.deletePool)
	handle(h, auth.ActionRead, "GET /api/sandboxes", "list-sandboxes", "List sandboxes", h.listSandboxes)
	handle(h, auth.ActionRead, "GET /api/sandboxes/{id}", "get-sandbox", "Read a sandbox", h.getSandbox)
	handle(h, auth.ActionStopSandbox, "POST /api/sandboxes/{id}/stop", "stop-sandbox", "Stop a sandbox", h.stopSandbox)
	handle(h, auth.ActionRead, "GET /api/sandboxes/{id}/log", "get-sandbox-log", "Read the console and runner log of a sandbox", h.sandboxLog, textResponse)
	handle(h, auth.ActionOpenShell, "GET /api/sandboxes/{id}/shell", "open-sandbox-shell", "Open the debug shell (WebSocket)", h.openShell, websocketResponse,
		describe("Only a debug sandbox has a shell, while its job runs and during its hold after the job."))

	handle(h, auth.ActionRead, "GET /api/jobs", "list-jobs", "List jobs, newest first", h.listJobs)
	handle(h, auth.ActionRead, "GET /api/jobs/facets", "get-job-facets", "List the values for the job filters", h.getJobFacets)
	handle(h, auth.ActionRead, "GET /api/jobs/{id}", "get-job", "Read a job", h.getJob)
	handle(h, auth.ActionRead, "GET /api/jobs/{id}/log", "get-job-log", "Read the log of a job", h.jobLog, jobLogResponse)
	handle(h, auth.ActionRead, "GET /api/jobs/{id}/metrics", "get-job-metrics", "Read the CPU and memory samples of a job", h.getJobMetrics)
	handle(h, auth.ActionRerunJob, "POST /api/jobs/{id}/rerun", "rerun-job", "Run a job again", h.rerunJob)

	handle(h, auth.ActionRead, "GET /api/servers", "list-servers", "List servers with live stats", h.listServers)
	handle(h, auth.ActionAdmin, "POST /api/servers/tokens", "create-server-token", "Make an install token for a new server", h.createServerToken)
	handle(h, auth.ActionAdmin, "GET /api/servers/tokens/{id}/status", "get-server-token-status", "Read whether an agent used an install token", h.getServerTokenStatus)
	handle(h, auth.ActionAdmin, "POST /api/servers/ssh", "install-server-ssh", "Install a new server over SSH", h.installServerSSH)
	handle(h, auth.ActionAdmin, "GET /api/servers/ssh-key", "get-server-ssh-key", "Read the runplane SSH public key", h.getSSHKey)
	handle(h, auth.ActionRead, "GET /api/servers/{id}", "get-server", "Read a server", h.getServer)
	handle(h, auth.ActionAdmin, "PATCH /api/servers/{id}", "update-server", "Rename a server or change its status", h.updateServer)
	handle(h, auth.ActionAdmin, "DELETE /api/servers/{id}", "delete-server", "Remove a server", h.deleteServer)
	handle(h, auth.ActionAdmin, "POST /api/servers/{id}/update", "update-agent", "Install the release of the primary on an agent", h.updateAgent,
		describe("The update runs in the background. update_state of the server shows it, and server.state events follow it."))
	handle(h, auth.ActionRead, "GET /api/servers/{id}/doctor", "get-server-doctor", "Read the last doctor result of a server", h.getDoctor)
	handle(h, auth.ActionAdmin, "POST /api/servers/{id}/doctor", "run-server-doctor", "Run the doctor checks on a server and keep the result", h.runDoctor)

	handle(h, auth.ActionRead, "GET /api/images", "list-images", "List images with the build status on each server", h.listImages)
	handle(h, auth.ActionAdmin, "POST /api/images", "create-image", "Add an image", h.createImage)
	handle(h, auth.ActionAdmin, "PATCH /api/images/{id}", "update-image", "Change an image or make it the default", h.updateImage)
	handle(h, auth.ActionAdmin, "DELETE /api/images/{id}", "delete-image", "Remove an image", h.deleteImage)
	handle(h, auth.ActionAdmin, "POST /api/images/{id}/refresh", "refresh-image", "Pull an image again", h.refreshImage)
	handle(h, auth.ActionRead, "GET /api/caches", "get-caches", "Read the cache use of each repository and the registry mirror", h.getCaches)
	handle(h, auth.ActionRead, "GET /api/caches/entries", "list-cache-entries", "List cache entries", h.listCacheEntries)
	handle(h, auth.ActionAdmin, "DELETE /api/caches", "delete-cache-entries", "Remove cache entries", h.deleteCacheEntries)
	handle(h, auth.ActionRead, "GET /api/runner-versions", "list-runner-versions", "List the runner bundles", h.listRunnerVersions)

	handle(h, auth.ActionRead, "GET /api/settings", "get-settings", "Read the settings", h.getSettings)
	handle(h, auth.ActionAdmin, "PATCH /api/settings", "update-settings", "Change settings", h.updateSettings)
	handle(h, auth.ActionRead, "GET /api/settings/dns", "get-domain-dns", "Check that the DNS record of the domain points to this server", h.getDomainDNS)
	handle(h, auth.ActionAdmin, "GET /api/notifications", "list-notifications", "List notification channels", h.listNotifications)
	handle(h, auth.ActionAdmin, "POST /api/notifications", "create-notification", "Add a notification channel", h.createNotification)
	handle(h, auth.ActionAdmin, "PATCH /api/notifications/{id}", "update-notification", "Change a notification channel", h.updateNotification)
	handle(h, auth.ActionAdmin, "DELETE /api/notifications/{id}", "delete-notification", "Remove a notification channel", h.deleteNotification)
	handle(h, auth.ActionAdmin, "POST /api/notifications/test", "test-notification-channel", "Send a test message to a channel that is not saved", h.testChannel)
	handle(h, auth.ActionAdmin, "POST /api/notifications/{id}/test", "test-notification", "Send a test message", h.testNotification)
	handle(h, auth.ActionAdmin, "GET /api/users", "list-users", "List users and open invites", h.listUsers)
	handle(h, auth.ActionAdmin, "PATCH /api/users/{id}", "update-user", "Change the role of a user", h.updateUser)
	handle(h, auth.ActionAdmin, "DELETE /api/users/{id}", "delete-user", "Delete a user", h.deleteUser)
	handle(h, auth.ActionAdmin, "POST /api/invites", "create-invite", "Invite a user", h.createInvite)
	handle(h, auth.ActionAdmin, "DELETE /api/invites/{id}", "delete-invite", "Withdraw an invite", h.deleteInvite)
	handle(h, auth.ActionAdmin, "POST /api/invites/{id}/link", "create-invite-link", "Replace an open invite with a new link", h.newInviteLink,
		describe("The invite gets a new ID and expiry, and the old link stops working."))
	handle(h, public, "GET /api/invites/{id}", "get-invite", "Read an invite by its token", h.getInvite)
	handle(h, public, "POST /api/invites/{id}", "accept-invite", "Accept an invite by its token", h.acceptInvite)
	handle(h, auth.ActionAdmin, "GET /api/audit", "list-audit", "List the audit log, newest first", h.listAudit)
	handle(h, auth.ActionAdmin, "GET /api/backups", "list-backups", "List backups", h.listBackups)
	handle(h, auth.ActionAdmin, "POST /api/backups", "create-backup", "Make a backup now", h.createBackup)
	handle(h, auth.ActionAdmin, "GET /api/backups/{id}/download", "download-backup", "Download a backup file", h.downloadBackup, binaryResponse("application/gzip"))
	handle(h, auth.ActionAdmin, "GET /api/update", "get-update", "Read the update status", h.getUpdate)
	handle(h, auth.ActionAdmin, "POST /api/update", "apply-update", "Install the latest release", h.applyUpdate)

	h.registerEvents()
	handle(h, public, "GET /install.sh", "get-install-script", "Read the install script", h.installScript, binaryResponse("text/x-shellscript"))
}

// public marks a route that needs no session. The other routes need the
// auth.Action in their metadata.
const public = auth.Action(-1)

const actionKey = "runplane.action"

// handle registers a Huma operation. pattern is "METHOD /path". An error that
// is not a huma.StatusError goes to the log, and the client gets a 500
// problem without its text.
func handle[I, O any](h *Handler, a auth.Action, pattern, id, summary string, fn func(context.Context, *I) (*O, error), opts ...func(*huma.Operation)) {
	huma.Register(h.api, operation(a, pattern, id, summary, opts...), func(ctx context.Context, in *I) (*O, error) {
		out, err := fn(ctx, in)
		var se huma.StatusError
		if err != nil && !errors.As(err, &se) {
			h.d.Log.Error("request failed", "err", err)
			return nil, huma.Error500InternalServerError("Something went wrong. The server log has the details.")
		}
		return out, err
	})
}

func operation(a auth.Action, pattern, id, summary string, opts ...func(*huma.Operation)) huma.Operation {
	method, path, _ := strings.Cut(pattern, " ")
	tag, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(path, "/api"), "/"), "/")
	op := huma.Operation{
		OperationID: id,
		Method:      method,
		Path:        path,
		Summary:     summary,
		Tags:        []string{tag},
		Metadata:    map[string]any{actionKey: a},
	}
	if a != public {
		op.Security = []map[string][]string{{"session": {}}}
	}
	for _, o := range opts {
		o(&op)
	}
	return op
}

func describe(text string) func(*huma.Operation) {
	return func(op *huma.Operation) { op.Description = text }
}

func textResponse(op *huma.Operation) {
	op.Responses = map[string]*huma.Response{"200": {
		Description: "The log as plain text. With follow=1, the response streams until the log ends.",
		Content:     map[string]*huma.MediaType{"text/plain": {Schema: &huma.Schema{Type: huma.TypeString}}},
	}}
}

func jobLogResponse(op *huma.Operation) {
	textResponse(op)
	op.Responses["200"].Headers = map[string]*huma.Param{stepFilterHeader: {
		Description: "ignored: the log has all steps, because the job runs and its live log has no step marks.",
		Schema:      &huma.Schema{Type: huma.TypeString, Enum: []any{"ignored"}},
	}}
}

func binaryResponse(contentType string) func(*huma.Operation) {
	return func(op *huma.Operation) {
		op.Responses = map[string]*huma.Response{"200": {
			Description: "OK",
			Content:     map[string]*huma.MediaType{contentType: {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}}},
		}}
	}
}

func websocketResponse(op *huma.Operation) {
	op.DefaultStatus = http.StatusSwitchingProtocols
	op.Responses = map[string]*huma.Response{"101": {
		Description: "The connection becomes a WebSocket. Binary frames carry the terminal bytes in both directions. " +
			`The client resizes the terminal with a text frame {"type":"resize","rows":N,"cols":N}.`,
	}}
}

func redirectResponse(op *huma.Operation) {
	op.DefaultStatus = http.StatusFound
	op.Description = "The browser comes here from GitHub. The response sends it on to GitHub or to the dashboard. " +
		"Errors go to the dashboard page in the query parameter error."
}

// PathID is the id path parameter. Input structs embed it; Huma finds
// parameters only in exported embedded types.
type PathID struct {
	ID string `path:"id" maxLength:"100"`
}

// body is the input or output of an operation with a JSON body.
type body[T any] struct{ Body T }

// empty is the output of operations without a response body (204).
type empty struct{}

// reply returns v as the response body, or err.
func reply[T any](v T, err error) (*body[T], error) {
	if err != nil {
		return nil, err
	}
	return &body[T]{v}, nil
}

// first returns the item of a converted list of one.
func first[T any](list []T, err error) (T, error) {
	if err != nil {
		var zero T
		return zero, err
	}
	return list[0], nil
}

// orEmpty returns list, or an empty slice for nil, so JSON has [] and not null.
func orEmpty[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}

// set copies *v to dst if v is not nil.
func set[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}

// refs holds the pools and servers by ID, for the labels and names that
// API objects show next to the IDs. Both tables are small.
type refs struct {
	pools   map[string]store.Pool
	servers map[string]store.Server
}

func (h *Handler) loadRefs(ctx context.Context) (refs, error) {
	pools, err := h.d.Store.ListPools(ctx)
	if err != nil {
		return refs{}, err
	}
	servers, err := h.d.Store.ListServers(ctx)
	if err != nil {
		return refs{}, err
	}
	r := refs{pools: make(map[string]store.Pool, len(pools)), servers: make(map[string]store.Server, len(servers))}
	for _, p := range pools {
		r.pools[p.ID] = p
	}
	for _, v := range servers {
		r.servers[v.ID] = v
	}
	return r, nil
}

// notFound maps store.ErrNotFound to a 404 with msg.
func notFound(err error, msg string) error {
	if errors.Is(err, store.ErrNotFound) {
		return huma.Error404NotFound(msg)
	}
	return err
}

// serviceErr shows the error of a service to the user.
func (h *Handler) serviceErr(err error) error {
	var se huma.StatusError
	switch {
	case errors.As(err, &se):
		return se
	case errors.Is(err, store.ErrNotFound):
		return huma.Error404NotFound(err.Error())
	case errors.Is(err, context.Canceled):
		return huma.Error503ServiceUnavailable("The request was cancelled.")
	}
	h.d.Log.Error("service failed", "err", err)
	return huma.Error500InternalServerError(err.Error())
}

func unavailable(what string) error {
	return huma.Error503ServiceUnavailable(what + " is not available in this runplane process.")
}

// invalid returns a 422 problem with one field error. loc is for example "body.label".
func invalid(loc, msg string, value any) error {
	return huma.Error422UnprocessableEntity(msg, &huma.ErrorDetail{Location: loc, Message: msg, Value: value})
}

// conflict returns a 409 problem with one field error.
func conflict(loc, msg string) error {
	return huma.Error409Conflict(msg, &huma.ErrorDetail{Location: loc, Message: msg})
}

// problems collects field errors for one 422 problem.
type problems []error

func (p *problems) add(loc, msg string, value any) {
	*p = append(*p, &huma.ErrorDetail{Location: loc, Message: msg, Value: value})
}

func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	if len(p) == 1 {
		return huma.Error422UnprocessableEntity(p[0].(*huma.ErrorDetail).Message, p...)
	}
	return huma.Error422UnprocessableEntity("Some values are not valid.", p...)
}
