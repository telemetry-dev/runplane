//go:build devseed

package devseed_test

import (
	"bufio"
	"context"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/devseed"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

// now is about the time of the "Overview" design: "Now" follows 14:00 UTC.
var now = time.Date(2026, 11, 4, 14, 20, 0, 0, time.UTC)

func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, dir
}

func seed(t *testing.T) (*store.Store, string) {
	t.Helper()
	st, dir := openStore(t)
	box, err := secret.NewBox(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(dir, "logs")
	if err := devseed.Seed(context.Background(), st, box, logs, now); err != nil {
		t.Fatal(err)
	}
	return st, logs
}

func allJobs(t *testing.T, st *store.Store) []store.Job {
	t.Helper()
	var all []store.Job
	cursor := ""
	for {
		page, next, err := st.ListJobs(context.Background(), store.JobFilter{Limit: 200, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if next == "" {
			return all
		}
		cursor = next
	}
}

func TestSeedRefusesUsedDatabase(t *testing.T) {
	ctx := context.Background()
	box, _ := secret.NewBox(make([]byte, secret.KeySize))
	for name, prepare := range map[string]func(*store.Store) error{
		"user": func(st *store.Store) error {
			return st.CreateUser(ctx, store.User{ID: "u1", Email: "a@b.c", PasswordHash: "x", Role: store.RoleAdmin, CreatedAt: now})
		},
		// runplane serve records the local server at its first start, before any user exists.
		"local server": func(st *store.Store) error {
			return st.CreateServer(ctx, store.Server{ID: "s1", Name: "runplane-01", Kind: store.ServerLocal, Status: store.ServerOnline, CreatedAt: now, UpdatedAt: now})
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, dir := openStore(t)
			if err := prepare(st); err != nil {
				t.Fatal(err)
			}
			if err := devseed.Seed(ctx, st, box, filepath.Join(dir, "logs"), now); err == nil {
				t.Fatal("Seed accepted a database that is in use")
			}
			if pools, _ := st.ListPools(ctx); len(pools) > 0 {
				t.Errorf("Seed wrote %d pools before it refused", len(pools))
			}
		})
	}
}

func TestSeed(t *testing.T) {
	ctx := context.Background()
	st, logs := seed(t)
	jobs := allJobs(t, st)
	servers, err := st.ListServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	serverName := map[string]string{}
	for _, v := range servers {
		serverName[v.ID] = v.Name
	}

	// serve sets the mode from RUNPLANE_MODE only when the settings have
	// none, and a seeded database must not reach a real webhook.
	t.Run("no mode and no real webhook hosts", func(t *testing.T) {
		settings, err := st.GetSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if settings.Mode != "" {
			t.Errorf("mode %q, want none", settings.Mode)
		}
		box, _ := secret.NewBox(make([]byte, secret.KeySize))
		channels, err := st.ListNotificationChannels(ctx)
		if err != nil || len(channels) == 0 {
			t.Fatalf("channels %v, %v", channels, err)
		}
		for _, c := range channels {
			raw, err := box.Open(c.URLEnc)
			if err != nil {
				t.Fatal(err)
			}
			if u, err := url.Parse(string(raw)); err != nil || !strings.HasSuffix(u.Hostname(), ".invalid") {
				t.Errorf("channel %q URL %s, want a host under .invalid", c.Name, raw)
			}
		}
	})

	t.Run("job count", func(t *testing.T) {
		if n := len(jobs); n < 1100 || n > 1500 {
			t.Errorf("got %d jobs, want about 1,300", n)
		}
	})

	t.Run("job times and steps are in order", func(t *testing.T) {
		for _, j := range jobs {
			times := []*time.Time{&j.QueuedAt, j.AssignedAt, j.StartedAt, j.FinishedAt}
			for i := 1; i < len(times); i++ {
				if times[i] != nil && times[i-1] == nil {
					t.Fatalf("job %s %s: time %d is set but time %d is not", j.Repository, j.JobName, i, i-1)
				}
				if times[i] != nil && times[i].Before(*times[i-1]) {
					t.Fatalf("job %s %s: times out of order: %v", j.Repository, j.JobName, times)
				}
			}
			if j.State != store.JobCompleted {
				continue
			}
			if len(j.Steps) == 0 {
				t.Fatalf("completed job %s %s has no steps", j.Repository, j.JobName)
			}
			prev := *j.StartedAt
			for _, s := range j.Steps {
				if s.StartedAt == nil || s.CompletedAt == nil {
					t.Fatalf("job %s %s: step %q has no times", j.Repository, j.JobName, s.Name)
				}
				if s.StartedAt.Before(prev) || s.CompletedAt.Before(*s.StartedAt) || s.CompletedAt.After(*j.FinishedAt) {
					t.Fatalf("job %s %s: step %q [%v, %v] is not in order within [%v, %v]",
						j.Repository, j.JobName, s.Name, s.StartedAt, s.CompletedAt, j.StartedAt, j.FinishedAt)
				}
				prev = *s.CompletedAt
			}
		}
	})

	t.Run("job states agree with sandbox states", func(t *testing.T) {
		running := map[string]int{} // by server name
		queued := 0
		for _, j := range jobs {
			switch j.State {
			case store.JobQueued:
				queued++
				if j.SandboxID != "" || j.StartedAt != nil {
					t.Errorf("queued job %s has a sandbox or a start time", j.JobName)
				}
				continue
			case store.JobRunning:
				running[serverName[j.ServerID]]++
				if j.Result != "" || j.FinishedAt != nil {
					t.Errorf("running job %s has a result", j.JobName)
				}
			case store.JobCompleted:
				if j.Result == "" || j.FinishedAt == nil {
					t.Errorf("completed job %s has no result or finish time", j.JobName)
				}
			}
			b, err := st.GetSandbox(ctx, j.SandboxID)
			if err != nil {
				t.Fatalf("job %s %s: sandbox: %v", j.Repository, j.JobName, err)
			}
			if b.JobID != j.ID || b.ServerID != j.ServerID || b.PoolID != j.PoolID {
				t.Fatalf("job %s and its sandbox do not point at each other: %+v", j.ID, b)
			}
			switch j.State {
			case store.JobRunning:
				if b.State != sandbox.StateBusy {
					t.Errorf("running job %s has a %s sandbox", j.JobName, b.State)
				}
			case store.JobCompleted:
				if !b.State.Done() || b.StoppedAt == nil || b.StoppedAt.Before(*j.FinishedAt) {
					t.Errorf("completed job %s has sandbox %s, stopped %v", j.JobName, b.State, b.StoppedAt)
				}
			}
		}
		// The "Servers" design: 3 jobs, 2 jobs, 1 job, 1 job; the "Jobs" design: 1 queued job.
		want := map[string]int{"fsn1-ax102-a": 3, "fsn1-ax52-a": 2, "fsn1-ax52-b": 1, "hel1-cax41": 1}
		if !maps.Equal(running, want) || queued != 1 {
			t.Errorf("running jobs by server = %v, queued = %d; want %v and 1 queued", running, queued, want)
		}

		live, err := st.ListLiveSandboxes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		idle := 0
		for _, b := range live {
			switch b.State {
			case sandbox.StateBusy:
				j, err := st.GetJob(ctx, b.JobID)
				if err != nil || j.State != store.JobRunning {
					t.Errorf("busy sandbox %s has job %q in state %q (%v)", b.ID, b.JobID, j.State, err)
				}
			case sandbox.StateIdle:
				idle++
				if b.JobID != "" {
					t.Errorf("idle sandbox %s has job %s", b.ID, b.JobID)
				}
			}
		}
		if idle == 0 {
			t.Error("no warm sandboxes")
		}
	})

	t.Run("overview shows the design numbers", func(t *testing.T) {
		day, err := st.Overview(ctx, now.Add(-24*time.Hour), now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if day.MedianQueueMS < 1500 || day.MedianQueueMS > 4500 {
			t.Errorf("median queue time in 24h = %dms, want a few seconds", day.MedianQueueMS)
		}
		if day.CacheHitRate < 0.88 || day.CacheHitRate > 0.96 {
			t.Errorf("cache hit rate in 24h = %.3f, want about 0.92", day.CacheHitRate)
		}
		month, err := st.Overview(ctx, now.Add(-30*24*time.Hour), now, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if month.FailureRate < 0.02 || month.FailureRate > 0.07 {
			t.Errorf("failure rate in 30 days = %.3f, want about 0.04", month.FailureRate)
		}
		cancelled := 0
		for _, b := range month.Buckets {
			cancelled += b.Cancelled
		}
		if cancelled == 0 || cancelled > month.Jobs/20 {
			t.Errorf("%d of %d jobs are cancelled, want a few", cancelled, month.Jobs)
		}

		// The "Recent jobs" table of the "Overview" design.
		want := []string{
			"kestrel-hq/api test (node 22)", "kestrel-hq/web e2e · webkit", "kestrel-hq/web typecheck",
			"kestrel-hq/api build-image", "kestrel-hq/api deploy-staging", "kestrel-hq/mobile lint",
			"kestrel-hq/infra terraform plan", "kestrel-hq/api test (node 20)",
		}
		var got []string
		for _, j := range day.Recent[:min(len(want), len(day.Recent))] {
			got = append(got, j.Repository+" "+j.JobName)
		}
		if !slices.Equal(got, want) {
			t.Errorf("recent jobs:\ngot  %q\nwant %q", got, want)
		}
	})

	t.Run("jobs follow a daily rhythm", func(t *testing.T) {
		var byHour [24]int
		for _, j := range jobs {
			byHour[j.QueuedAt.Hour()]++
		}
		day := byHour[9] + byHour[10] + byHour[11] + byHour[13] + byHour[14] + byHour[15]
		night := byHour[0] + byHour[1] + byHour[2] + byHour[4] + byHour[5] + byHour[6]
		if day < 5*night {
			t.Errorf("jobs by UTC hour %v: the busy hours have %d jobs, the night hours %d", byHour, day, night)
		}
	})

	t.Run("e2e webkit job matches the job detail design", func(t *testing.T) {
		i := slices.IndexFunc(jobs, func(j store.Job) bool { return j.JobName == "e2e · webkit" && j.RunNumber == 2290 })
		if i < 0 {
			t.Fatal("no e2e · webkit job #2290")
		}
		j := jobs[i]
		if j.Result != store.ResultFailed || serverName[j.ServerID] != "fsn1-ax52-a" || !strings.HasPrefix(j.HeadSHA, "c4e8f12") ||
			j.PRNumber != 412 || j.Actor != "mira-k" || j.CommitMessage != "Fix redirect loop after Stripe checkout" {
			t.Errorf("job = %+v", j)
		}
		if q, d := j.AssignedAt.Sub(j.QueuedAt), j.FinishedAt.Sub(*j.StartedAt); q != 4*time.Second || d != 408*time.Second {
			t.Errorf("queued %v and ran %v, want 4s and 6m48s", q, d)
		}
		var steps []string
		for _, s := range j.Steps {
			steps = append(steps, s.Name+" "+s.Conclusion+" "+s.CompletedAt.Sub(*s.StartedAt).String())
		}
		wantSteps := []string{
			"Set up job success 2s", "Checkout success 3s", "Set up Node 22 success 1s", "Restore pnpm cache success 4s",
			"Install dependencies success 18s", "Install Playwright browsers success 41s",
			"Run Playwright (webkit) failure 5m32s", "Upload report skipped 0s", "Complete job success 1s",
		}
		if !slices.Equal(steps, wantSteps) {
			t.Errorf("steps:\ngot  %q\nwant %q", steps, wantSteps)
		}

		sum, err := st.JobMetricsSummary(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if sum.CPUPeak != 98 || math.Round(sum.CPUAvg) != 64 || sum.MemPeakMB != 11674 {
			t.Errorf("metrics = %+v, want 98%% peak, 64%% average, 11.4 GB peak", sum)
		}

		log, err := os.ReadFile(filepath.Join(logs, j.ID+".log"))
		if err != nil {
			t.Fatal(err)
		}
		var text []string
		for _, line := range strings.Split(strings.TrimSuffix(string(log), "\n"), "\n") {
			text = append(text, line[len("2026-11-04T14:09:00.0000000Z "):])
		}
		step := strings.Join(text, "\n")
		const design = `##[group]Run pnpm test:e2e --project=webkit
pnpm test:e2e --project=webkit
shell: /usr/bin/bash -e {0}
##[endgroup]
> web@0.0.0 test:e2e /home/runner/work/web/web
> playwright test --project=webkit

Running 148 tests using 8 workers

  ✓  checkout/cart.spec.ts:14:3 › adds item to cart (2.1s)
  ✓  checkout/cart.spec.ts:38:3 › applies promo code (1.8s)
  ✓  auth/login.spec.ts:9:3 › signs in with email (1.2s)
  ✘  checkout/redirect.spec.ts:22:3 › returns to order page after payment (30.0s)

  1) [webkit] › checkout/redirect.spec.ts:22:3 › returns to order page after payment

     Error: Timed out 30000ms waiting for expect(page).toHaveURL(/\/orders\/\w+/)

     Expected pattern: /\/orders\/\w+/
     Received string:  "http://localhost:3000/checkout/return?session_id=cs_test_a1"

       29 |   await page.getByRole('button', { name: 'Pay now' }).click();
     > 30 |   await expect(page).toHaveURL(/\/orders\/\w+/);

  1 failed
  147 passed (5.4m)
##[error]Process completed with exit code 1.`
		if !strings.Contains(step, design) {
			t.Errorf("the log does not have the Playwright output of the design:\n%s", step)
		}
	})

	t.Run("stored logs and metrics stay within their jobs", func(t *testing.T) {
		stamp := regexp.MustCompile(`^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{7}Z) `)
		stored, sampled := 0, 0
		for _, j := range jobs {
			end := now
			if j.FinishedAt != nil {
				end = *j.FinishedAt
			}
			samples, err := st.ListJobMetrics(ctx, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(samples) > 0 {
				sampled++
			}
			for i, m := range samples {
				if m.At.Before(*j.StartedAt) || m.At.After(end) || i > 0 && m.At.Sub(samples[i-1].At) != 2*time.Second {
					t.Fatalf("job %s %s: sample %d at %v is outside [%v, %v] or not 2s after the last", j.Repository, j.JobName, i, m.At, j.StartedAt, end)
				}
			}
			if j.LogState != store.LogStored {
				continue
			}
			stored++
			f, err := os.Open(filepath.Join(logs, j.ID+".log"))
			if err != nil {
				t.Fatal(err)
			}
			prev, groups := *j.StartedAt, 0
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				m := stamp.FindStringSubmatch(sc.Text())
				if m == nil {
					t.Fatalf("job %s: log line without a time: %q", j.JobName, sc.Text())
				}
				at, err := time.Parse(time.RFC3339Nano, m[1])
				if err != nil || at.Before(prev) || at.After(end) {
					t.Fatalf("job %s: log time %s is out of order or outside the job (%v)", j.JobName, m[1], err)
				}
				prev = at
				if strings.Contains(sc.Text(), " ##[group]Run ") {
					groups++
				}
			}
			f.Close()
			if groups == 0 {
				t.Errorf("job %s %s: the log has no ##[group]Run step lines", j.Repository, j.JobName)
			}
		}
		if stored < 10 || sampled < 40 {
			t.Errorf("%d jobs have a stored log and %d have metrics, want the newest jobs to have both", stored, sampled)
		}
	})
}

func TestSeedIsDeterministic(t *testing.T) {
	ctx := context.Background()
	type row struct {
		request                   int64
		name, sha, result         string
		queued, started, finished string
		sandbox                   string
	}
	snapshot := func() ([]row, []store.Sandbox) {
		st, _ := seed(t)
		var rows []row
		for _, j := range allJobs(t, st) {
			r := row{request: j.RunnerRequestID, name: j.JobName, sha: j.HeadSHA, result: j.Result,
				queued: j.QueuedAt.String(), sandbox: j.SandboxID}
			if j.StartedAt != nil {
				r.started = j.StartedAt.String()
			}
			if j.FinishedAt != nil {
				r.finished = j.FinishedAt.String()
			}
			rows = append(rows, r)
		}
		boxes, err := st.ListSandboxes(ctx, store.SandboxFilter{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range boxes {
			boxes[i].JobID = "" // the store makes the job IDs
		}
		return rows, boxes
	}
	jobs1, boxes1 := snapshot()
	jobs2, boxes2 := snapshot()
	if !slices.Equal(jobs1, jobs2) {
		t.Error("two seeds with the same time made different jobs")
	}
	if !slices.EqualFunc(boxes1, boxes2, func(a, b store.Sandbox) bool { return a.ID == b.ID && a.IP == b.IP && a.CreatedAt.Equal(b.CreatedAt) }) {
		t.Error("two seeds with the same time made different sandboxes")
	}
}
