package github

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/store"
)

func TestTargets(t *testing.T) {
	tests := []struct {
		name         string
		mode         sandbox.Mode
		allowPublic  bool
		wantPersonal []string // repositories of the personal account with a scale set
	}{
		{"public repositories are skipped by default", sandbox.ModeVM, false, []string{"octo/private"}},
		{"vm mode with allow_public_repos", sandbox.ModeVM, true, []string{"octo/private", "octo/public"}},
		// The scheduler rejects the jobs of the public repository unless the servers run VM mode.
		{"no mode setting with allow_public_repos", "", true, []string{"octo/private", "octo/public"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.connect()
			ctx := context.Background()
			e.installOrg(10, "acme")
			e.install(store.Installation{ID: 20, AppID: testAppID, AccountLogin: "octo", AccountType: "User", RepositorySelection: "selected"})
			e.st.ReplaceRepositories(ctx, 20, []store.Repository{
				{ID: 200, FullName: "octo/private", Private: true},
				{ID: 201, FullName: "octo/public"},
				{ID: 202, FullName: "octo/old", Private: true, Archived: true},
			})
			e.pool("p1", "runplane", true)
			e.pool("p2", "runplane-big", false)
			e.st.UpdateSettings(ctx, func(s *store.Settings) error {
				s.Mode, s.AllowPublicRepos = tt.mode, tt.allowPublic
				return nil
			})

			want, _, err := e.svc.targets(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var personal []string
			orgs := 0
			for _, tgt := range want {
				if tgt.pool.ID != "p1" {
					t.Errorf("target for the disabled pool %s", tgt.pool.Label)
				}
				if tgt.repo == nil {
					orgs++
					if tgt.owner() != "acme" {
						t.Errorf("organization target owner = %q", tgt.owner())
					}
					continue
				}
				personal = append(personal, tgt.owner())
			}
			slices.Sort(personal)
			if orgs != 1 || !slices.Equal(personal, tt.wantPersonal) {
				t.Errorf("organization targets = %d, personal = %v; want 1 and %v", orgs, personal, tt.wantPersonal)
			}
		})
	}
}

// listeningEnv has one organization, one enabled pool, and a fake Actions
// service. It returns after the listener of the pool listens.
func listeningEnv(t *testing.T) (*env, *fakeActions, store.ScaleSet) {
	e := newEnv(t)
	e.connect()
	e.installOrg(10, "acme")
	e.pool("p1", "runplane", true)
	f := e.actions()
	e.sched.max = 3
	e.mux.HandleFunc("GET /api/v3/orgs/acme/actions/runners", func(w http.ResponseWriter, r *http.Request) {
		label := func(names ...string) []map[string]any {
			var out []map[string]any
			for _, n := range names {
				out = append(out, map[string]any{"id": 1, "name": n, "type": "read-only"})
			}
			return out
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": 4, "runners": []map[string]any{
			{"id": 50, "name": "runplane-01aaa", "os": "Linux", "status": "online", "busy": false, "labels": label("runplane")},
			{"id": 51, "name": "runplane-01bbb", "os": "Linux", "status": "online", "busy": true, "labels": label("runplane")},
			{"id": 52, "name": "runplane-big-01ccc", "os": "Linux", "status": "online", "busy": false, "labels": label("runplane-big")},
			{"id": 53, "name": "laptop", "os": "macOS", "status": "online", "busy": false, "labels": label("runplane")},
		}})
	})
	e.svc.reconcileNow(context.Background())
	row := waitListening(t, e)
	return e, f, row
}

func waitListening(t *testing.T, e *env) store.ScaleSet {
	t.Helper()
	var row store.ScaleSet
	eventually(t, "scale set listens", func() bool {
		list, _ := e.st.ListScaleSets(context.Background())
		if len(list) == 1 {
			row = list[0]
		}
		return len(list) == 1 && row.Status == store.ScaleSetListening && row.GitHubID != 0
	})
	return row
}

