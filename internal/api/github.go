package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/ghrest"
	"github.com/runplane/runplane/internal/pages"
	"github.com/runplane/runplane/internal/store"
)

type GitHubStatus struct {
	Connected      bool             `json:"connected" doc:"An app with at least one installation exists."`
	App            *GitHubApp       `json:"app,omitempty" doc:"Absent before the manifest flow."`
	Installations  []Installation   `json:"installations"`
	ScaleSets      []ScaleSetStatus `json:"scale_sets" doc:"One for each pool and installation, or for each pool and repository of a personal account."`
	AppPermissions AppPermissions   `json:"app_permissions" doc:"The permissions that the manifest of a new app asks for."`
}

// AppPermissions has the permissions of the app for each account_type of
// ManifestRequest.
type AppPermissions struct {
	Organization []AppPermission `json:"organization"`
	User         []AppPermission `json:"user"`
}

type AppPermission struct {
	Name   string `json:"name" enum:"organization_self_hosted_runners,administration,actions,contents,pull_requests,workflows,metadata"`
	Access string `json:"access" enum:"read,write"`
}

type GitHubApp struct {
	AppID          int64     `json:"app_id"`
	Slug           string    `json:"slug"`
	Name           string    `json:"name"`
	ClientID       string    `json:"client_id"`
	OwnerLogin     string    `json:"owner_login"`
	OwnerType      string    `json:"owner_type" doc:"Organization or User."`
	HTMLURL        string    `json:"html_url" doc:"Public page of the app on GitHub."`
	SettingsURL    string    `json:"settings_url" doc:"Settings page of the app on GitHub: name, permissions, and keys."`
	PermissionsURL string    `json:"permissions_url" doc:"Permissions page of the app on GitHub. An app from an earlier runplane release gets the pull request permissions there."`
	InstallURL     string    `json:"install_url" doc:"GitHub page to install the app on more accounts or repositories."`
	CreatedAt      time.Time `json:"created_at"`
}

type Installation struct {
	ID                  int64        `json:"id" doc:"GitHub installation ID."`
	AccountLogin        string       `json:"account_login"`
	AccountType         string       `json:"account_type" enum:"Organization,User"`
	AvatarURL           string       `json:"avatar_url"`
	RepositorySelection string       `json:"repository_selection" enum:"all,selected"`
	RepositoryCount     int          `json:"repository_count"`
	Repositories        []Repository `json:"repositories"`
	Suspended           bool         `json:"suspended"`
	SuspendedAt         *time.Time   `json:"suspended_at"`
	HTMLURL             string       `json:"html_url" doc:"Settings page of the installation on GitHub, where the account owner chooses the repositories."`
}

type Repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name" doc:"owner/name"`
	Private  bool   `json:"private" doc:"runplane rejects jobs of public repositories unless allow_public_repos is on in VM mode."`
	Archived bool   `json:"archived"`
}

type ScaleSetStatus struct {
	ID             string        `json:"id"`
	PoolID         string        `json:"pool_id"`
	PoolLabel      string        `json:"pool_label" doc:"Empty while runplane deletes the scale set of a deleted pool."`
	InstallationID int64         `json:"installation_id"`
	AccountLogin   string        `json:"account_login"`
	Repository     string        `json:"repository" doc:"owner/name for a personal account. Empty for an organization."`
	RunnerGroup    string        `json:"runner_group" doc:"Runner group of the scale set on GitHub: Default, where runplane makes its scale sets, or \"Group <ID>\"."`
	Status         string        `json:"status" enum:"creating,listening,error,deleting"`
	Error          string        `json:"error"`
	LastPollAt     *time.Time    `json:"last_poll_at" doc:"Last message from the listener."`
	Stats          ScaleSetStats `json:"stats" doc:"Statistics of the last listener message."`
}

type ScaleSetStats struct {
	AvailableJobs     int `json:"available_jobs"`
	AcquiredJobs      int `json:"acquired_jobs"`
	AssignedJobs      int `json:"assigned_jobs"`
	RunningJobs       int `json:"running_jobs"`
	RegisteredRunners int `json:"registered_runners"`
	BusyRunners       int `json:"busy_runners"`
	IdleRunners       int `json:"idle_runners"`
}

