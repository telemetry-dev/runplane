package github

import (
	"context"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

func TestRunFollowsSettingsAndPools(t *testing.T) {
	e := newEnv(t)
	e.connect()
	f := e.actions()
	e.installations().set([]map[string]any{{
		"id": 20, "app_id": testAppID, "suspended_at": nil, "account": map[string]any{"login": "octo", "id": 2, "type": "User"},
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
	if st := f.state(); st.sessions != 0 || st.closed != 4 {
		t.Errorf("after shutdown: open sessions = %d, closed = %d", st.sessions, st.closed)
	}
	// Jobs continue across a restart, so shutdown keeps the scale sets.
	if list, _ := e.st.ListScaleSets(ctx); len(list) != 4 || len(e.sched.detached) != 0 {
		t.Errorf("after shutdown: rows = %d, detached = %v", len(list), e.sched.detached)
	}
}

func TestGitHubStateEvents(t *testing.T) {
	e := newEnv(t)
	e.connect()
	published, unsubscribe := e.bus.Subscribe(16)
	defer unsubscribe()
	// Polls publish at most once in 30 seconds.
	t0 := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	for _, step := range []struct {
		at   time.Duration
		want int
	}{
		{0, 1},
		{10 * time.Second, 0},
		{29 * time.Second, 0},
		{31 * time.Second, 1},
		{40 * time.Second, 0},
	} {
		e.svc.polledChanged(t0.Add(step.at))
		if got := countEvents(published, events.GitHubState); got != step.want {
			t.Errorf("poll at +%s: %d github.state events, want %d", step.at, got, step.want)
		}
	}
	if err := e.svc.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countEvents(published, events.GitHubState) == 0 {
		t.Error("no github.state event after the app was removed")
	}
}
