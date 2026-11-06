package pages

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Each link of runplane must open a page of the web router, and the page
// must read the search params of the link. A rename on one side breaks the
// links in notifications, invites, and the GitHub flows without an error.
func TestLinksOpenPagesOfTheWebRouter(t *testing.T) {
	routes := webRoutes(t)
	const (
		login    = "../../web/src/routes/_setup/login.tsx"
		callback = "../../web/src/components/pages/github/callback.ts"
	)
	for _, link := range []struct {
		url string
		// search is the file that declares the search params of the page.
		search string
	}{
		{url: Jobs},
		{url: Job("$")},
		{url: Server("$")},
		{url: Invite("$")},
		{url: GeneralSettings},
		{url: GitHubSettings},
		{url: NotificationSettings},
		{url: RunnerSettings},
		{url: FirstJob},
		{url: Login("/jobs"), search: login},
		{url: GitHubRequested(false), search: callback},
		{url: GitHubRequested(true), search: callback},
		{url: GitHubError(false, "x"), search: callback},
		{url: GitHubError(true, "x"), search: callback},
	} {
		u, err := url.Parse(link.url)
		if err != nil {
			t.Fatal(err)
		}
		if !routes[u.Path] {
			t.Errorf("%s: web/src/routeTree.gen.ts has no route %s", link.url, u.Path)
		}
		if len(u.Query()) == 0 {
			continue
		}
		src, err := os.ReadFile(link.search)
		if err != nil {
			t.Fatal(err)
		}
		for key := range u.Query() {
			if !regexp.MustCompile(`\b` + key + `\?:`).Match(src) {
				t.Errorf("%s: %s declares no search param %s", link.url, link.search, key)
			}
		}
	}
}

// webRoutes returns the paths that the web router opens, with "$" in the
// place of each path param: /jobs/$jobId is /jobs/$.
func webRoutes(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("../../web/src/routeTree.gen.ts")
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(src), "export interface FileRoutesByTo {")
	block, _, _ = strings.Cut(block, "}")
	if !ok {
		t.Fatal("routeTree.gen.ts has no FileRoutesByTo")
	}
	param := regexp.MustCompile(`\$\w+`)
	routes := map[string]bool{}
	for _, m := range regexp.MustCompile(`'(/[^']*)':`).FindAllStringSubmatch(block, -1) {
		routes[param.ReplaceAllString(m[1], "$$")] = true
	}
	return routes
}