func (h *Handler) gitHubStatus(ctx context.Context) (GitHubStatus, error) {
	st := h.d.Store
	out := GitHubStatus{Installations: []Installation{}, ScaleSets: []ScaleSetStatus{}, AppPermissions: AppPermissions{
		Organization: appPermissions(ghrest.Account{Type: ghrest.Organization}),
		User:         appPermissions(ghrest.Account{Type: ghrest.User}),
	}}
	app, err := st.GetGitHubApp(ctx)
	switch {
	case err == nil:
		owner := ghrest.Account{Type: app.OwnerType, Login: app.OwnerLogin}
		out.App = &GitHubApp{
			AppID: app.AppID, Slug: app.Slug, Name: app.Name, ClientID: app.ClientID, OwnerLogin: app.OwnerLogin,
			OwnerType: app.OwnerType, HTMLURL: app.HTMLURL, CreatedAt: app.CreatedAt,
			SettingsURL:    owner.AppURL(ghrest.Web, app.Slug),
			PermissionsURL: owner.AppURL(ghrest.Web, app.Slug) + "/permissions",
			InstallURL:     ghrest.AppInstallURL(ghrest.Web, app.Slug),
		}
	case !errors.Is(err, store.ErrNotFound):
		return GitHubStatus{}, err
	}
	installs, err := st.ListInstallations(ctx)
	if err != nil {
		return GitHubStatus{}, err
	}
	accounts := map[int64]string{}
	repoNames := map[int64]string{}
	for _, inst := range installs {
		repos, err := st.ListRepositories(ctx, inst.ID)
		if err != nil {
			return GitHubStatus{}, err
		}
		out.Installations = append(out.Installations, Installation{
			ID: inst.ID, AccountLogin: inst.AccountLogin, AccountType: inst.AccountType, AvatarURL: inst.AvatarURL,
			RepositorySelection: inst.RepositorySelection, RepositoryCount: len(repos), Repositories: make([]Repository, len(repos)),
			Suspended: inst.SuspendedAt != nil, SuspendedAt: inst.SuspendedAt,
			HTMLURL: ghrest.Account{Type: inst.AccountType, Login: inst.AccountLogin}.InstallationURL(ghrest.Web, inst.ID),
		})
		list := out.Installations[len(out.Installations)-1].Repositories
		for i, r := range repos {
			list[i] = Repository{ID: r.ID, FullName: r.FullName, Private: r.Private, Archived: r.Archived}
			repoNames[r.ID] = r.FullName
		}
		accounts[inst.ID] = inst.AccountLogin
	}
	out.Connected = out.App != nil && len(installs) > 0

	sets, err := st.ListScaleSets(ctx)
	if err != nil {
		return GitHubStatus{}, err
	}
	r, err := h.loadRefs(ctx)
	if err != nil {
		return GitHubStatus{}, err
	}
	for _, x := range sets {
		repo := ""
		if x.RepositoryID != nil {
			repo = repoNames[*x.RepositoryID]
		}
		out.ScaleSets = append(out.ScaleSets, ScaleSetStatus{
			ID: x.ID, PoolID: x.PoolID, PoolLabel: r.pools[x.PoolID].Label, InstallationID: x.InstallationID,
			AccountLogin: accounts[x.InstallationID], Repository: repo, RunnerGroup: runnerGroup(x.RunnerGroupID),
			Status: x.Status, Error: x.Error,
			LastPollAt: x.LastPollAt, Stats: ScaleSetStats(x.Stats),
		})
	}
	return out, nil
}

// runnerGroup names the runner group of a scale set. runplane makes its
// scale sets in the group Default.
func runnerGroup(id int64) string {
	if id <= ghrest.DefaultRunnerGroup {
		return "Default"
	}
	return "Group " + strconv.FormatInt(id, 10)
}

func appPermissions(owner ghrest.Account) []AppPermission {
	list := owner.AppPermissions()
	out := make([]AppPermission, len(list))
	for i, p := range list {
		out[i] = AppPermission(p)
	}
	return out
}

func (h *Handler) getGitHub(ctx context.Context, _ *struct{}) (*body[GitHubStatus], error) {
	return reply(h.gitHubStatus(ctx))
}

func (h *Handler) githubConnected(ctx context.Context) (bool, error) {
	if _, err := h.d.Store.GetGitHubApp(ctx); errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	list, err := h.d.Store.ListInstallations(ctx)
	return len(list) > 0, err
}

type ManifestRequest struct {
	AccountType  string `json:"account_type" enum:"organization,user"`
	Organization string `json:"organization,omitempty" maxLength:"39" pattern:"^[A-Za-z0-9][A-Za-z0-9-]*$" doc:"Login of the organization. Required for account_type organization."`
}

type ManifestForm struct {
	ActionURL string `json:"action_url" doc:"GitHub URL. Post a form with the field manifest to it."`
	Manifest  string `json:"manifest" doc:"The app manifest as JSON text, the value of the form field manifest."`
}

func (h *Handler) createManifest(ctx context.Context, in *body[ManifestRequest]) (*body[ManifestForm], error) {
	b := in.Body
	owner := ghrest.Account{Type: ghrest.User}
	if b.AccountType == "organization" {
		if b.Organization == "" {
			return nil, invalid("body.organization", "Enter the organization.", nil)
		}
		owner = ghrest.Account{Type: ghrest.Organization, Login: b.Organization}
	}
	base, err := h.baseURL(ctx)
	if err != nil {
		return nil, err
	}
	action, manifest, err := h.d.GitHub.Manifest(ctx, base, owner)
	if err != nil {
		return nil, serviceErr(err)
	}
	h.record(ctx, audit.GitHubManifest, "", map[string]any{"account_type": b.AccountType, "organization": owner.Login})
	return &body[ManifestForm]{ManifestForm{ActionURL: action, Manifest: manifest}}, nil
}

