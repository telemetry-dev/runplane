package store

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

func TestUpsertJobIdempotentAndMonotonic(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	msg := func(state string, edit func(*JobMessage)) JobMessage {
		m := JobMessage{
			RunnerRequestID: 77, State: state, ScaleSetID: "ss1", PoolID: "p1", Repository: "acme/api",
			WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", JobName: "test", RunID: 9001,
			Event: "push", Labels: []string{"runplane"}, QueuedAt: sec(0),
		}
		if edit != nil {
			edit(&m)
		}
		return m
	}

	first := addJob(t, s, msg(JobQueued, nil))
	if again := addJob(t, s, msg(JobQueued, nil)); again.ID != first.ID {
		t.Fatalf("a repeated message made a new job: %s, %s", first.ID, again.ID)
	}
	if first.WorkflowFile != ".github/workflows/ci.yml" {
		t.Errorf("workflow file = %q", first.WorkflowFile)
	}

	addJob(t, s, msg(JobRunning, func(m *JobMessage) { m.StartedAt = sec(5) }))
	// A late JobAssigned does not move the job back, but fills assigned_at.
	j := addJob(t, s, msg(JobAssigned, func(m *JobMessage) { m.AssignedAt = sec(2) }))
	if j.State != JobRunning || j.AssignedAt == nil || !j.AssignedAt.Equal(sec(2)) {
		t.Fatalf("after a late assigned message: state %s, assigned_at %v; want running, t0+2s", j.State, j.AssignedAt)
	}

	j = addJob(t, s, msg(JobCompleted, func(m *JobMessage) { m.Result = ResultFailed; m.FinishedAt = sec(60) }))
	if j.State != JobCompleted || j.Result != ResultFailed || !j.FinishedAt.Equal(sec(60)) {
		t.Fatalf("after completed: state %s, result %s, finished %v", j.State, j.Result, j.FinishedAt)
	}

	// A completed job keeps its state, result, and finish time.
	addJob(t, s, msg(JobCompleted, func(m *JobMessage) { m.Result = ResultSucceeded; m.FinishedAt = sec(90) }))
	j = addJob(t, s, msg(JobQueued, nil))
	if j.State != JobCompleted || j.Result != ResultFailed || !j.FinishedAt.Equal(sec(60)) {
		t.Fatalf("completed job changed: state %s, result %s, finished %v", j.State, j.Result, j.FinishedAt)
	}

	// Enrichment still changes a completed job.
	must(t, s.SetJobEnrichment(ctx, j.ID, JobEnrichment{HeadBranch: "main", HeadSHA: "abc123", RunNumber: 4}, sec(100)))
	got, err := s.GetJob(ctx, j.ID)
	must(t, err)
	if got.HeadBranch != "main" || got.RunNumber != 4 || got.Event != "push" || got.EnrichedAt == nil {
		t.Errorf("after enrichment: %+v", got)
	}
	if got.State != JobCompleted || got.QueuedAt != sec(0) || !slices.Equal(got.Labels, []string{"runplane"}) {
		t.Errorf("stored job: %+v", got)
	}
	if n, err := s.CountJobs(ctx, JobFilter{}); err != nil || n != 1 {
		t.Fatalf("jobs = %d, %v; want 1", n, err)
	}
}

