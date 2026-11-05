// Package api serves the runplane HTTP API, the SSE stream, the debug shell,
// the install script, and the embedded frontend.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/store"
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

	updateMu sync.Mutex
	updating string // state of an update that POST /api/update applies; empty when idle
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

// public marks a route that needs no session. The other routes need the
// auth.Action in their metadata.
const public = auth.Action(-1)

const actionKey = "runplane.action"

// handle registers a Huma operation. pattern is "METHOD /path".
func handle[I, O any](h *Handler, a auth.Action, pattern, id, summary string, fn func(context.Context, *I) (*O, error), opts ...func(*huma.Operation)) {
	huma.Register(h.api, operation(a, pattern, id, summary, opts...), fn)
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

func (h *Handler) routes() {
	// Setup, sessions, and the current user.
	handle(h, public, "GET /api/status", "get-status", "Read the instance status", h.getStatus)
	handle(h, public, "POST /api/setup", "setup", "Make the first admin with the setup code", h.setup)
	handle(h, public, "GET /api/setup/host", "get-setup-host", "Check this server during setup", h.getSetupHost)
	handle(h, auth.ActionRead, "GET /api/session", "get-session", "Read the current user", h.getSession)
	handle(h, public, "POST /api/session", "login", "Log in", h.login)
	handle(h, public, "DELETE /api/session", "logout", "Log out", h.logout)
	handle(h, auth.ActionRead, "PATCH /api/me", "update-me", "Change the name or the password of the current user", h.updateMe)

	handle(h, auth.ActionRead, "GET /api/overview", "get-overview", "Read the overview numbers, running jobs, servers, and chart", h.getOverview)

	// GitHub.
	handle(h, auth.ActionRead, "GET /api/github", "get-github", "Read the GitHub App, installations, and listener status", h.getGitHub)
	handle(h, auth.ActionAdmin, "POST /api/github/manifest", "create-github-manifest", "Start the GitHub App manifest flow", h.createManifest)
	handle(h, public, "GET /api/github/callback", "github-callback", "Finish the manifest flow (browser redirect from GitHub)", h.githubCallback, redirectResponse)
	handle(h, public, "GET /api/github/setup", "github-setup", "Record an installation (browser redirect from GitHub)", h.githubSetup, redirectResponse)
	handle(h, auth.ActionAdmin, "POST /api/github/sync", "sync-github", "Sync the installations now", h.syncGitHub)
	handle(h, auth.ActionAdmin, "DELETE /api/github", "disconnect-github", "Disconnect GitHub", h.disconnectGitHub)

	// Pools and sandboxes.
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

	// Jobs.
	handle(h, auth.ActionRead, "GET /api/jobs", "list-jobs", "List jobs, newest first", h.listJobs)
	handle(h, auth.ActionRead, "GET /api/jobs/facets", "get-job-facets", "List the values for the job filters", h.getJobFacets)
	handle(h, auth.ActionRead, "GET /api/jobs/{id}", "get-job", "Read a job", h.getJob)
	handle(h, auth.ActionRead, "GET /api/jobs/{id}/log", "get-job-log", "Read the log of a job", h.jobLog, jobLogResponse)
	handle(h, auth.ActionRead, "GET /api/jobs/{id}/metrics", "get-job-metrics", "Read the CPU and memory samples of a job", h.getJobMetrics)
	handle(h, auth.ActionRerunJob, "POST /api/jobs/{id}/rerun", "rerun-job", "Run a job again", h.rerunJob)

	// Servers.
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

	// Images, caches, and runner versions.
	handle(h, auth.ActionRead, "GET /api/images", "list-images", "List images with the build status on each server", h.listImages)
	handle(h, auth.ActionAdmin, "POST /api/images", "create-image", "Add an image", h.createImage)
	handle(h, auth.ActionAdmin, "PATCH /api/images/{id}", "update-image", "Change an image or make it the default", h.updateImage)
	handle(h, auth.ActionAdmin, "DELETE /api/images/{id}", "delete-image", "Remove an image", h.deleteImage)
	handle(h, auth.ActionAdmin, "POST /api/images/{id}/refresh", "refresh-image", "Pull an image again", h.refreshImage)
	handle(h, auth.ActionRead, "GET /api/caches", "get-caches", "Read the cache use of each repository and the registry mirror", h.getCaches)
	handle(h, auth.ActionRead, "GET /api/caches/entries", "list-cache-entries", "List cache entries", h.listCacheEntries)
	handle(h, auth.ActionAdmin, "DELETE /api/caches", "delete-cache-entries", "Remove cache entries", h.deleteCacheEntries)
	handle(h, auth.ActionRead, "GET /api/runner-versions", "list-runner-versions", "List the runner bundles", h.listRunnerVersions)

	// Settings and administration.
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

// Error helpers. Handlers return these so that internal error text never
// reaches the client, except the text of service errors.

// internal logs err and returns a 500 problem without the error text.
func (h *Handler) internal(err error) error {
	h.d.Log.Error("request failed", "err", err)
	return huma.Error500InternalServerError("Something went wrong. The server log has the details.")
}

// storeErr maps store.ErrNotFound to a 404 with the message notFound.
func (h *Handler) storeErr(err error, notFound string) error {
	if errors.Is(err, store.ErrNotFound) {
		return huma.Error404NotFound(notFound)
	}
	return h.internal(err)
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

// problems collects field errors for one 422 problem.
type problems []error

func (p *problems) add(loc, msg string, value any) {
	*p = append(*p, &huma.ErrorDetail{Location: loc, Message: msg, Value: value})
}

// locIndex returns the location of item i of a list, for example "body.server_ids[2]".
func locIndex(loc string, i int) string { return fmt.Sprintf("%s[%d]", loc, i) }

func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	if len(p) == 1 {
		return huma.Error422UnprocessableEntity(p[0].(*huma.ErrorDetail).Message, p...)
	}
	return huma.Error422UnprocessableEntity("Some values are not valid.", p...)
}

// PathID is the id path parameter. Input structs embed it; Huma finds
// parameters only in exported embedded types.
type PathID struct {
	ID string `path:"id" maxLength:"100"`
}

// empty is the output of operations without a response body (204).
type empty struct{}

// orEmpty returns list, or an empty slice for nil, so JSON has [] and not null.
func orEmpty[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}
