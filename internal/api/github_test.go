package api

import (
	"context"
	"encoding/json"
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