// runplane guesses a result when GitHub does not report the job as
// completed. The result from GitHub replaces a guess, but a guess never
// replaces a result from GitHub.
func TestGitHubResultReplacesAGuess(t *testing.T) {
	tests := []struct {
		name       string
		first      JobMessage
		then       JobMessage
		wantResult string
		wantGuess  bool
		wantFinish int
	}{
		{"GitHub after a guess",
			JobMessage{Result: ResultSucceeded, Guessed: true, FinishedAt: sec(10)},
			JobMessage{Result: ResultFailed, FinishedAt: sec(12)},
			ResultFailed, false, 12},
		{"GitHub cancel after a guessed cancel",
			JobMessage{Result: ResultCancelled, Guessed: true, FinishedAt: sec(10)},
			JobMessage{Result: ResultCancelled, FinishedAt: sec(12)},
			ResultCancelled, false, 12},
		{"guess after GitHub",
			JobMessage{Result: ResultFailed, FinishedAt: sec(10)},
			JobMessage{Result: ResultCancelled, Guessed: true, FinishedAt: sec(12)},
			ResultFailed, false, 10},
		{"second guess",
			JobMessage{Result: ResultSucceeded, Guessed: true, FinishedAt: sec(10)},
			JobMessage{Result: ResultCancelled, Guessed: true, FinishedAt: sec(12)},
			ResultSucceeded, true, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTest(t)
			ran := JobMessage{RunnerRequestID: 1, State: JobRunning, QueuedAt: sec(0), StartedAt: sec(1)}
			addJob(t, s, ran)
			for _, m := range []JobMessage{tt.first, tt.then} {
				m.RunnerRequestID, m.State = 1, JobCompleted
				addJob(t, s, m)
			}
			j, err := s.GetJobByRunnerRequestID(t.Context(), 1)
			must(t, err)
			if j.Result != tt.wantResult || j.ResultGuessed != tt.wantGuess || !j.FinishedAt.Equal(sec(tt.wantFinish)) {
				t.Errorf("job = %s (guessed %v) at %v, want %s (guessed %v) at t0+%ds",
					j.Result, j.ResultGuessed, j.FinishedAt, tt.wantResult, tt.wantGuess, tt.wantFinish)
			}
		})
	}
}

// A completed job waits for its finish step until the scheduler ends the
// step. ListUnfinishedJobs finds the job after a restart, also when the job
// had no live log.
func TestFinishPending(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	unfinished := func() []int64 {
		t.Helper()
		list, err := s.ListUnfinishedJobs(ctx, 10)
		must(t, err)
		var ids []int64
		for _, j := range list {
			ids = append(ids, j.RunnerRequestID)
		}
		return ids
	}
	streamed := addJob(t, s, JobMessage{RunnerRequestID: 1, State: JobRunning, QueuedAt: sec(0), StartedAt: sec(1)})
	must(t, s.SetJobLogState(ctx, streamed.ID, LogLive))
	addJob(t, s, JobMessage{RunnerRequestID: 2, State: JobRunning, QueuedAt: sec(0), StartedAt: sec(1)})
	addJob(t, s, JobMessage{RunnerRequestID: 3, State: JobRunning, QueuedAt: sec(0), StartedAt: sec(1)})
	if got := unfinished(); got != nil {
		t.Fatalf("unfinished jobs before a completion: %v", got)
	}

	j := addJob(t, s, JobMessage{RunnerRequestID: 1, State: JobCompleted, Result: ResultSucceeded, FinishedAt: sec(60)})
	if !j.FinishPending || j.LogState != LogNone {
		t.Errorf("completed job: finish pending %v, log %q; want true and none, because the sandbox log ended", j.FinishPending, j.LogState)
	}
	addJob(t, s, JobMessage{RunnerRequestID: 2, State: JobCompleted, Result: ResultFailed, FinishedAt: sec(70)})
	if got := unfinished(); !slices.Equal(got, []int64{2, 1}) {
		t.Fatalf("unfinished jobs = %v, want [2 1]", got)
	}

	must(t, s.EndJobFinish(ctx, j.ID))
	addJob(t, s, JobMessage{RunnerRequestID: 1, State: JobCompleted, Result: ResultSucceeded, FinishedAt: sec(60)})
	if got := unfinished(); !slices.Equal(got, []int64{2}) {
		t.Errorf("unfinished jobs after the end of a step and a repeated message = %v, want [2]", got)
	}

	// The result from GitHub after a guess needs the finish step again.
	guess := addJob(t, s, JobMessage{RunnerRequestID: 3, State: JobCompleted, Result: ResultCancelled, Guessed: true, FinishedAt: sec(80)})
	must(t, s.EndJobFinish(ctx, guess.ID))
	if j := addJob(t, s, JobMessage{RunnerRequestID: 3, State: JobCompleted, Result: ResultFailed, FinishedAt: sec(90)}); !j.FinishPending {
		t.Error("the result from GitHub after a guess: no finish step")
	}
}

