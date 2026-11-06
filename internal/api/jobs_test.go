package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

func TestJobPages(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleMember)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var want []string
	for i, repo := range []string{"acme/api", "acme/web", "acme/api", "acme/api", "acme/web"} {
		j := e.addJob(int64(i+1), repo, base.Add(time.Duration(i)*time.Minute))
		if repo == "acme/api" {
			want = append([]string{j.ID}, want...) // newest first
		}
	}

	var got []string
	var sizes []int
	cursor := ""
	for range 3 {
		var page JobPage
		b.do("GET", "/api/jobs?repository=acme/api&limit=2&cursor="+cursor, nil, &page).want(t, 200)
		if page.Total != 3 {
			t.Fatalf("total %d, want the 3 jobs of acme/api on each page", page.Total)
		}
		sizes = append(sizes, len(page.Items))
		for _, j := range page.Items {
			got = append(got, j.ID)
			if j.WorkflowFile != ".github/workflows/ci.yml" || j.State != "queued" || j.Result != nil {
				t.Fatalf("job %+v: wrong workflow file, state, or result", j)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !slices.Equal(got, want) || !slices.Equal(sizes, []int{2, 1}) {
		t.Fatalf("pages of %v gave %v, want %v on pages of 2 and 1", sizes, got, want)
	}
	var web JobPage
	b.do("GET", "/api/jobs?q=web", nil, &web).want(t, 200)
	if web.Total != 2 {
		t.Fatalf("total for q=web = %d, want 2", web.Total)
	}
	b.do("GET", "/api/jobs?cursor=bad!", nil, nil).want(t, 422)
}

func TestJobShowsItsDebugSandbox(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleMember)
	ctx := context.Background()
	_, err := e.st.UpdateSettings(ctx, func(s *store.Settings) error {
		s.PublicURL = "https://ci.example.com"
		return nil
	})
	must(t, err)
	j := e.addJob(1, "acme/api", time.Now())
	const id = "01jbz00000000000000000debg"
	e.addSandbox(id, sandbox.StateBusy)
	_, _, err = e.st.LinkJobSandbox(ctx, j.ID, id)
	must(t, err)

	var got Job
	b.do("GET", "/api/jobs/"+j.ID, nil, &got).want(t, 200)
	if got.SandboxState != "busy" || got.SSHCommand != "" {
		t.Fatalf("job without debug: sandbox state %q, ssh command %q; want busy and no command", got.SandboxState, got.SSHCommand)
	}

	hold := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Millisecond)
	must(t, e.st.SetSandboxDebug(ctx, id))
	must(t, e.st.SetSandboxHold(ctx, id, hold))
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

	_, _, err = e.st.AdvanceSandboxState(ctx, id, sandbox.StateGone, "", time.Now())
	must(t, err)
	b.do("GET", "/api/jobs/"+j.ID, nil, &got).want(t, 200)
	if got.SandboxState != "gone" || got.SSHCommand != "" {
		t.Fatalf("gone sandbox: state %q, ssh command %q; want gone and no command", got.SandboxState, got.SSHCommand)
	}
	b.do("GET", "/api/sandboxes/nope", nil, nil).want(t, 404)
}

func TestSSHPortComesFromTheListenAddress(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Config.SSHListen = "0.0.0.0:2022" })
	ctx := context.Background()
	_, err := e.st.UpdateSettings(ctx, func(s *store.Settings) error {
		s.PublicURL = "https://ci.example.com"
		return nil
	})
	must(t, err)
	j := e.addJob(1, "acme/api", time.Now())
	const id = "01jbz00000000000000000debg"
	e.addSandbox(id, sandbox.StateBusy)
	_, _, err = e.st.LinkJobSandbox(ctx, j.ID, id)
	must(t, err)
	must(t, e.st.SetSandboxDebug(ctx, id))

	var got Job
	e.login("ada@example.com", auth.RoleMember).do("GET", "/api/jobs/"+j.ID, nil, &got).want(t, 200)
	if want := "ssh -p 2022 " + id + "@ci.example.com"; got.SSHCommand != want {
		t.Fatalf("ssh command %q, want %q", got.SSHCommand, want)
	}
}

