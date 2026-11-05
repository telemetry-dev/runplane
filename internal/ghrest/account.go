package ghrest

import "net/url"

// The account types of GitHub that runplane runs jobs for. An organization
// has runners for all of its repositories. A personal account has runners
// only for each repository.
const (
	Organization = "Organization"
	User         = "User"
)

// Account is a GitHub organization or personal account.
type Account struct {
	Type  string `json:"type"` // Organization or User
	Login string `json:"login"`
}

// Supported tells if runplane can run jobs for the account type.
func (a Account) Supported() bool { return a.Type == Organization || a.Type == User }

// SettingsURL is the settings page of the account on the web host web. The
// pages of its apps and of its installations are below it.
func (a Account) SettingsURL(web string) string {
	if a.Type == Organization {
		return web + "/organizations/" + url.PathEscape(a.Login) + "/settings"
	}
	return web + "/settings"
}
