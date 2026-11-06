package github

import (
	"context"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/store"
)

// A personal account has a scale set for each pool and repository whose
// jobs runplane takes: not for a disabled pool, an archived repository, or
// a public repository while allow_public_repos is off.
func TestRunFollowsSettingsAndPools(t *testing.T) {
	e := newEnv(t)
	e.connect()
	f := e.actions()
	e.inst.set([]map[string]any{{
		"id": 20, "app_id": testAppID, "suspended_at": nil, "account": map[string]any{"login": "octo", "id": 2, "type": "User"},
	}}, map[string][]map[string]any{"ghs_20_": {
		{"id": 200, "full_name": "octo/app", "private": true},
		{"id": 201, "full_name": "octo/site", "private": false},
		{"id": 202, "full_name": "octo/old", "private": true, "archived": true},
	}})
	e.pool("p1", "runplane", true)
	e.pool("p0", "runplane-off", false)
	ctx := context.Background()

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
	e.bus.Publish(events.SettingsChanged{})
	eventually(t, "a scale set for the public repository too", listening(2))

	e.pool("p2", "runplane-big", true)
	e.bus.Publish(events.PoolsChanged{})
	eventually(t, "scale sets for the new pool", listening(4))
	scopes := map[string]int{}
	for _, sc := range f.state().scopes {
		scopes[sc]++
	}
	if len(scopes) != 2 || scopes["octo--app"] != 2 || scopes["octo--site"] != 2 {
		t.Errorf("scale sets by repository = %v, want two in octo/app and in octo/site", scopes)
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

// A listener polls GitHub all the time. The browsers learn the time of the
// last poll from a github.state event, but at most once in 30 seconds.
func TestGitHubStateEvents(t *testing.T) {
	e, f, _ := listeningEnv(t)
	published := events.Subscribe[events.GitHubState](t.Context(), e.bus)
	polls := len(f.state().polls)
	eventually(t, "10 more polls", func() bool { return len(f.state().polls) >= polls+10 })
	if n := countEvents(published); n > 1 {
		t.Errorf("%d github.state events for 10 polls, want at most 1", n)
	}
	if err := e.svc.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countEvents(published) == 0 {
		t.Error("no github.state event after the app was removed")
	}
}
