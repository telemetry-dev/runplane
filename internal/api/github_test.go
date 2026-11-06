package api

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/store"
)

func TestGitHubLinksFollowTheAppHost(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	must(t, e.st.SaveGitHubApp(ctx, store.GitHubApp{
		ID: "a", AppID: 5, Slug: "runplane-acme", Name: "runplane acme", OwnerLogin: "acme", OwnerType: "Organization",
		HTMLURL: "https://ghe.example.com/apps/runplane-acme", PrivateKeyEnc: []byte{1}, CreatedAt: now,
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
		{"app settings", gh.App.SettingsURL, "https://ghe.example.com/organizations/acme/settings/apps/runplane-acme"},
		{"app install", gh.App.InstallURL, "https://ghe.example.com/apps/runplane-acme/installations/new"},
		{"organization installation", gh.Installations[0].HTMLURL, "https://ghe.example.com/organizations/acme/settings/installations/77"},
		{"personal installation", gh.Installations[1].HTMLURL, "https://ghe.example.com/settings/installations/78"},
		{"runner group", gh.ScaleSets[0].RunnerGroup, "Default"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// fakeGitHub opens pull requests. The tests use no other GitHub method.
type fakeGitHub struct {
	GitHub
	opened []string // "repository label" of each call
}

func (f *fakeGitHub) OpenPullRequest(_ context.Context, _, repository, label string) (int, string, error) {
	f.opened = append(f.opened, repository+" "+label)
	return 12, "https://github.com/" + repository + "/pull/12", nil
}

// A pull request must move jobs to a label that an enabled pool serves, or
// the jobs never start. Only an admin can write to repositories.
func TestCreatePullRequest(t *testing.T) {
	gh := &fakeGitHub{}
	e := newEnv(t, func(d *Deps) { d.GitHub = gh })
	now := time.Now()
	for _, p := range []store.Pool{
		{ID: "p1", Name: "big", Label: "big", Arch: "x64", VCPU: 4, MemoryMB: 8192, DiskGB: 64, MaxRunners: 2, Enabled: true, CreatedAt: now, UpdatedAt: now},
		{ID: "p2", Name: "off", Label: "off", Arch: "x64", VCPU: 2, MemoryMB: 4096, DiskGB: 64, MaxRunners: 2, CreatedAt: now, UpdatedAt: now},
	} {
		must(t, e.st.CreatePool(context.Background(), p))
	}
	const path = "/api/github/pull-requests"
	e.login("bob@example.com", auth.RoleMember).do("POST", path, map[string]any{"repository": "acme/web", "label": "big"}, nil).want(t, 403)
	admin := e.login("ada@example.com", auth.RoleAdmin)
	for _, label := range []string{"off", "gone"} {
		admin.do("POST", path, map[string]any{"repository": "acme/web", "label": label}, nil).wantField(t, 422, "body.label")
	}

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
	backup := store.Backup{ID: "bk1", CreatedAt: time.Now(), Status: "running"}
	e.publish(events.Event{Type: events.BackupState, Data: backup}, events.Event{Type: events.GitHubState})

	for seen := 0; seen < 2; {
		ev := recv(t, member)
		switch ev.name {
		case "backup.state":
			t.Fatalf("a member got backup.state: %s", ev.data)
		case "github.state":
			var gh GitHubStatus
			if err := json.Unmarshal([]byte(ev.data), &gh); err != nil || gh.Connected || gh.Installations == nil {
				t.Fatalf("github.state data %s: want the GitHub status without a connection", ev.data)
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
