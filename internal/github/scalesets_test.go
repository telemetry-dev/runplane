package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/store"
)

// listeningEnv has one organization, one enabled pool, and a fake Actions
// service. It returns after the listener of the pool listens.
func listeningEnv(t *testing.T) (*env, *fakeActions, store.ScaleSet) {
	e := acmeEnv(t)
	e.pool("p1", "runplane", true)
	f := e.actions()
	e.sched.max = 3
	e.mux.HandleFunc("GET /api/v3/orgs/acme/actions/runners", func(w http.ResponseWriter, r *http.Request) {
		runner := func(id int, name string, busy bool, label string) map[string]any {
			return map[string]any{"id": id, "name": name, "busy": busy, "labels": []map[string]any{{"id": 1, "name": label}}}
		}
		// Only 50 and 51 belong to the scale set runplane.
		writeJSON(w, http.StatusOK, map[string]any{"total_count": 4, "runners": []map[string]any{
			runner(50, "runplane-01aaa", false, "runplane"),
			runner(51, "Runplane-01bbb", true, "RUNPLANE"),
			runner(52, "runplane-big-01ccc", false, "runplane-big"),
			runner(53, "laptop", false, "runplane"),
		}})
	})
	e.sync()
	return e, f, waitStatus(t, e, store.ScaleSetListening)
}