// The status shows a job that GitHub assigned as queued until a runner
// starts it.
func TestJobStatus(t *testing.T) {
	for _, tt := range []struct {
		state, result, want string
	}{
		{JobQueued, "", StatusQueued},
		{JobAssigned, "", StatusQueued},
		{JobRunning, "", StatusRunning},
		{JobCompleted, ResultSucceeded, ResultSucceeded},
		{JobCompleted, ResultSkipped, ResultSkipped},
		{JobCompleted, "", ResultFailed},
	} {
		if got := (Job{State: tt.state, Result: tt.result}).Status(); got != tt.want {
			t.Errorf("job %s with result %q: status %q, want %q", tt.state, tt.result, got, tt.want)
		}
	}
}

func TestListJobsCursorPages(t *testing.T) {
	s := openTest(t)
	// Four jobs share one queued_at, so the cursor must also use the ID.
	for i, q := range []int{10, 20, 20, 20, 20, 30, 5} {
		addJob(t, s, JobMessage{RunnerRequestID: int64(i + 1), State: JobQueued, QueuedAt: sec(q)})
	}
	var got []int64
	f := JobFilter{Limit: 2}
	for range 10 {
		jobs, next, err := s.ListJobs(t.Context(), f)
		must(t, err)
		for _, j := range jobs {
			got = append(got, j.RunnerRequestID)
		}
		if next == "" {
			break
		}
		f.Cursor = next
	}
	// Newest first. The IDs sort in the order of the inserts, so the four
	// jobs at 20 come last inserted first.
	if want := []int64{6, 5, 4, 3, 2, 1, 7}; !slices.Equal(got, want) {
		t.Fatalf("pages = %v, want %v", got, want)
	}
	if _, _, err := s.ListJobs(t.Context(), JobFilter{Cursor: "not a cursor"}); !errors.Is(err, ErrBadCursor) {
		t.Errorf("bad cursor: err = %v", err)
	}
}

// CountJobs repeats the conditions of ListJobs, so each filter must give
// the same jobs to both.
func TestListAndCountJobsFilters(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	for _, j := range []struct {
		id                                           int64
		state, result, repo, pool, branch, sha, name string
	}{
		{1, JobQueued, "", "acme/api", "p1", "main", "aaa111", "lint"},
		{2, JobAssigned, "", "acme/api", "p1", "main", "bbb222", "test"},
		{3, JobRunning, "", "acme/web", "p2", "feat/x_y", "ccc333", "build"},
		{4, JobCompleted, ResultSucceeded, "acme/web", "p2", "main", "ddd444", "e2e"},
		{5, JobCompleted, ResultFailed, "acme/api", "p1", "fix", "eee555", "test"},
		{6, JobCompleted, ResultCancelled, "acme/api", "p2", "feat/x-y", "fff666", "lint"},
	} {
		job := addJob(t, s, JobMessage{RunnerRequestID: j.id, State: j.state, Result: j.result,
			Repository: j.repo, PoolID: j.pool, JobName: j.name, QueuedAt: sec(int(j.id))})
		must(t, s.SetJobEnrichment(ctx, job.ID, JobEnrichment{HeadBranch: j.branch, HeadSHA: j.sha}, sec(100)))
		sb := fmt.Sprintf("sb%d", j.id)
		must(t, s.InsertSandbox(ctx, Sandbox{ID: sb, PoolID: j.pool, ServerID: "srv-" + j.repo, Mode: sandbox.ModeContainer,
			State: sandbox.StateGone, CreatedAt: sec(0)}))
		if _, _, err := s.LinkJobSandbox(ctx, job.ID, sb); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		f    JobFilter
		want []int64
	}{
		{JobFilter{}, []int64{6, 5, 4, 3, 2, 1}},
		{JobFilter{Status: "queued"}, []int64{2, 1}},
		{JobFilter{Status: "running"}, []int64{3}},
		{JobFilter{Status: ResultSucceeded}, []int64{4}},
		{JobFilter{Status: ResultFailed}, []int64{5}},
		{JobFilter{Status: ResultCancelled}, []int64{6}},
		{JobFilter{Repository: "acme/web"}, []int64{4, 3}},
		{JobFilter{PoolID: "p2"}, []int64{6, 4, 3}},
		{JobFilter{ServerID: "srv-acme/web"}, []int64{4, 3}},
		{JobFilter{Q: "test"}, []int64{5, 2}},
		{JobFilter{Q: "ddd4"}, []int64{4}}, // SHA prefix
		{JobFilter{Q: "dd44"}, nil},        // not a prefix of the SHA
		{JobFilter{Q: "x_y"}, []int64{3}},  // "_" is literal
		{JobFilter{Q: "x%y"}, nil},         // "%" is literal
		{JobFilter{Since: sec(2), Until: sec(4)}, []int64{3, 2}},
		{JobFilter{Status: "failed", Repository: "acme/web"}, nil},
		{JobFilter{Status: "queued", Repository: "acme/api", Q: "lint"}, []int64{1}},
	}
	for _, tt := range tests {
		jobs, _, err := s.ListJobs(ctx, tt.f)
		must(t, err)
		var got []int64
		for _, j := range jobs {
			got = append(got, j.RunnerRequestID)
			// The page shows the status that the filter selects.
			if tt.f.Status != "" && j.Status() != tt.f.Status {
				t.Errorf("status filter %q gave job %d with status %q", tt.f.Status, j.RunnerRequestID, j.Status())
			}
		}
		n, err := s.CountJobs(ctx, tt.f)
		must(t, err)
		if !slices.Equal(got, tt.want) || n != len(tt.want) {
			t.Errorf("%+v: ListJobs %v, CountJobs %d; want %v", tt.f, got, n, tt.want)
		}
	}
	if n, err := s.CountJobs(ctx, JobFilter{Cursor: "anything", Limit: 1}); err != nil || n != 6 {
		t.Errorf("count with a cursor and a limit = %d, %v; want 6", n, err)
	}
	if _, _, err := s.ListJobs(ctx, JobFilter{Status: "bogus"}); err == nil {
		t.Error("unknown status: no error")
	}
}

