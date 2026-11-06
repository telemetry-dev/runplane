package api

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/store"
)

func TestGitHubLinks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	must(t, e.st.SaveGitHubApp(ctx, store.GitHubApp{
		ID: "a", AppID: 5, Slug: "runplane-acme", Name: "runplane acme", OwnerLogin: "acme", OwnerType: "Organization",
		HTMLURL: "https://github.com/apps/runplane-acme", PrivateKeyEnc: []byte{1}, CreatedAt: now,
	}))
	for _, inst := range []store.Installation{
		{ID: 77, AppID: 5, AccountLogin: "acme", AccountType: "Organization", CreatedAt: now, UpdatedAt: now},
		{ID: 78, AppID: 5, AccountLogin: "octo", AccountType: "User", CreatedAt: now, UpdatedAt: now},
	} {
		must(t, e.st.UpsertInstallation(ctx, inst))
	}
	must(t, e.st.UpsertScaleSet(ctx, store.ScaleSet{ID: "ss", PoolID: "p", InstallationID: 77, RunnerGroupID: 1, Name: "build",
		Status: store.ScaleSetListening, UpdatedAt: now}))

	var gh GitHubStatus
	e.login("ada@example.com", auth.RoleMember).do("GET", "/api/github", nil, &gh).want(t, 200)
	for _, c := range []struct{ name, got, want string }{
		{"app settings", gh.App.SettingsURL, "https://github.com/organizations/acme/settings/apps/runplane-acme"},
		{"app permissions", gh.App.PermissionsURL, "https://github.com/organizations/acme/settings/apps/runplane-acme/permissions"},
		{"app install", gh.App.InstallURL, "https://github.com/apps/runplane-acme/installations/new"},
		{"organization installation", gh.Installations[0].HTMLURL, "https://github.com/organizations/acme/settings/installations/77"},
		{"personal installation", gh.Installations[1].HTMLURL, "https://github.com/settings/installations/78"},
		{"runner group", gh.ScaleSets[0].RunnerGroup, "Default"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// The pull request form lists the repositories whose jobs runplane takes,
// so that the jobs of a pull request start.
func TestRepositoriesTakeJobs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	must(t, e.st.SaveGitHubApp(ctx, store.GitHubApp{ID: "a", AppID: 5, Slug: "runplane-acme", Name: "runplane acme",
		OwnerLogin: "acme", OwnerType: "Organization", PrivateKeyEnc: []byte{1}, CreatedAt: now}))
	for _, inst := range []store.Installation{
		{ID: 77, AppID: 5, AccountLogin: "acme", AccountType: "Organization", CreatedAt: now, UpdatedAt: now},
		{ID: 78, AppID: 5, AccountLogin: "octo", AccountType: "User", SuspendedAt: &now, CreatedAt: now, UpdatedAt: now},
	} {
		must(t, e.st.UpsertInstallation(ctx, inst))
	}
	must(t, e.st.ReplaceRepositories(ctx, 77, []store.Repository{
		{ID: 1, FullName: "acme/api", Private: true, UpdatedAt: now},
		{ID: 2, FullName: "acme/site", UpdatedAt: now},
		{ID: 3, FullName: "acme/old", Private: true, Archived: true, UpdatedAt: now},
	}))
	must(t, e.st.ReplaceRepositories(ctx, 78, []store.Repository{{ID: 4, FullName: "octo/app", Private: true, UpdatedAt: now}}))

	var gh GitHubStatus
	e.login("ada@example.com", auth.RoleMember).do("GET", "/api/github", nil, &gh).want(t, 200)
	takes := map[string]bool{}
	for _, inst := range gh.Installations {
		for _, r := range inst.Repositories {
			takes[r.FullName] = r.TakesJobs
		}
	}
	want := map[string]bool{"acme/api": true, "acme/site": false, "acme/old": false, "octo/app": false}
	if !maps.Equal(takes, want) {
		t.Errorf("takes_jobs = %v, want %v", takes, want)
	}
}

// fakeGitHub opens pull requests to the label big. The tests use no other
// GitHub method.
type fakeGitHub struct {
	GitHub
	opened []string // "repository label" of each call
}

func (f *fakeGitHub) OpenPullRequest(_ context.Context, _, repository, label string) (int, string, error) {
	if label != "big" {
		return 0, "", field.Problems{{Key: "label", Message: "No enabled pool has this label.", Value: label}}
	}
	f.opened = append(f.opened, repository+" "+label)
	return 12, "https://github.com/" + repository + "/pull/12", nil
}

// The form shows a refused pull request at its field. Only an admin can
// write to repositories.
func TestCreatePullRequest(t *testing.T) {
	gh := &fakeGitHub{}
	e := newEnv(t, func(d *Deps) { d.GitHub = gh })
	const path = "/api/github/pull-requests"
	e.login("bob@example.com", auth.RoleMember).do("POST", path, map[string]any{"repository": "acme/web", "label": "big"}, nil).want(t, 403)
	admin := e.login("ada@example.com", auth.RoleAdmin)
	admin.do("POST", path, map[string]any{"repository": "acme/web", "label": "off"}, nil).wantField(t, 422, "body.label")

	var pr PullRequest
	admin.do("POST", path, map[string]any{"repository": "acme/web", "label": "big"}, &pr).want(t, 200)
	if pr.Number != 12 || pr.HTMLURL != "https://github.com/acme/web/pull/12" || !slices.Equal(gh.opened, []string{"acme/web big"}) {
		t.Errorf("pull request = %+v after the calls %q, want number 12 from one call for acme/web with big", pr, gh.opened)
	}
}

func TestGitHubAndBackupEvents(t *testing.T) {
	e := newEnv(t)
	member := e.login("bob@example.com", auth.RoleMember).events(t)
	admin := e.login("ada@example.com", auth.RoleAdmin).events(t)
	backup := events.BackupState{ID: "bk1", CreatedAt: time.Now(), Status: "running"}
	e.publish(backup, events.GitHubState{})

	for seen := 0; seen < 2; {
		ev := recv(t, member)
		switch ev.name {
		case "backup.state":
			t.Fatalf("a member got backup.state: %s", ev.data)
		case "github.state":
			var gh GitHubStatus
			if err := json.Unmarshal([]byte(ev.data), &gh); err != nil || gh.App != nil || gh.Installations == nil {
				t.Fatalf("github.state data %s: want the GitHub status without an app", ev.data)
			}
			seen++
		}
	}
	for {
		ev := recv(t, admin)
		if ev.name != "backup.state" {
			continue
		}
		var b Backup
		if err := json.Unmarshal([]byte(ev.data), &b); err != nil || b.ID != "bk1" || b.Status != "running" {
			t.Fatalf("backup.state data %s: want the running backup bk1", ev.data)
		}
		return
	}
}
