package ghrest

import (
	"net/url"
	"strconv"
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
	return []Permission{runners, {"actions", "write"}, {"metadata", "read"}}
}

// SettingsURL is the settings page of the account on the web host web. The
// pages of its apps and of its installations are below it.
func (a Account) SettingsURL(web string) string {
	if a.Type == Organization {
		return web + "/organizations/" + url.PathEscape(a.Login) + "/settings"
	}
	return web + "/settings"
}

// AppURL is the settings page of the app slug of the account: its name,
// permissions, and keys.
func (a Account) AppURL(web, slug string) string {
	return a.SettingsURL(web) + "/apps/" + url.PathEscape(slug)
}

// InstallationURL is the settings page of the installation id on the
// account, where the account owner chooses the repositories.
func (a Account) InstallationURL(web string, id int64) string {
	return a.SettingsURL(web) + "/installations/" + strconv.FormatInt(id, 10)
}

// AppInstallURL is the page that installs the app slug on more accounts or
// repositories.
func AppInstallURL(web, slug string) string {
	return web + "/apps/" + url.PathEscape(slug) + "/installations/new"
}