func TestExpectedDurationMedian(t *testing.T) {
	s := openTest(t)
	n := int64(0)
	add := func(job, result string, start, run int) {
		n++
		addJob(t, s, JobMessage{RunnerRequestID: n, State: JobCompleted, Result: result, Repository: "acme/api",
			WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", JobName: job,
			QueuedAt: sec(start), StartedAt: sec(start), FinishedAt: sec(start + run)})
	}
	// 22 succeeded runs of "test". The two oldest are slow and fall out of the last 20.
	add("test", ResultSucceeded, 0, 50)
	add("test", ResultSucceeded, 1, 60)
	for i := range 20 {
		add("test", ResultSucceeded, 100+i, 10+i) // 10..29 s, median 19.5 s
	}
	add("test", ResultFailed, 500, 5000)
	add("lint", ResultSucceeded, 600, 7)

	if got, err := s.ExpectedDurationMS(t.Context(), "acme/api", ".github/workflows/ci.yml", "test"); err != nil || got != 19500 {
		t.Errorf("expected duration = %d ms, %v; want 19500", got, err)
	}
	if got, err := s.ExpectedDurationMS(t.Context(), "acme/api", ".github/workflows/ci.yml", "deploy"); err != nil || got != 0 {
		t.Errorf("no runs: %d, %v; want 0", got, err)
	}
}

