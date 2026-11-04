package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
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
	err := e.st.SaveGitHubApp(ctx, store.GitHubApp{
		ID: "a", AppID: 5, Slug: "runplane-acme", Name: "runplane acme", OwnerLogin: "acme", OwnerType: "Organization",
		HTMLURL: "https://ghe.example.com/apps/runplane-acme", PrivateKeyEnc: []byte{1}, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range []store.Installation{
		{ID: 77, AppID: 5, AccountLogin: "acme", AccountType: "Organization", CreatedAt: now, UpdatedAt: now},
		{ID: 78, AppID: 5, AccountLogin: "octo", AccountType: "User", CreatedAt: now, UpdatedAt: now},
	} {
		if err := e.st.UpsertInstallation(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}
	err = e.st.UpsertScaleSet(ctx, store.ScaleSet{ID: "ss", PoolID: "p", InstallationID: 77, RunnerGroupID: 1, Name: "build",
		Status: store.ScaleSetListening, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}

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

type sseEvent struct{ name, data string }

// events opens the SSE stream of b and sends its events to the channel.
func (b *browser) events(t *testing.T) <-chan sseEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", b.e.srv.URL+"/api/events", nil)
	res, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("events: status %d", res.StatusCode)
	}
	ch := make(chan sseEvent, 64)
	go func() {
		defer res.Body.Close()
		r := bufio.NewReader(res.Body)
		var name string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\n")
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				ch <- sseEvent{name, v}
			}
		}
	}()
	return ch
}

func TestGitHubAndBackupEvents(t *testing.T) {
	e := newEnv(t)
	member := e.login("bob@example.com", auth.RoleMember).events(t)
	admin := e.login("ada@example.com", auth.RoleAdmin).events(t)
	backup := store.Backup{ID: "bk1", CreatedAt: time.Now(), Status: "running"}

	// The handlers subscribe after the headers, so publish until the events come.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			e.d.Bus.Publish(events.Event{Type: events.BackupState, Data: backup})
			e.d.Bus.Publish(events.Event{Type: events.GitHubState})
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()

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
