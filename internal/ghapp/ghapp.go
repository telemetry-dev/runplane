// Package ghapp holds the facts of the GitHub App of runplane that need no
// call to GitHub: the account types, the permissions of the app, the labels
// that its pull request moves, and the web pages of an account.
package ghapp

import (
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/runplane/runplane/images"
)

// The account types of GitHub that runplane runs jobs for. An organization
// has runners for all of its repositories. A personal account has runners
// only for each repository.
const (
	Organization = "Organization"
	User         = "User"
)

// DefaultRunnerGroup is the ID of the "Default" runner group. A free
// organization can only use this group, and a repository has only this one.
const DefaultRunnerGroup = 1

type Account struct {
	Type  string `json:"type"` // Organization, User, or another GitHub account type
	Login string `json:"login"`
}

// Supported tells if runplane can run jobs for the account type.
func (a Account) Supported() bool { return a.Type == Organization || a.Type == User }

// Permission is a permission of a GitHub App, with its access: read or write.
type Permission struct{ Name, Access string }

// AppPermissions are the permissions of the GitHub App of runplane on the
// account. An organization uses organization runners. A personal account
// can only use repository runners, which need administration.
func (a Account) AppPermissions() []Permission {
	runners := Permission{"administration", "write"}
	if a.Type == Organization {
		runners = Permission{"organization_self_hosted_runners", "write"}
	}
	return slices.Concat([]Permission{runners, {"actions", "write"}}, PullPermissions, []Permission{{"metadata", "read"}})
}

// PullPermissions let runplane open the pull request that moves the jobs of
// a repository to runplane. A change in .github/workflows needs workflows
// next to contents.
var PullPermissions = []Permission{{"contents", "write"}, {"pull_requests", "write"}, {"workflows", "write"}}

// ErrNoPullPermissions tells that the account of an installation has not
// accepted PullPermissions. An app from before runplane asked for them gets
// them in its settings on GitHub.
var ErrNoPullPermissions = errors.New("the GitHub App needs read and write access to Contents, Pull requests, and Workflows")

// HostedLabels are the runs-on labels of GitHub-hosted runners that the
// pull request of runplane moves to a pool: ubuntu-latest, and the label of
// the Ubuntu release of the default image. A builtin image has the name of
// the GitHub label of its release.
var HostedLabels = []string{"ubuntu-latest", images.Default}

// HostedLabelsText gives HostedLabels for a sentence, as "a or b", with
// quote on the two sides of each label.
func HostedLabelsText(quote string) string {
	quoted := make([]string, len(HostedLabels))
	for i, l := range HostedLabels {
		quoted[i] = quote + l + quote
	}
	return strings.Join(quoted, " or ")
}

// NewAppURL is the page on the web host web where the account makes a new
// app from a manifest. state comes back with the manifest code.
func (a Account) NewAppURL(web, state string) string {
	return a.settingsURL(web) + "/apps/new?state=" + url.QueryEscape(state)
}

// AppURL is the settings page of the app slug of the account: its name,
// permissions, and keys.
func (a Account) AppURL(web, slug string) string {
	return a.settingsURL(web) + "/apps/" + url.PathEscape(slug)
}

// AppPermissionsURL is the page where the account changes the permissions of
// the app slug.
func (a Account) AppPermissionsURL(web, slug string) string {
	return a.AppURL(web, slug) + "/permissions"
}

// AppDeleteURL is the page with the "Delete GitHub App" button of the app
// slug.
func (a Account) AppDeleteURL(web, slug string) string {
	return a.AppURL(web, slug) + "/advanced"
}

// InstallationURL is the settings page of the installation id on the
// account, where the account owner chooses the repositories.
func (a Account) InstallationURL(web string, id int64) string {
	return a.settingsURL(web) + "/installations/" + strconv.FormatInt(id, 10)
}

// AppInstallURL is the page that installs the app slug on more accounts or
// repositories.
func AppInstallURL(web, slug string) string {
	return web + "/apps/" + url.PathEscape(slug) + "/installations/new"
}

// settingsURL is the settings page of the account. The pages of its apps
// and of its installations are below it.
func (a Account) settingsURL(web string) string {
	if a.Type == Organization {
		return web + "/organizations/" + url.PathEscape(a.Login) + "/settings"
	}
	return web + "/settings"
}
