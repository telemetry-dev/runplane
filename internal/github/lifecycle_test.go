package github

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

var _ api.GitHub = (*Service)(nil)

func TestRunFollowsSettingsAndPools(t *testing.T) {
	e := newEnv(t)
	e.connect()
	f := e.actions()
	e.installations().set([]map[string]any{{
		"id": 20, "app_id": testAppID, "repository_selection": "selected", "suspended_at": nil,
		"account": map[string]any{"login": "octo", "id": 2, "type": "User"},
	}}, map[string][]map[string]any{"ghs_20_": {
		{"id": 200, "full_name": "octo/app", "private": true},
		{"id": 201, "full_name": "octo/site", "private": false},
	}})
	e.pool("p1", "runplane", true)
	ctx := context.Background()
	e.st.UpdateSettings(ctx, func(s *store.Settings) error { s.Mode = sandbox.ModeVM; return nil })

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		e.svc.Run(runCtx)
		close(done)
	}()
	listening := func(n int) func() bool {
		return func() bool {
			list, _ := e.st.ListScaleSets(ctx)
			ok := len(list) == n
			for _, x := range list {
				ok = ok && x.Status == store.ScaleSetListening
			}
			return ok
		}
	}
	eventually(t, "one scale set for the private repository", listening(1))

	e.st.UpdateSettings(ctx, func(s *store.Settings) error { s.AllowPublicRepos = true; return nil })
	e.bus.Publish(events.Event{Type: events.SettingsChanged})
	eventually(t, "a scale set for the public repository too", listening(2))

	e.pool("p2", "runplane-big", true)
	e.svc.PoolsChanged(ctx)
	eventually(t, "scale sets for the new pool", listening(4))
	scopes := map[string]int{}
	for _, sc := range f.state().scopes {
		scopes[sc]++
	}
	if scopes["octo--app"] != 2 || scopes["octo--site"] != 2 {
		t.Errorf("scale sets by repository = %v, want two in each", scopes)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if st := f.state(); len(st.sessions) != 0 || st.closed != 4 {
		t.Errorf("after shutdown: open sessions = %v, closed = %d", st.sessions, st.closed)
	}
	// Jobs continue across a restart, so shutdown keeps the scale sets.
	if list, _ := e.st.ListScaleSets(ctx); len(list) != 4 || len(e.sched.detached) != 0 {
		t.Errorf("after shutdown: rows = %d, detached = %v", len(list), e.sched.detached)
	}
}

func TestDisconnect(t *testing.T) {
	e, f, row := listeningEnv(t)
	ctx := context.Background()
	if err := e.svc.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.GetGitHubApp(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the app is still there: %v", err)
	}
	if list, _ := e.st.ListInstallations(ctx); len(list) != 0 {
		t.Errorf("installations = %+v", list)
	}
	if list, _ := e.st.ListScaleSets(ctx); len(list) != 0 {
		t.Errorf("scale sets = %+v", list)
	}
	st := f.state()
	if len(st.sets) != 0 || len(st.sessions) != 0 || !slices.Contains(st.removed, 50) {
		t.Errorf("on GitHub: scale sets = %+v, sessions = %v, removed runners = %v", st.sets, st.sessions, st.removed)
	}
	if !slices.Equal(e.sched.detached, []string{row.ID}) {
		t.Errorf("detached = %v", e.sched.detached)
	}
	// A second disconnect has nothing to do.
	if err := e.svc.Disconnect(ctx); err != nil {
		t.Errorf("second disconnect: %v", err)
	}
}

func TestDisconnectNamesScaleSetsLeftOnGitHub(t *testing.T) {
	e, f, _ := listeningEnv(t)
	f.mu.Lock()
	f.agents[50] = "broken"
	f.mu.Unlock()
	err := e.svc.Disconnect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "runplane") {
		t.Fatalf("err = %v, want the scale set name", err)
	}
	if _, err := e.st.GetGitHubApp(context.Background()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the app is still there after a GitHub failure: %v", err)
	}
}

func TestAppDeleteURL(t *testing.T) {
	org := AppDeleteURL(store.GitHubApp{Slug: "runplane-ab12cd", OwnerLogin: "acme", OwnerType: "Organization"})
	user := AppDeleteURL(store.GitHubApp{Slug: "runplane-ab12cd", OwnerLogin: "octo", OwnerType: "User"})
	if org != "https://github.com/organizations/acme/settings/apps/runplane-ab12cd/advanced" ||
		user != "https://github.com/settings/apps/runplane-ab12cd/advanced" {
		t.Errorf("AppDeleteURL = %q, %q", org, user)
	}
	ghes := AppDeleteURL(store.GitHubApp{Slug: "runplane-ab12cd", OwnerLogin: "acme", OwnerType: "Organization",
		HTMLURL: "https://git.example.com/apps/runplane-ab12cd"})
	if ghes != "https://git.example.com/organizations/acme/settings/apps/runplane-ab12cd/advanced" {
		t.Errorf("AppDeleteURL on GitHub Enterprise Server = %q", ghes)
	}
}