func TestScaleSetListens(t *testing.T) {
	e, f, row := listeningEnv(t)
	ctx := context.Background()

	st := f.state()
	set, ok := st.sets[int(row.GitHubID)]
	if !ok || len(st.sets) != 1 {
		t.Fatalf("scale sets on GitHub = %+v, want one with ID %d", st.sets, row.GitHubID)
	}
	if set.Name != "runplane" || set.RunnerGroupID != 1 || len(set.Labels) != 1 || set.Labels[0].Name != "runplane" || !set.RunnerSetting.DisableUpdate {
		t.Errorf("scale set = %+v, want name and label runplane in the default group, without runner updates", set)
	}
	if row.RunnerGroupID != 1 || row.Name != "runplane" || row.RepositoryID != nil {
		t.Errorf("row = %+v", row)
	}

	// A job message goes to the scheduler, which acquires it through the session.
	f.send(1, 101)
	select {
	case msg := <-e.sched.got:
		if msg.MessageID != 1 || len(msg.JobAvailableMessages) != 1 || msg.JobAvailableMessages[0].RunnerRequestID != 101 {
			t.Errorf("message = %+v", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduler got no message")
	}
	eventually(t, "job acquired", func() bool { return slices.Contains(f.state().acquired, 101) })
	eventually(t, "statistics recorded", func() bool {
		got, _ := e.st.GetScaleSet(ctx, row.ID)
		return got.Stats.AvailableJobs == 1 && got.Stats.RegisteredRunners == 2 && got.LastPollAt != nil
	})

	// The max runners of the scheduler go with each poll.
	if polls := f.state().polls; len(polls) == 0 || polls[0] != 3 {
		t.Errorf("first polls = %v, want max capacity 3 from Attach", polls)
	}
	c := e.sched.client(row.ID)
	c.SetMaxRunners(5)
	eventually(t, "new max capacity", func() bool { p := f.state().polls; return p[len(p)-1] == 5 })

	jit, err := c.GenerateJITConfig(ctx, "runplane-01xyz")
	if err != nil {
		t.Fatal(err)
	}
	if jit != (scheduler.JITConfig{Encoded: "jit-runplane-01xyz", RunnerID: 9001, RunnerName: "runplane-01xyz"}) {
		t.Errorf("JIT config = %+v", jit)
	}
	runners, err := c.ListRunners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []scheduler.Runner{{ID: 50, Name: "runplane-01aaa"}, {ID: 51, Name: "runplane-01bbb", Busy: true}}
	if !slices.Equal(runners, want) {
		t.Errorf("runners = %+v, want %+v", runners, want)
	}

	// Again with nothing changed: no second scale set or session.
	e.svc.reconcileNow(ctx)
	if st := f.state(); len(st.sets) != 1 || st.closed != 0 {
		t.Errorf("a second reconcile made %d scale sets and closed %d sessions", len(st.sets), st.closed)
	}
}

func TestScaleSetFollowsPool(t *testing.T) {
	e, f, row := listeningEnv(t)
	ctx := context.Background()

	pool, _ := e.st.GetPool(ctx, "p1")
	pool.Label = "runplane-new"
	e.st.UpdatePool(ctx, pool)
	e.svc.reconcileNow(ctx)
	st := f.state()
	if set := st.sets[int(row.GitHubID)]; len(st.sets) != 1 || set.Name != "runplane-new" || set.Labels[0].Name != "runplane-new" {
		t.Errorf("after the label change: scale sets = %+v, want the same one renamed", st.sets)
	}
	if got, _ := e.st.GetScaleSet(ctx, row.ID); got.Name != "runplane-new" {
		t.Errorf("row name = %q", got.Name)
	}

	pool.Label = "runplane"
	e.st.UpdatePool(ctx, pool)
	e.svc.reconcileNow(ctx)
	pool.Enabled = false
	e.st.UpdatePool(ctx, pool)
	e.svc.reconcileNow(ctx)
	if _, err := e.st.GetScaleSet(ctx, row.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the row of a disabled pool is still there: %v", err)
	}
	st = f.state()
	if len(st.sets) != 0 || !slices.Equal(st.deletedSets, []int{int(row.GitHubID)}) {
		t.Errorf("scale sets on GitHub = %+v, deleted = %v", st.sets, st.deletedSets)
	}
	if len(st.sessions) != 0 || st.closed != 1 {
		t.Errorf("open sessions = %v, closed = %d; want the session closed", st.sessions, st.closed)
	}
	// Only the idle runner of this scale set; the busy one keeps its job.
	if !slices.Equal(st.removed, []int64{50}) {
		t.Errorf("removed runners = %v, want [50]", st.removed)
	}
	if !slices.Equal(e.sched.detached, []string{row.ID}) {
		t.Errorf("detached = %v", e.sched.detached)
	}
}

func TestScaleSetFindsExistingByName(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.installOrg(10, "acme")
	e.pool("p1", "runplane", true)
	f := e.actions()
	// An earlier start made the scale set but stopped before it saved the ID.
	f.sets[30] = scaleSetSpec("runplane", 1)
	f.sets[30].ID = 30
	f.scopes[30] = "acme"

	e.svc.reconcileNow(context.Background())
	row := waitListening(t, e)
	if row.GitHubID != 30 || len(f.state().sets) != 1 {
		t.Errorf("github_id = %d, scale sets = %d; want the existing scale set 30", row.GitHubID, len(f.state().sets))
	}
}

func TestScaleSetMadeAgainWhenGoneOnGitHub(t *testing.T) {
	e, f, row := listeningEnv(t)
	f.mu.Lock()
	delete(f.sets, int(row.GitHubID))
	f.queue <- "not json" // ends the current poll with an error
	f.mu.Unlock()

	var got store.ScaleSet
	eventually(t, "new scale set", func() bool {
		got, _ = e.st.GetScaleSet(context.Background(), row.ID)
		return got.GitHubID != 0 && got.GitHubID != row.GitHubID && got.Status == store.ScaleSetListening
	})
	if st := f.state(); len(st.sets) != 1 || st.sets[int(got.GitHubID)].Name != "runplane" {
		t.Errorf("scale sets = %+v", st.sets)
	}
}

// GitHub can revoke the Actions token of a client before it expires. The
// listener then needs a new client, or it fails until the token expires.
func TestListenerRenewsRevokedToken(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.installOrg(10, "acme")
	e.pool("p1", "runplane", true)
	f := e.actions()
	f.revokeNext = true

	e.svc.reconcileNow(context.Background())
	eventually(t, "listening with a new token", func() bool {
		list, _ := e.st.ListScaleSets(context.Background())
		return len(list) == 1 && list[0].Status == store.ScaleSetListening
	})
}

func TestSessionConflictIsClearError(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.installOrg(10, "acme")
	e.pool("p1", "runplane", true)
	f := e.actions()
	f.conflict = true

	e.svc.reconcileNow(context.Background())
	eventually(t, "error status", func() bool {
		list, _ := e.st.ListScaleSets(context.Background())
		return len(list) == 1 && list[0].Status == store.ScaleSetError
	})
	list, _ := e.st.ListScaleSets(context.Background())
	if msg := list[0].Error; !strings.Contains(msg, "another listener has the session") || !strings.Contains(msg, "runplane in acme") {
		t.Errorf("error = %q, want the session conflict explained", msg)
	}
}

func TestSuspendedInstallationPausesListener(t *testing.T) {
	e, f, row := listeningEnv(t)
	ctx := context.Background()
	now := time.Now()
	e.st.SetInstallationSuspended(ctx, 10, &now)
	e.svc.reconcileNow(ctx)

	got, err := e.st.GetScaleSet(ctx, row.ID)
	if err != nil {
		t.Fatalf("the row of a suspended installation is gone: %v", err)
	}
	if got.Status != store.ScaleSetError || !strings.Contains(got.Error, "suspended") {
		t.Errorf("status = %s %q", got.Status, got.Error)
	}
	if st := f.state(); len(st.sessions) != 0 || len(st.sets) != 1 {
		t.Errorf("sessions = %v, scale sets = %d; want the session closed and the scale set kept", st.sessions, len(st.sets))
	}

	e.st.SetInstallationSuspended(ctx, 10, nil)
	e.svc.reconcileNow(ctx)
	if again := waitListening(t, e); again.GitHubID != row.GitHubID {
		t.Errorf("github_id = %d after the suspension, want %d", again.GitHubID, row.GitHubID)
	}
}

func TestRemoveRunner(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.installOrg(10, "acme")
	f := e.actions()
	f.agents[60], f.agents[61], f.agents[63] = "busy", "gone", "broken"
	inst, _ := e.st.GetInstallation(context.Background(), 10)
	cl, err := e.svc.scalesetClient(context.Background(), target{pool: store.Pool{Label: "runplane"}, inst: inst})
	if err != nil {
		t.Fatal(err)
	}
	l := &setListener{s: e.svc, cl: cl}
	tests := []struct {
		id       int64
		wantErr  bool
		wantBusy bool
	}{
		{60, true, true},
		{61, false, false},
		{62, false, false},
		{63, true, false},
	}
	for _, tt := range tests {
		err := l.RemoveRunner(context.Background(), tt.id)
		if (err != nil) != tt.wantErr || errors.Is(err, scheduler.ErrRunnerBusy) != tt.wantBusy {
			t.Errorf("RemoveRunner(%d) = %v; want error %v, busy %v", tt.id, err, tt.wantErr, tt.wantBusy)
		}
	}
}

func TestInScaleSet(t *testing.T) {
	r := func(name string, labels ...string) runner {
		x := runner{Name: name}
		for _, l := range labels {
			x.Labels = append(x.Labels, runnerLabel{Name: l})
		}
		return x
	}
	tests := []struct {
		r    runner
		want bool
	}{
		{r("runplane-01abc", "runplane"), true},
		{r("Runplane-01abc", "RUNPLANE"), true},
		{r("runplane-4cpu-01abc", "runplane-4cpu"), false}, // a pool whose label starts the same
		{r("laptop", "runplane"), false},
		{r("runplane-01abc", "other"), false},
	}
	for _, tt := range tests {
		if got := inScaleSet(tt.r, "runplane"); got != tt.want {
			t.Errorf("inScaleSet(%+v) = %v, want %v", tt.r, got, tt.want)
		}
	}
}