func TestOverview(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	for i, f := range []struct {
		state, result     string
		queued, wait, run int // seconds; wait is queued to assigned
		noAssign          bool
	}{
		{JobCompleted, ResultSucceeded, 60, 4, 100, false},    // bucket 0
		{JobCompleted, ResultFailed, 120, 10, 50, false},      // bucket 0
		{JobCompleted, ResultSucceeded, 3700, 2, 30, true},    // bucket 1, waits until started
		{JobCompleted, ResultCancelled, 7300, 6, 0, false},    // bucket 2
		{JobRunning, "", 7400, 8, 0, false},                   // bucket 2, running
		{JobQueued, "", 7500, 0, 0, true},                     // no wait yet
		{JobCompleted, ResultSucceeded, -60, 1, 1, false},     // before the range
		{JobCompleted, ResultFailed, 3*3600 + 1, 1, 1, false}, // after the range
	} {
		m := JobMessage{RunnerRequestID: int64(i + 1), State: f.state, Result: f.result, QueuedAt: sec(f.queued)}
		if f.state != JobQueued {
			start := sec(f.queued + f.wait)
			if !f.noAssign {
				m.AssignedAt = start
				start = start.Add(time.Second)
			}
			m.StartedAt = start
			if f.state == JobCompleted {
				m.FinishedAt = start.Add(time.Duration(f.run) * time.Second)
			}
		}
		addJob(t, s, m)
	}
	must(t, s.AddCacheStats(ctx, sec(10), "acme/api", CacheCounts{Hits: 3, Misses: 1}))
	must(t, s.AddCacheStats(ctx, sec(3700), "acme/api", CacheCounts{Hits: 3, Misses: 1}))
	must(t, s.AddCacheStats(ctx, sec(-3700), "acme/api", CacheCounts{Hits: 100})) // the hour before the range

	o, err := s.Overview(ctx, sec(0), sec(3*3600), time.Hour)
	must(t, err)
	if o.Jobs != 6 {
		t.Errorf("jobs = %d, want 6", o.Jobs)
	}
	// Waits: 4, 10, 2, 6, 8 s. Median 6 s.
	if o.MedianQueueMS == nil || *o.MedianQueueMS != 6000 {
		t.Errorf("median queue = %v ms, want 6000", o.MedianQueueMS)
	}
	// One failed of three succeeded or failed.
	if o.FailureRate == nil || fmt.Sprintf("%.4f", *o.FailureRate) != "0.3333" {
		t.Errorf("failure rate = %v, want 1/3", o.FailureRate)
	}
	if o.CacheHitRate == nil || *o.CacheHitRate != 0.75 {
		t.Errorf("cache hit rate = %v, want 0.75", o.CacheHitRate)
	}
	// Run times: 100 + 50 + 30 + 0 s.
	if o.DurationMS != 180_000 {
		t.Errorf("duration = %d ms, want 180000", o.DurationMS)
	}
	want := []OverviewBucket{
		{Start: sec(0), Succeeded: 1, Failed: 1},
		{Start: sec(3600), Succeeded: 1},
		{Start: sec(7200), Cancelled: 1},
	}
	if !slices.Equal(o.Buckets, want) {
		t.Errorf("buckets:\n got %+v\nwant %+v", o.Buckets, want)
	}
	if len(o.Running) != 1 || o.Running[0].RunnerRequestID != 5 {
		t.Errorf("running = %+v, want job 5", o.Running)
	}
	// Recent: completed jobs, last finished first (the job after the range finished last).
	var recent []int64
	for _, j := range o.Recent {
		recent = append(recent, j.RunnerRequestID)
	}
	if !slices.Equal(recent, []int64{8, 4, 3, 2, 1, 7}) {
		t.Errorf("recent = %v", recent)
	}
}

func TestDeleteJobsBeforeDropsMetrics(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	old := addJob(t, s, JobMessage{RunnerRequestID: 1, State: JobQueued, QueuedAt: sec(0)})
	keep := addJob(t, s, JobMessage{RunnerRequestID: 2, State: JobQueued, QueuedAt: sec(100)})
	for _, id := range []string{old.ID, keep.ID} {
		must(t, s.AppendJobMetrics(ctx, id, []JobMetric{{At: sec(1), CPUPercent: 50, MemoryMB: 100},
			{At: sec(6), CPUPercent: 90, MemoryMB: 300}}))
	}
	sum, err := s.JobMetricsSummary(ctx, keep.ID)
	if err != nil || sum != (MetricsSummary{CPUPeak: 90, CPUAvg: 70, MemPeakMB: 300}) {
		t.Errorf("summary = %+v, %v", sum, err)
	}

	deleted, err := s.DeleteJobsBefore(ctx, sec(50))
	if err != nil || !slices.Equal(deleted, []string{old.ID}) {
		t.Fatalf("deleted = %v, %v; want [%s]", deleted, err, old.ID)
	}
	if _, err := s.JobMetricsSummary(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("metrics of a deleted job: err = %v, want ErrNotFound", err)
	}
	if m, _ := s.ListJobMetrics(ctx, keep.ID); len(m) != 2 {
		t.Errorf("metrics of the kept job = %d, want 2", len(m))
	}
}