func TestJobLinksToItsRunBeforeGitHubGivesTheJob(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleMember)
	ctx := context.Background()
	job := func(id, runID int64, enrich *store.JobEnrichment) string {
		j, err := e.st.UpsertJob(ctx, store.JobMessage{RunnerRequestID: id, State: store.JobQueued, Repository: "acme/api", JobName: "test",
			WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", RunID: runID, QueuedAt: time.Now()}, time.Now())
		must(t, err)
		if enrich != nil {
			must(t, e.st.SetJobEnrichment(ctx, j.ID, *enrich, time.Now()))
		}
		return j.ID
	}
	for _, tc := range []struct {
		id, want string
	}{
		{job(1, 9001, nil), "https://github.com/acme/api/actions/runs/9001"},
		{job(2, 0, nil), ""},
		{job(3, 9002, &store.JobEnrichment{RunAttempt: 2, HTMLURL: "https://github.com/acme/api/actions/runs/9002/job/77"}), "https://github.com/acme/api/actions/runs/9002/job/77"},
		// The run of the second attempt, before GitHub lists its job.
		{job(4, 9003, &store.JobEnrichment{RunAttempt: 2}), "https://github.com/acme/api/actions/runs/9003/attempts/2"},
	} {
		var j Job
		b.do("GET", "/api/jobs/"+tc.id, nil, &j).want(t, 200)
		if j.HTMLURL != tc.want {
			t.Errorf("job %s: html_url %q, want %q", tc.id, j.HTMLURL, tc.want)
		}
	}
}

// The web shows the status of the API, so a job that GitHub assigned must
// have the status of the filter that lists it.
func TestAssignedJobHasTheQueuedStatus(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	_, err := e.st.UpsertJob(context.Background(), store.JobMessage{
		RunnerRequestID: 1, State: store.JobAssigned, Repository: "acme/api", QueuedAt: now, AssignedAt: now,
	}, now)
	must(t, err)
	var page JobPage
	e.login("ada@example.com", auth.RoleMember).do("GET", "/api/jobs?status=queued", nil, &page).want(t, 200)
	if len(page.Items) != 1 || page.Items[0].Status != store.StatusQueued {
		t.Errorf("queued jobs %+v, want the assigned job with the status queued", page.Items)
	}
}

func TestOverviewHasNoRatesWithoutData(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleMember)
	ctx := context.Background()
	now := time.Now().UTC()
	e.addJob(1, "acme/api", now.Add(-time.Minute))

	res := b.do("GET", "/api/overview", nil, nil)
	res.want(t, 200)
	for _, want := range []string{`"median_queue_ms":null`, `"failure_rate":null`, `"cache_hit_rate":null`} {
		if !strings.Contains(string(res.body), want) {
			t.Errorf("overview without assigned, finished, and cached jobs has no %s: %s", want, res.body)
		}
	}

	_, err := e.st.UpsertJob(ctx, store.JobMessage{RunnerRequestID: 1, State: store.JobAssigned, AssignedAt: now}, now)
	must(t, err)
	must(t, e.st.AddCacheStats(ctx, now, "acme/api", store.CacheCounts{Misses: 4}))
	var o Overview
	b.do("GET", "/api/overview", nil, &o).want(t, 200)
	if o.Stats.MedianQueueMS == nil || o.Stats.CacheHitRate == nil || *o.Stats.CacheHitRate != 0 {
		t.Fatalf("overview stats %+v, want a median and a hit rate of 0", o.Stats)
	}
}

// An unreachable agent keeps its sandbox rows until it connects again. Its
// sandboxes must not count against the vCPUs of the servers that are left.
// A disabled server still runs its jobs.
func TestOverviewVCPUsSkipUnreachableServers(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleMember)
	e.addServer("online", "x64", store.ServerOnline, 8, 16384)
	e.addServer("lost", "x64", store.ServerUnreachable, 16, 32768)
	e.addServer("off", "x64", store.ServerDisabled, 4, 8192)
	for i, x := range []struct {
		server string
		vcpu   int
	}{{"online", 2}, {"lost", 4}, {"lost", 4}, {"off", 1}} {
		must(t, e.st.InsertSandbox(context.Background(), store.Sandbox{
			ID: fmt.Sprint("box", i), PoolID: "p", ServerID: x.server, Mode: sandbox.ModeContainer,
			State: sandbox.StateBusy, VCPU: x.vcpu, MemoryMB: 4096, CreatedAt: time.Now(),
		}))
	}
	var o Overview
	b.do("GET", "/api/overview", nil, &o).want(t, 200)
	if o.Stats.VCPUInUse != 3 || o.Stats.VCPUTotal != 12 {
		t.Fatalf("vCPUs %d / %d, want 3 / 12", o.Stats.VCPUInUse, o.Stats.VCPUTotal)
	}
}
