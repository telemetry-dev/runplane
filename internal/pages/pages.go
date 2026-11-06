// Package pages gives the paths of the dashboard pages that runplane links
// to from outside the dashboard: notifications, invites, and the ends of the
// GitHub flows. The web router declares the pages in web/src/routes, and the
// test of this package finds each path in web/src/routeTree.gen.ts.
package pages

import "net/url"

const (
	GeneralSettings      = "/settings/general"
	GitHubSettings       = "/settings/github"
	NotificationSettings = "/settings/notifications"
	RunnerSettings       = "/settings/runners"
	// FirstJob is the last setup step, after GitHub connects the first time.
	FirstJob = "/setup/first-job"
)

func Job(id string) string { return "/jobs/" + id }

func Server(id string) string { return "/servers/" + id }

func Invite(token string) string { return "/invite/" + token }

// Login is the login page that opens redirect after the login.
func Login(redirect string) string {
	return "/login?" + url.Values{"redirect": {redirect}}.Encode()
}

// GitHubRequested is the GitHub page that tells that an organization owner
// must approve the install request.
func GitHubRequested(connected bool) string {
	return gitHub(connected) + "?requested=1"
}

// GitHubError is the GitHub page that shows why a GitHub flow stopped.
func GitHubError(connected bool, msg string) string {
	return gitHub(connected) + "?" + url.Values{"error": {msg}}.Encode()
}

// gitHub is the setup step before GitHub is connected, and the GitHub
// settings after.
func gitHub(connected bool) string {
	if connected {
		return GitHubSettings
	}
	return "/setup/github"
}