func TestUpsertJobConcurrentMessages(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	states := []string{JobQueued, JobAssigned, JobRunning, JobCompleted}
	errs := make(chan error, 40)
	for i := range 40 {
		go func() {
			_, err := s.UpsertJob(ctx, JobMessage{RunnerRequestID: 5, State: states[i%4], Result: ResultSucceeded, QueuedAt: sec(0)}, sec(0))
			errs <- err
		}()
	}
	for range 40 {
		must(t, <-errs)
	}
	j, err := s.GetJobByRunnerRequestID(ctx, 5)
	if err != nil || j.State != JobCompleted || j.Result != ResultSucceeded {
		t.Fatalf("job = %s %s, %v; want completed succeeded", j.State, j.Result, err)
	}
	if n, err := s.CountJobs(ctx, JobFilter{}); err != nil || n != 1 {
		t.Fatalf("jobs = %d, %v; want 1", n, err)
	}
}

// A job that runplane never takes must not wait in queued forever, but it
// must open again when GitHub gives it to runplane after all.
func TestExpireQueuedJobs(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	upsert := func(id int64, state string, queued int) Job {
		return addJob(t, s, JobMessage{RunnerRequestID: id, State: state, QueuedAt: sec(queued)})
	}
	const fork = "Fork pull request: the setting allow_fork_prs is off."
	waiting := upsert(1, JobQueued, 0)
	rejected := upsert(2, JobQueued, 0)
	must(t, s.SetJobRejectReason(ctx, rejected.ID, fork))
	recent := upsert(3, JobQueued, 3700)
	assigned := upsert(4, JobAssigned, 0)

	got, err := s.ExpireQueuedJobs(ctx, sec(3600), sec(4000))
	must(t, err)
	var ids []string
	for _, j := range got {
		ids = append(ids, j.ID)
	}
	slices.Sort(ids)
	if want := []string{waiting.ID, rejected.ID}; !slices.Equal(ids, want) {
		t.Fatalf("expired %v, want the two jobs queued before the limit %v", ids, want)
	}
	// Only the scheduler gives reject reasons, and a job that never ran has
	// nothing to finish.
	for id, want := range map[string]string{waiting.ID: "", rejected.ID: fork} {
		j, _ := s.GetJob(ctx, id)
		if j.State != JobCompleted || j.Result != ResultCancelled || !j.ResultGuessed || j.FinishedAt == nil ||
			!j.FinishedAt.Equal(sec(4000)) || j.RejectReason != want || j.FinishPending {
			t.Errorf("job %d = %s %s (guessed %v) finished %v reason %q finish pending %v, "+
				"want completed cancelled (guessed) at 4000 with %q and no finish step",
				j.RunnerRequestID, j.State, j.Result, j.ResultGuessed, j.FinishedAt, j.RejectReason, j.FinishPending, want)
		}
	}
	for _, id := range []string{recent.ID, assigned.ID} {
		if j, _ := s.GetJob(ctx, id); j.State == JobCompleted {
			t.Errorf("job %d expired in state %s", j.RunnerRequestID, j.State)
		}
	}

	j := upsert(1, JobAssigned, 0)
	if j.State != JobAssigned || j.Result != "" || j.FinishedAt != nil || j.RejectReason != "" {
		t.Errorf("expired job after an assignment = %s %q finished %v reason %q, want assigned", j.State, j.Result, j.FinishedAt, j.RejectReason)
	}
	// A setting can allow a rejected job later, so a new offer opens it
	// again for the scheduler to check.
	j = upsert(2, JobQueued, 0)
	if j.State != JobQueued || j.Result != "" || j.FinishedAt != nil || j.RejectReason != "" {
		t.Errorf("expired rejected job after a new offer = %s %q finished %v reason %q, want queued with no reason",
			j.State, j.Result, j.FinishedAt, j.RejectReason)
	}

	// A cancelled job that ran stays closed.
	addJob(t, s, JobMessage{RunnerRequestID: 4, State: JobCompleted, Result: ResultCancelled, StartedAt: sec(2)})
	if j := upsert(4, JobQueued, 0); j.State != JobCompleted {
		t.Errorf("cancelled job that ran, after a late offer = %s, want completed", j.State)
	}
}