type redirectOutput struct {
	Status   int
	Location string `header:"Location"`
}

func redirect(to string) *redirectOutput {
	return &redirectOutput{Status: http.StatusFound, Location: to}
}

type githubCallbackInput struct {
	Code  string `query:"code" maxLength:"200"`
	State string `query:"state" maxLength:"200"`
}

func (h *Handler) githubCallback(ctx context.Context, in *githubCallbackInput) (*redirectOutput, error) {
	self := "/api/github/callback?" + url.Values{"code": {in.Code}, "state": {in.State}}.Encode()
	ctx, to := h.browserAdmin(ctx, self)
	if to != "" {
		return redirect(to), nil
	}
	to, err := h.d.GitHub.Callback(ctx, in.Code, in.State)
	if err != nil {
		h.d.Log.Warn("github app manifest callback", "err", err)
		return redirect(h.githubErrorPage(ctx, err.Error())), nil
	}
	h.record(ctx, audit.GitHubAppCreate, "", nil)
	return redirect(to), nil
}

type githubSetupInput struct {
	InstallationID int64  `query:"installation_id" minimum:"0"`
	SetupAction    string `query:"setup_action" maxLength:"20" doc:"install, update, or request."`
}

func (h *Handler) githubSetup(ctx context.Context, in *githubSetupInput) (*redirectOutput, error) {
	q := url.Values{"setup_action": {in.SetupAction}}
	if in.InstallationID != 0 {
		q.Set("installation_id", strconv.FormatInt(in.InstallationID, 10))
	}
	ctx, to := h.browserAdmin(ctx, "/api/github/setup?"+q.Encode())
	if to != "" {
		return redirect(to), nil
	}
	to, err := h.d.GitHub.Setup(ctx, in.InstallationID, in.SetupAction)
	if err != nil {
		h.d.Log.Warn("github installation setup", "installation", in.InstallationID, "err", err)
		return redirect(h.githubErrorPage(ctx, err.Error())), nil
	}
	h.record(ctx, audit.GitHubInstallation(in.SetupAction), strconv.FormatInt(in.InstallationID, 10), nil)
	return redirect(to), nil
}

// browserAdmin checks the session of a browser route. Without a session it
// returns the login page that comes back to self; for a member it returns
// an error page. For an admin, it returns ctx with the user.
func (h *Handler) browserAdmin(ctx context.Context, self string) (context.Context, string) {
	u, err := h.d.Auth.UserForToken(ctx, clientOf(ctx).token)
	if err != nil {
		return ctx, pages.Login(self)
	}
	ctx = context.WithValue(ctx, userKey{}, u)
	if !auth.CanDo(u.Role, auth.ActionAdmin) {
		return ctx, h.githubErrorPage(ctx, "Only an admin can connect GitHub.")
	}
	return ctx, ""
}

func (h *Handler) githubErrorPage(ctx context.Context, msg string) string {
	ok, err := h.githubConnected(ctx)
	return pages.GitHubError(err == nil && ok, msg)
}

func (h *Handler) syncGitHub(ctx context.Context, _ *struct{}) (*body[GitHubStatus], error) {
	if err := h.d.GitHub.Sync(ctx); err != nil {
		return nil, serviceErr(err)
	}
	h.record(ctx, audit.GitHubSync, "", nil)
	return reply(h.gitHubStatus(ctx))
}

type CreatePullRequestRequest struct {
	Repository string `json:"repository" maxLength:"140" doc:"owner/name of a repository that the GitHub App can access."`
	Label      string `json:"label" doc:"The label of an enabled pool."`
}

type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url" doc:"Page of the pull request on GitHub."`
}

func (h *Handler) createPullRequest(ctx context.Context, in *body[CreatePullRequestRequest]) (*body[PullRequest], error) {
	b := in.Body
	pools, err := h.d.Store.ListPools(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(pools, func(p store.Pool) bool { return p.Enabled && p.Label == b.Label }) {
		return nil, invalid("body.label", "No enabled pool has this label.", b.Label)
	}
	base, err := h.baseURL(ctx)
	if err != nil {
		return nil, err
	}
	number, htmlURL, err := h.d.GitHub.OpenPullRequest(ctx, base, b.Repository, b.Label)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, huma.Error404NotFound("The GitHub App has no access to " + b.Repository + ".")
	case errors.Is(err, ghrest.ErrNoPullPermissions):
		return nil, huma.Error403Forbidden(field.Sentence(err))
	case err != nil:
		return nil, serviceErr(err)
	}
	h.record(ctx, audit.GitHubPullRequest, b.Repository, map[string]any{"repository": b.Repository, "label": b.Label, "number": number})
	return &body[PullRequest]{PullRequest{Number: number, HTMLURL: htmlURL}}, nil
}

func (h *Handler) disconnectGitHub(ctx context.Context, _ *struct{}) (*empty, error) {
	if err := h.d.GitHub.Disconnect(ctx); err != nil {
		return nil, serviceErr(err)
	}
	h.record(ctx, audit.GitHubDisconnect, "", nil)
	return nil, nil
}
