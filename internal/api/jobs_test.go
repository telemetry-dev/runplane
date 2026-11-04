package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// addJob records a queued job of repo.
func (e *env) addJob(requestID int64, repo string, queued time.Time) store.Job {
	e.t.Helper()
	j, err := e.st.UpsertJob(context.Background(), store.JobMessage{
		RunnerRequestID: requestID, State: store.JobQueued, Repository: repo, JobName: fmt.Sprint("job ", requestID),
		WorkflowRef: repo + "/.github/workflows/ci.yml@refs/heads/main", QueuedAt: queued,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func TestJobTotalCountsAllPages(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleMember)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for i, repo := range []string{"acme/api", "acme/web", "acme/api", "acme/api", "acme/web"} {
		e.addJob(int64(i+1), repo, base.Add(time.Duration(i)*time.Minute))
	}

	var first, second JobPage
	b.do("GET", "/api/jobs?repository=acme/api&limit=2", nil, &first).want(t, 200)
	if len(first.Items) != 2 || first.Total != 3 || first.NextCursor == "" {
		t.Fatalf("first page: %d items, total %d, cursor %q; want 2 items of 3", len(first.Items), first.Total, first.NextCursor)
	}
	b.do("GET", "/api/jobs?repository=acme/api&limit=2&cursor="+first.NextCursor, nil, &second).want(t, 200)
	if len(second.Items) != 1 || second.Total != 3 {
		t.Fatalf("second page: %d items, total %d; want 1 item of 3", len(second.Items), second.Total)
	}
	var all JobPage
	b.do("GET", "/api/jobs?q=web", nil, &all).want(t, 200)
	if all.Total != 2 {
		t.Fatalf("total for q=web = %d, want 2", all.Total)
	}
}

func TestJobShowsItsDebugSandbox(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleMember)
	ctx := context.Background()
	if _, err := e.st.UpdateSettings(ctx, func(s *store.Settings) error {
		s.PublicURL = "https://ci.example.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	j := e.addJob(1, "acme/api", time.Now())
	const id = "01jbz00000000000000000debg"
	e.addSandbox(id, sandbox.StateBusy)
	if err := e.st.SetJobSandbox(ctx, j.ID, id, "s"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetSandboxJob(ctx, id, j.ID); err != nil {
		t.Fatal(err)
	}

	var got Job
	b.do("GET", "/api/jobs/"+j.ID, nil, &got).want(t, 200)
	if got.SandboxState != "busy" || got.SSHCommand != "" {
		t.Fatalf("job without debug: sandbox state %q, ssh command %q; want busy and no command", got.SandboxState, got.SSHCommand)
	}

	hold := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Millisecond)
	if err := e.st.SetSandboxDebug(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetSandboxHold(ctx, id, hold); err != nil {
		t.Fatal(err)
	}
	b.do("GET", "/api/jobs/"+j.ID, nil, &got).want(t, 200)
	if want := "ssh -p 2222 " + id + "@ci.example.com"; got.SSHCommand != want {
		t.Fatalf("ssh command %q, want %q", got.SSHCommand, want)
	}
	if got.HoldUntil == nil || !got.HoldUntil.Equal(hold) {
		t.Fatalf("hold_until %v, want %v", got.HoldUntil, hold)
	}

	var sb Sandbox
	b.do("GET", "/api/sandboxes/"+id, nil, &sb).want(t, 200)
	if sb.JobRepository != "acme/api" || sb.JobWorkflowFile != ".github/workflows/ci.yml" || sb.JobName != "job 1" {
		t.Fatalf("sandbox job fields = %q, %q, %q", sb.JobRepository, sb.JobWorkflowFile, sb.JobName)
	}

	if _, err := e.st.SetSandboxState(ctx, id, sandbox.StateGone, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	b.do("GET", "/api/jobs/"+j.ID, nil, &got).want(t, 200)
	if got.SandboxState != "gone" || got.SSHCommand != "" {
		t.Fatalf("gone sandbox: state %q, ssh command %q; want gone and no command", got.SandboxState, got.SSHCommand)
	}
	b.do("GET", "/api/sandboxes/nope", nil, nil).want(t, 404)
}

func TestSSHPortComesFromTheListenAddress(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Config.SSHListen = "0.0.0.0:2022" })
	var st Status
	e.browser().do("GET", "/api/status", nil, &st).want(t, 200)
	var settings Settings
	e.login("ada@example.com", auth.RoleMember).do("GET", "/api/settings", nil, &settings).want(t, 200)
	if st.SSHPort != 2022 || settings.SSHPort != 2022 {
		t.Fatalf("ssh_port in status %d, in settings %d; want 2022", st.SSHPort, settings.SSHPort)
	}
}

func TestShellAllowedOnlyForLiveDebugJobs(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	later, earlier := now.Add(time.Minute), now.Add(-time.Minute)
	for _, tc := range []struct {
		name string
		b    store.Sandbox
		want int // 0: allowed
	}{
		{"busy debug sandbox", store.Sandbox{Debug: true, State: sandbox.StateBusy}, 0},
		{"held after the job", store.Sandbox{Debug: true, State: sandbox.StateStopping, HoldUntil: &later}, 0},
		{"busy without debug", store.Sandbox{State: sandbox.StateBusy}, http.StatusForbidden},
		{"hold ended", store.Sandbox{Debug: true, State: sandbox.StateStopping, HoldUntil: &earlier}, http.StatusConflict},
		{"debug sandbox before its job", store.Sandbox{Debug: true, State: sandbox.StateIdle}, http.StatusConflict},
		{"gone during its hold", store.Sandbox{Debug: true, State: sandbox.StateGone, HoldUntil: &later}, http.StatusConflict},
	} {
		err := shellAllowed(tc.b, now)
		got := 0
		if err != nil {
			got = err.(interface{ GetStatus() int }).GetStatus()
		}
		if got != tc.want {
			t.Errorf("%s: status %d (%v), want %d", tc.name, got, err, tc.want)
		}
	}
}

func TestJobLinksToItsRunBeforeGitHubGivesTheJob(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleMember)
	ctx := context.Background()
	msg := func(id, runID int64) store.JobMessage {
		return store.JobMessage{RunnerRequestID: id, State: store.JobQueued, Repository: "acme/api", JobName: "test",
			WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", RunID: runID, QueuedAt: time.Now()}
	}
	noJob, err := e.st.UpsertJob(ctx, msg(1, 9001))
	if err != nil {
		t.Fatal(err)
	}
	noRun, err := e.st.UpsertJob(ctx, msg(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	enriched, err := e.st.UpsertJob(ctx, msg(3, 9002))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetJobEnrichment(ctx, enriched.ID, store.JobEnrichment{RunAttempt: 2, HTMLURL: "https://github.com/acme/api/actions/runs/9002/job/77"}); err != nil {
		t.Fatal(err)
	}
	// The run of the second attempt, before GitHub lists its job.
	retry, err := e.st.UpsertJob(ctx, msg(4, 9003))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetJobEnrichment(ctx, retry.ID, store.JobEnrichment{RunAttempt: 2}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		id, want string
	}{
		{noJob.ID, "https://github.com/acme/api/actions/runs/9001"},
		{noRun.ID, ""},
		{enriched.ID, "https://github.com/acme/api/actions/runs/9002/job/77"},
		{retry.ID, "https://github.com/acme/api/actions/runs/9003/attempts/2"},
	} {
		var j Job
		b.do("GET", "/api/jobs/"+tc.id, nil, &j).want(t, 200)
		if j.HTMLURL != tc.want {
			t.Errorf("job %s: html_url %q, want %q", tc.id, j.HTMLURL, tc.want)
		}
	}
}