func TestListRunUsage(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	must(t, s.CreatePool(ctx, Pool{ID: "p", Name: "small", Label: "small", Arch: "x64", VCPU: 2, MemoryMB: 4096,
		DiskGB: 64, MaxRunners: 2, Enabled: true, CreatedAt: sec(0), UpdatedAt: sec(0)}))
	must(t, s.InsertSandbox(ctx, Sandbox{ID: "big-box", PoolID: "p", ServerID: "s1", Mode: sandbox.ModeVM,
		State: sandbox.StateGone, VCPU: 8, MemoryMB: 16384, CreatedAt: sec(0)}))
	n := int64(0)
	add := func(job, state string, finish int, m *MetricsSummary, sandboxID string) {
		n++
		j := addJob(t, s, JobMessage{RunnerRequestID: n, State: state, PoolID: "p", Repository: "acme/api",
			WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", JobName: job,
			QueuedAt: sec(0), StartedAt: sec(0), FinishedAt: sec(finish)})
		if m != nil {
			must(t, s.SetJobMetricsSummary(ctx, j.ID, *m))
		}
		if sandboxID != "" {
			if _, _, err := s.LinkJobSandbox(ctx, j.ID, sandboxID); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := range 11 { // the oldest falls out of the last 10
		add("test", JobCompleted, 100+i, &MetricsSummary{CPUPeak: float64(i), CPUAvg: 1, MemPeakMB: 1000 + i}, "")
	}
	add("test", JobCompleted, 200, &MetricsSummary{CPUPeak: 90, CPUAvg: 40, MemPeakMB: 9000}, "big-box")
	add("test", JobCompleted, 300, nil, "")                                                 // no metrics
	add("test", JobRunning, 0, &MetricsSummary{CPUPeak: 99, CPUAvg: 99, MemPeakMB: 99}, "") // not completed
	add("lint", JobCompleted, 400, &MetricsSummary{CPUPeak: 99, CPUAvg: 99, MemPeakMB: 99}, "")

	got, err := s.ListRunUsage(ctx, "acme/api", ".github/workflows/ci.yml", "test", 10)
	must(t, err)
	want := []RunUsage{{CPUPeak: 90, CPUAvg: 40, MemPeakMB: 9000, VCPU: 8, MemoryMB: 16384}} // the size of its sandbox
	for i := 10; i >= 2; i-- {
		want = append(want, RunUsage{CPUPeak: float64(i), CPUAvg: 1, MemPeakMB: 1000 + i, VCPU: 2, MemoryMB: 4096}) // the size of the pool
	}
	if !slices.Equal(got, want) {
		t.Errorf("run usage:\n got %+v\nwant %+v", got, want)
	}
}

// The scale set messages come when a runner can take the job, so the queue
// time from GitHub's REST job (created_at) must move queued_at earlier.
// A later or missing time must not move it.
func TestEnrichmentQueuedAtMovesOnlyEarlier(t *testing.T) {
	s := openTest(t)
	msg := time.Date(2026, 11, 4, 22, 52, 20, 0, time.UTC)
	earlier := time.Date(2026, 11, 4, 22, 51, 24, 0, time.UTC)
	j := addJob(t, s, JobMessage{RunnerRequestID: -7, State: JobAssigned, QueuedAt: msg, AssignedAt: msg})
	for _, tt := range []struct {
		name         string
		queued, want time.Time
	}{
		{"no time from GitHub", time.Time{}, msg},
		{"later time", msg.Add(time.Minute), msg},
		{"earlier time", earlier, earlier},
	} {
		must(t, s.SetJobEnrichment(t.Context(), j.ID, JobEnrichment{QueuedAt: tt.queued}, msg))
		if got, err := s.GetJob(t.Context(), j.ID); err != nil || !got.QueuedAt.Equal(tt.want) {
			t.Errorf("%s: queued_at %v, %v; want %v", tt.name, got.QueuedAt, err, tt.want)
		}
	}
}