// waitStatus waits until the only scale set has the status, and a GitHub
// ID if it listens.
func waitStatus(t *testing.T, e *env, status string) store.ScaleSet {
	t.Helper()
	var row store.ScaleSet
	eventually(t, "scale set "+status, func() bool {
		list, _ := e.st.ListScaleSets(context.Background())
		if len(list) != 1 {
			return false
		}
		row = list[0]
		return row.Status == status && (status != store.ScaleSetListening || row.GitHubID != 0)
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

	// A job message goes to the scheduler in runplane types, and the
	// scheduler acquires the job through the session.
	f.send(1, 101)
	select {
	case msg := <-e.sched.got:
		if len(msg.Available) != 1 || msg.Available[0].RunnerRequestID != 101 || msg.Available[0].Repository != "acme/web" ||
			msg.Available[0].State != store.JobQueued || msg.Stats == nil || msg.Stats.AvailableJobs != 1 {
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
	if polls := f.state().polls; polls[0] != 3 {
		t.Errorf("first polls = %v, want max capacity 3 from Attach", polls)
	}
	c := e.sched.last
	c.SetMaxRunners(5)
	eventually(t, "new max capacity", func() bool { p := f.state().polls; return p[len(p)-1] == 5 })

	jit, err := c.GenerateJITConfig(ctx, "runplane-01xyz")
	if err != nil || jit != (scheduler.JITConfig{Encoded: "jit-runplane-01xyz-_work", RunnerID: 9001}) {
		t.Errorf("JIT config = %+v, %v", jit, err)
	}
	runners, err := c.ListRunners(ctx)
	want := []scheduler.Runner{{ID: 50, Name: "runplane-01aaa"}, {ID: 51, Name: "Runplane-01bbb", Busy: true}}
	if err != nil || !slices.Equal(runners, want) {
		t.Errorf("runners = %+v, %v; want %+v", runners, err, want)
	}

	// Again with nothing changed: no second scale set or session.
	e.sync()
	if st := f.state(); len(st.sets) != 1 || st.closed != 0 {
		t.Errorf("a second reconcile made %d scale sets and closed %d sessions", len(st.sets), st.closed)
	}
}

// GitHub sends runnerRequestId 0 in scale set messages and names each job by
// its jobId. Two such jobs must get two job keys, and an acquisition by
// request ID must give each key back.
func TestJobsWithoutRequestIDs(t *testing.T) {
	e, f, _ := listeningEnv(t)
	job := func(id, name string) map[string]any {
		return map[string]any{"messageType": "JobAvailable", "runnerRequestId": 0, "jobId": id, "repositoryName": "api",
			"ownerName": "acme", "jobDisplayName": name, "workflowRunId": 900, "requestLabels": []string{"runplane"}}
	}
	jobs, _ := json.Marshal([]map[string]any{job("e8c96f8d-1f58-5afe-b930-91e0f3f291fe", "lint"), job("eb596b22-9a5a-56b8-bf07-7b3f1be34661", "test")})
	body, _ := json.Marshal(map[string]any{"messageId": 1, "messageType": "RunnerScaleSetJobMessages", "body": string(jobs)})
	f.queue <- string(body)

	var msg scheduler.Message
	select {
	case msg = <-e.sched.got:
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduler got no message")
	}
	if len(msg.Available) != 2 {
		t.Fatalf("message = %+v, want two jobs", msg)
	}
	lint, test := msg.Available[0].RunnerRequestID, msg.Available[1].RunnerRequestID
	if lint == test || lint >= 0 || test >= 0 {
		t.Errorf("job keys %d and %d, want two different negative keys", lint, test)
	}
	if got := f.state().acquired; !slices.Equal(got, []int64{0, 0}) {
		t.Errorf("acquired request IDs %v on GitHub, want [0 0]", got)
	}
	e.sched.mu.Lock()
	defer e.sched.mu.Unlock()
	if !slices.Equal(e.sched.acquired, []int64{lint, test}) {
		t.Errorf("acquired keys %v, want %v", e.sched.acquired, []int64{lint, test})
	}
}

// A failed rename is the error of the scale set until a rename works. The
// polls of the listener do not hide it.
func TestRenameErrorStaysUntilARenameWorks(t *testing.T) {
	e, f, row := listeningEnv(t)
	ctx := context.Background()
	f.mu.Lock()
	f.noUpdate = true
	f.mu.Unlock()
	pool, _ := e.st.GetPool(ctx, "p1")
	pool.Label = "runplane-new"
	e.st.UpdatePool(ctx, pool)
	e.sync()

	polls := len(f.state().polls)
	eventually(t, "more polls", func() bool { return len(f.state().polls) > polls+2 })
	if got, _ := e.st.GetScaleSet(ctx, row.ID); got.Status != store.ScaleSetError || !strings.Contains(got.Error, "rename") {
		t.Errorf("scale set after a failed rename = %s %q, want the rename error", got.Status, got.Error)
	}

	f.mu.Lock()
	f.noUpdate = false
	f.mu.Unlock()
	e.sync()
	if got, _ := e.st.GetScaleSet(ctx, row.ID); got.Status != store.ScaleSetListening || got.Error != "" || got.Name != "runplane-new" {
		t.Errorf("scale set after the rename = %s %q %s, want listening with no error as runplane-new", got.Status, got.Error, got.Name)
	}
}

func TestScaleSetFollowsPool(t *testing.T) {
	e, f, row := listeningEnv(t)
	ctx := context.Background()
	setPool := func(label string, enabled bool) {
		pool, _ := e.st.GetPool(ctx, "p1")
		pool.Label, pool.Enabled = label, enabled
		e.st.UpdatePool(ctx, pool)
		e.sync()
	}

	setPool("runplane-new", true)
	st := f.state()
	if set := st.sets[int(row.GitHubID)]; len(st.sets) != 1 || set.Name != "runplane-new" || set.Labels[0].Name != "runplane-new" {
		t.Errorf("after the label change: scale sets = %+v, want the same one renamed", st.sets)
	}
	if got, _ := e.st.GetScaleSet(ctx, row.ID); got.Name != "runplane-new" {
		t.Errorf("row name = %q", got.Name)
	}

	setPool("runplane", true)
	setPool("runplane", false)
	if _, err := e.st.GetScaleSet(ctx, row.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the row of a disabled pool is still there: %v", err)
	}
	st = f.state()
	if len(st.sets) != 0 || !slices.Equal(st.deleted, []int{int(row.GitHubID)}) || st.sessions != 0 || st.closed != 1 {
		t.Errorf("on GitHub: scale sets = %+v, deleted = %v, open sessions = %d, closed = %d", st.sets, st.deleted, st.sessions, st.closed)
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
	e := acmeEnv(t)
	e.pool("p1", "runplane", true)
	f := e.actions()
	// An earlier start made the scale set but stopped before it saved the ID.
	f.sets[30] = scaleset.RunnerScaleSet{ID: 30, Name: "runplane", RunnerGroupID: 1,
		Labels: []scaleset.Label{{Name: "runplane", Type: "System"}}, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}}
	f.scopes[30] = "acme"

	e.sync()
	if row := waitStatus(t, e, store.ScaleSetListening); row.GitHubID != 30 || len(f.state().sets) != 1 {
		t.Errorf("github_id = %d, scale sets = %d; want the existing scale set 30", row.GitHubID, len(f.state().sets))
	}
}

func TestScaleSetMadeAgainWhenGoneOnGitHub(t *testing.T) {
	e, f, row := listeningEnv(t)
	f.mu.Lock()
	delete(f.sets, int(row.GitHubID))
	f.mu.Unlock()
	f.queue <- "not json" // ends the current poll with an error

	var got store.ScaleSet
	eventually(t, "new scale set", func() bool {
		got, _ = e.st.GetScaleSet(context.Background(), row.ID)
		return got.GitHubID != 0 && got.GitHubID != row.GitHubID && got.Status == store.ScaleSetListening
	})
	if st := f.state(); len(st.sets) != 1 || st.sets[int(got.GitHubID)].Name != "runplane" {
		t.Errorf("scale sets = %+v", st.sets)
	}
}

func TestListenerErrors(t *testing.T) {
	tests := []struct {
		name       string
		conflict   bool
		revokeNext bool
		wantStatus string
		wantError  string
	}{
		// GitHub can revoke the Actions token of a client before it expires.
		// The listener then needs a new client, or it fails until the token
		// expires.
		{"listener renews a revoked token", false, true, store.ScaleSetListening, ""},
		{"session conflict", true, false, store.ScaleSetError, "another listener has the session of scale set runplane in acme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := acmeEnv(t)
			e.pool("p1", "runplane", true)
			f := e.actions()
			f.conflict, f.revokeNext = tt.conflict, tt.revokeNext
			e.sync()
			if row := waitStatus(t, e, tt.wantStatus); !strings.Contains(row.Error, tt.wantError) {
				t.Errorf("error = %q, want %q", row.Error, tt.wantError)
			}
		})
	}
}

func TestSuspendedInstallationPausesListener(t *testing.T) {
	e, f, row := listeningEnv(t)
	ctx := context.Background()
	// suspend sets suspended_at of the installation on GitHub, and syncs.
	suspend := func(at any) {
		e.inst.mu.Lock()
		e.inst.list[0]["suspended_at"] = at
		e.inst.mu.Unlock()
		e.sync()
	}
	suspend("2026-11-04T08:00:00Z")
	got, err := e.st.GetScaleSet(ctx, row.ID)
	if err != nil || got.Status != store.ScaleSetError || !strings.Contains(got.Error, "suspended") {
		t.Errorf("scale set = %s %q, %v; want an error about the suspension", got.Status, got.Error, err)
	}
	if st := f.state(); st.sessions != 0 || len(st.sets) != 1 {
		t.Errorf("sessions = %d, scale sets = %d; want the session closed and the scale set kept", st.sessions, len(st.sets))
	}

	suspend(nil)
	if again := waitStatus(t, e, store.ScaleSetListening); again.GitHubID != row.GitHubID {
		t.Errorf("github_id = %d after the suspension, want %d", again.GitHubID, row.GitHubID)
	}
}

// The scheduler removes the runners of a scale set through its client. A
// runner with a job is busy, and a runner that GitHub no longer has is
// removed already.
func TestRemoveRunner(t *testing.T) {
	e, f, _ := listeningEnv(t)
	f.mu.Lock()
	f.agents[60], f.agents[61], f.agents[63] = "busy", "gone", "broken"
	f.mu.Unlock()
	c := e.sched.last
	tests := []struct {
		id                int64
		wantErr, wantBusy bool
	}{
		{60, true, true},
		{61, false, false},
		{62, false, false},
		{63, true, false},
	}
	for _, tt := range tests {
		err := c.RemoveRunner(context.Background(), tt.id)
		if (err != nil) != tt.wantErr || errors.Is(err, scheduler.ErrRunnerBusy) != tt.wantBusy {
			t.Errorf("RemoveRunner(%d) = %v; want error %v, busy %v", tt.id, err, tt.wantErr, tt.wantBusy)
		}
	}
}
