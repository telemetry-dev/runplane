package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

var t0 = time.UnixMilli(1_700_000_000_000).UTC()

func sec(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }

func TestUpsertJobIdempotentAndMonotonic(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	base := JobMessage{
		RunnerRequestID: 77, ScaleSetID: "ss1", PoolID: "p1", Repository: "acme/api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", JobName: "test", RunID: 9001,
		Event: "push", Labels: []string{"runplane"}, QueuedAt: sec(0),
	}
	msg := func(state string, edit func(*JobMessage)) JobMessage {
		m := base
		m.State = state
		if edit != nil {
			edit(&m)
		}
		return m
	}
	upsert := func(m JobMessage) Job {
		t.Helper()
		j, err := s.UpsertJob(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}

	first := upsert(msg(JobQueued, nil))
	again := upsert(msg(JobQueued, nil))
	if again.ID != first.ID {
		t.Fatalf("a repeated message made a new job: %s, %s", first.ID, again.ID)
	}
	if first.WorkflowFile != ".github/workflows/ci.yml" {
		t.Errorf("workflow file = %q", first.WorkflowFile)
	}

	upsert(msg(JobRunning, func(m *JobMessage) { m.StartedAt = sec(5) }))
	// A late JobAssigned does not move the job back, but fills assigned_at.
	j := upsert(msg(JobAssigned, func(m *JobMessage) { m.AssignedAt = sec(2) }))
	if j.State != JobRunning {
		t.Fatalf("state = %s after a late assigned message, want running", j.State)
	}
	if j.AssignedAt == nil || !j.AssignedAt.Equal(sec(2)) {
		t.Errorf("assigned_at = %v, want t0+2s", j.AssignedAt)
	}

	j = upsert(msg(JobCompleted, func(m *JobMessage) { m.Result = ResultFailed; m.FinishedAt = sec(60) }))
	if j.State != JobCompleted || j.Result != ResultFailed || !j.FinishedAt.Equal(sec(60)) {
		t.Fatalf("after completed: state %s, result %s, finished %v", j.State, j.Result, j.FinishedAt)
	}

	// A completed job keeps its state, result, and finish time.
	upsert(msg(JobCompleted, func(m *JobMessage) { m.Result = ResultSucceeded; m.FinishedAt = sec(90) }))
	j = upsert(msg(JobQueued, nil))
	if j.State != JobCompleted || j.Result != ResultFailed || !j.FinishedAt.Equal(sec(60)) {
		t.Fatalf("completed job changed: state %s, result %s, finished %v", j.State, j.Result, j.FinishedAt)
	}

	// Enrichment still changes a completed job.
	if err := s.SetJobEnrichment(ctx, j.ID, JobEnrichment{HeadBranch: "main", HeadSHA: "abc123", RunNumber: 4}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadBranch != "main" || got.RunNumber != 4 || got.Event != "push" || got.EnrichedAt == nil {
		t.Errorf("after enrichment: %+v", got)
	}
	if got.State != JobCompleted || got.QueuedAt != sec(0) || !slices.Equal(got.Labels, []string{"runplane"}) {
		t.Errorf("stored job: %+v", got)
	}

	var n int
	if err := s.r.QueryRow("SELECT count(*) FROM jobs").Scan(&n); err != nil || n != 1 {
		t.Fatalf("jobs = %d, %v; want 1", n, err)
	}
}

func TestUpsertJobFirstMessageCompleted(t *testing.T) {
	s := openTest(t)
	j, err := s.UpsertJob(context.Background(), JobMessage{RunnerRequestID: 1, State: JobCompleted,
		Result: ResultCancelled, QueuedAt: sec(0), FinishedAt: sec(3)})
	if err != nil {
		t.Fatal(err)
	}
	if j.State != JobCompleted || j.Result != ResultCancelled || j.FinishedAt == nil {
		t.Fatalf("job = %+v", j)
	}
}

func TestListJobsCursorPages(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	// Seven jobs; four share one queued_at, so the cursor must also use the ID.
	queued := []int{10, 20, 20, 20, 20, 30, 5}
	for i, q := range queued {
		if _, err := s.UpsertJob(ctx, JobMessage{RunnerRequestID: int64(i + 1), State: JobQueued, QueuedAt: sec(q)}); err != nil {
			t.Fatal(err)
		}
	}
	all, _, err := s.ListJobs(ctx, JobFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	var cursor string
	for page := 0; ; page++ {
		jobs, next, err := s.ListJobs(ctx, JobFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs {
			got = append(got, j.ID)
		}
		if next == "" {
			break
		}
		if page > 10 {
			t.Fatal("the pages do not end")
		}
		cursor = next
	}
	if len(got) != len(queued) {
		t.Fatalf("pages returned %d jobs, want %d", len(got), len(queued))
	}
	for i, id := range got {
		if id != all[i].ID {
			t.Fatalf("page order differs from one full page at %d", i)
		}
	}
	// Newest first: 30, then the four at 20 by ID descending, then 10 and 5.
	wantQueued := []int{30, 20, 20, 20, 20, 10, 5}
	for i, j := range all {
		if !j.QueuedAt.Equal(sec(wantQueued[i])) {
			t.Fatalf("job %d queued at %v, want t0+%ds", i, j.QueuedAt, wantQueued[i])
		}
		if i > 0 && j.QueuedAt.Equal(all[i-1].QueuedAt) && j.ID >= all[i-1].ID {
			t.Fatalf("equal queued_at not ordered by ID descending at %d", i)
		}
	}

	if _, _, err := s.ListJobs(ctx, JobFilter{Cursor: "not a cursor"}); !errors.Is(err, ErrBadCursor) {
		t.Errorf("bad cursor: err = %v", err)
	}
}

func TestListJobsFilters(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	add := func(id int64, state, result string, repo, branch, sha, job string, q int) {
		t.Helper()
		j, err := s.UpsertJob(ctx, JobMessage{RunnerRequestID: id, State: state, Result: result, Repository: repo,
			JobName: job, QueuedAt: sec(q)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetJobEnrichment(ctx, j.ID, JobEnrichment{HeadBranch: branch, HeadSHA: sha}); err != nil {
			t.Fatal(err)
		}
	}
	add(1, JobQueued, "", "acme/api", "main", "aaa111", "lint", 1)
	add(2, JobAssigned, "", "acme/api", "main", "bbb222", "test", 2)
	add(3, JobRunning, "", "acme/web", "feat/x_y", "ccc333", "build", 3)
	add(4, JobCompleted, ResultSucceeded, "acme/web", "main", "ddd444", "e2e", 4)
	add(5, JobCompleted, ResultFailed, "acme/api", "fix", "eee555", "test", 5)

	ids := func(f JobFilter) []int64 {
		t.Helper()
		jobs, _, err := s.ListJobs(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, j := range jobs {
			out = append(out, j.RunnerRequestID)
		}
		return out
	}
	tests := []struct {
		f    JobFilter
		want []int64
	}{
		{JobFilter{Status: "queued"}, []int64{2, 1}},
		{JobFilter{Status: "running"}, []int64{3}},
		{JobFilter{Status: "failed"}, []int64{5}},
		{JobFilter{Repository: "acme/web"}, []int64{4, 3}},
		{JobFilter{Q: "test"}, []int64{5, 2}},
		{JobFilter{Q: "ddd4"}, []int64{4}}, // SHA prefix
		{JobFilter{Q: "dd44"}, nil},        // not a prefix of the SHA
		{JobFilter{Q: "x_y"}, []int64{3}},  // branch, "_" is literal
		{JobFilter{Q: "x%y"}, nil},         // "%" is literal
		{JobFilter{Since: sec(2), Until: sec(4)}, []int64{3, 2}},
		{JobFilter{Status: "failed", Repository: "acme/web"}, nil},
	}
	for _, tt := range tests {
		if got := ids(tt.f); !slices.Equal(got, tt.want) {
			t.Errorf("%+v: got %v, want %v", tt.f, got, tt.want)
		}
	}
	if _, _, err := s.ListJobs(ctx, JobFilter{Status: "bogus"}); err == nil {
		t.Error("unknown status: no error")
	}
}

func TestExpectedDurationMedian(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	n := int64(0)
	add := func(job, result string, startSec, runSec int) {
		t.Helper()
		n++
		_, err := s.UpsertJob(ctx, JobMessage{RunnerRequestID: n, State: JobCompleted, Result: result,
			Repository: "acme/api", WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", JobName: job,
			QueuedAt: sec(startSec), StartedAt: sec(startSec), FinishedAt: sec(startSec + runSec)})
		if err != nil {
			t.Fatal(err)
		}
	}
	// 22 succeeded runs of "test". The two oldest are slow and fall out of the last 20.
	add("test", ResultSucceeded, 0, 50)
	add("test", ResultSucceeded, 1, 60)
	for i := range 20 {
		add("test", ResultSucceeded, 100+i, 10+i) // 10..29 s, median 19.5 s
	}
	add("test", ResultFailed, 500, 5000)
	add("lint", ResultSucceeded, 600, 7)

	got, err := s.ExpectedDurationMS(ctx, "acme/api", ".github/workflows/ci.yml", "test")
	if err != nil {
		t.Fatal(err)
	}
	if got != 19500 {
		t.Errorf("expected duration = %d ms, want 19500", got)
	}
	if got, _ := s.ExpectedDurationMS(ctx, "acme/api", ".github/workflows/ci.yml", "deploy"); got != 0 {
		t.Errorf("no runs: %d, want 0", got)
	}
}

func TestOverview(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	since := sec(0)
	until := sec(3 * 3600)
	n := int64(0)
	type fixture struct {
		state, result     string
		queued, wait, run int // seconds; wait is queued to assigned
		noAssign          bool
	}
	for _, f := range []fixture{
		{JobCompleted, ResultSucceeded, 60, 4, 100, false},    // bucket 0
		{JobCompleted, ResultFailed, 120, 10, 50, false},      // bucket 0
		{JobCompleted, ResultSucceeded, 3700, 2, 30, true},    // bucket 1, waits until started
		{JobCompleted, ResultCancelled, 7300, 6, 0, false},    // bucket 2
		{JobRunning, "", 7400, 8, 0, false},                   // bucket 2, running
		{JobQueued, "", 7500, 0, 0, true},                     // no wait yet
		{JobCompleted, ResultSucceeded, -60, 1, 1, false},     // before the range
		{JobCompleted, ResultFailed, 3*3600 + 1, 1, 1, false}, // after the range
	} {
		n++
		m := JobMessage{RunnerRequestID: n, State: f.state, Result: f.result, QueuedAt: sec(f.queued)}
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
		if _, err := s.UpsertJob(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		at           time.Time
		hits, misses int64
	}{
		{sec(10), 3, 1},
		{sec(3700), 3, 1},
		{sec(-3700), 100, 0}, // the hour before the range
	} {
		if err := s.AddCacheStats(ctx, c.at, "acme/api", CacheCounts{Hits: c.hits, Misses: c.misses}); err != nil {
			t.Fatal(err)
		}
	}

	o, err := s.Overview(ctx, since, until, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if o.Jobs != 6 {
		t.Errorf("jobs = %d, want 6", o.Jobs)
	}
	// Waits: 4, 10, 2, 6, 8 s. Median 6 s.
	if o.MedianQueueMS != 6000 {
		t.Errorf("median queue = %d ms, want 6000", o.MedianQueueMS)
	}
	// One failed of three succeeded or failed.
	if fmt.Sprintf("%.4f", o.FailureRate) != "0.3333" {
		t.Errorf("failure rate = %v, want 1/3", o.FailureRate)
	}
	if o.CacheHitRate != 0.75 {
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
	ctx := context.Background()
	old, _ := s.UpsertJob(ctx, JobMessage{RunnerRequestID: 1, State: JobQueued, QueuedAt: sec(0)})
	keep, _ := s.UpsertJob(ctx, JobMessage{RunnerRequestID: 2, State: JobQueued, QueuedAt: sec(100)})
	for _, id := range []string{old.ID, keep.ID} {
		if err := s.AppendJobMetrics(ctx, id, []JobMetric{{At: sec(1), CPUPercent: 50, MemoryMB: 100},
			{At: sec(6), CPUPercent: 90, MemoryMB: 300}}); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := s.JobMetricsSummary(ctx, keep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum != (MetricsSummary{CPUPeak: 90, CPUAvg: 70, MemPeakMB: 300}) {
		t.Errorf("summary = %+v", sum)
	}

	deleted, err := s.DeleteJobsBefore(ctx, sec(50))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{old.ID}) {
		t.Fatalf("deleted = %v, want [%s]", deleted, old.ID)
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
	ctx := context.Background()
	states := []string{JobQueued, JobAssigned, JobRunning, JobCompleted}
	errs := make(chan error, 40)
	for i := range 40 {
		go func() {
			_, err := s.UpsertJob(ctx, JobMessage{RunnerRequestID: 5, State: states[i%4], Result: ResultSucceeded, QueuedAt: sec(0)})
			errs <- err
		}()
	}
	for range 40 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	j, err := s.GetJobByRunnerRequestID(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != JobCompleted || j.Result != ResultSucceeded {
		t.Fatalf("job = %s %s, want completed succeeded", j.State, j.Result)
	}
	var n int
	if err := s.r.QueryRow("SELECT count(*) FROM jobs").Scan(&n); err != nil || n != 1 {
		t.Fatalf("jobs = %d, %v; want 1", n, err)
	}
}

// A job that runplane never takes must not wait in queued forever, but it
// must open again when GitHub gives it to runplane after all.
func TestExpireQueuedJobs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	upsert := func(id int64, state string, queued time.Time) Job {
		t.Helper()
		j, err := s.UpsertJob(ctx, JobMessage{RunnerRequestID: id, State: state, QueuedAt: queued})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	waiting := upsert(1, JobQueued, sec(0))
	rejected := upsert(2, JobQueued, sec(0))
	if err := s.SetJobRejectReason(ctx, rejected.ID, "Fork pull request: the setting allow_fork_prs is off."); err != nil {
		t.Fatal(err)
	}
	recent := upsert(3, JobQueued, sec(3700))
	assigned := upsert(4, JobAssigned, sec(0))

	got, err := s.ExpireQueuedJobs(ctx, sec(3600), sec(4000))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]Job{}
	for _, j := range got {
		ids[j.ID] = j
	}
	if len(got) != 2 || ids[waiting.ID].ID == "" || ids[rejected.ID].ID == "" {
		t.Fatalf("expired %+v, want the two jobs queued before the limit", got)
	}
	for id, want := range map[string]string{waiting.ID: NotRunReason, rejected.ID: "Fork pull request: the setting allow_fork_prs is off."} {
		j, _ := s.GetJob(ctx, id)
		if j.State != JobCompleted || j.Result != ResultCancelled || j.FinishedAt == nil || !j.FinishedAt.Equal(sec(4000)) || j.RejectReason != want {
			t.Errorf("job %d = %s %s finished %v reason %q, want completed cancelled at 4000 with %q",
				j.RunnerRequestID, j.State, j.Result, j.FinishedAt, j.RejectReason, want)
		}
	}
	for _, id := range []string{recent.ID, assigned.ID} {
		if j, _ := s.GetJob(ctx, id); j.State == JobCompleted {
			t.Errorf("job %d expired in state %s", j.RunnerRequestID, j.State)
		}
	}

	j := upsert(1, JobAssigned, sec(0))
	if j.State != JobAssigned || j.Result != "" || j.FinishedAt != nil || j.RejectReason != "" {
		t.Errorf("expired job after an assignment = %s %q finished %v reason %q, want assigned", j.State, j.Result, j.FinishedAt, j.RejectReason)
	}
	// A setting can allow a rejected job later, so a new offer opens it
	// again for the scheduler to check.
	j = upsert(2, JobQueued, sec(0))
	if j.State != JobQueued || j.Result != "" || j.FinishedAt != nil || j.RejectReason != "" {
		t.Errorf("expired rejected job after a new offer = %s %q finished %v reason %q, want queued with no reason",
			j.State, j.Result, j.FinishedAt, j.RejectReason)
	}

	// A cancelled job that ran stays closed.
	if _, err := s.UpsertJob(ctx, JobMessage{RunnerRequestID: 4, State: JobCompleted, Result: ResultCancelled, StartedAt: sec(2)}); err != nil {
		t.Fatal(err)
	}
	if j := upsert(4, JobQueued, sec(0)); j.State != JobCompleted {
		t.Errorf("cancelled job that ran, after a late offer = %s, want completed", j.State)
	}
}

func TestListRunUsage(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreatePool(ctx, Pool{ID: "p", Name: "small", Label: "small", Arch: "x64", VCPU: 2, MemoryMB: 4096, DiskGB: 64,
		MaxRunners: 2, Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSandbox(ctx, Sandbox{ID: "big-box", PoolID: "p", ServerID: "s1", Mode: sandbox.ModeVM,
		State: sandbox.StateGone, VCPU: 8, MemoryMB: 16384, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	n := int64(0)
	add := func(job, state string, finishSec int, m *MetricsSummary, sandboxID string) {
		t.Helper()
		n++
		j, err := s.UpsertJob(ctx, JobMessage{RunnerRequestID: n, State: state, PoolID: "p",
			Repository: "acme/api", WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", JobName: job,
			QueuedAt: sec(0), StartedAt: sec(0), FinishedAt: sec(finishSec)})
		if err != nil {
			t.Fatal(err)
		}
		if m != nil {
			if err := s.SetJobMetricsSummary(ctx, j.ID, *m); err != nil {
				t.Fatal(err)
			}
		}
		if sandboxID != "" {
			if err := s.SetJobSandbox(ctx, j.ID, sandboxID, "s1"); err != nil {
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
	if err != nil {
		t.Fatal(err)
	}
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
	ctx := context.Background()
	msg := time.Date(2026, 11, 4, 22, 52, 20, 0, time.UTC)
	j, err := s.UpsertJob(ctx, JobMessage{RunnerRequestID: -7, State: JobAssigned, QueuedAt: msg, AssignedAt: msg})
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name   string
		queued time.Time
		want   time.Time
	}{
		{"no time from GitHub", time.Time{}, msg},
		{"later time", msg.Add(time.Minute), msg},
		{"earlier time", time.Date(2026, 11, 4, 22, 51, 24, 0, time.UTC), time.Date(2026, 11, 4, 22, 51, 24, 0, time.UTC)},
	} {
		if err := s.SetJobEnrichment(ctx, j.ID, JobEnrichment{QueuedAt: tt.queued}); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetJob(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.QueuedAt.Equal(tt.want) {
			t.Errorf("%s: queued_at %v, want %v", tt.name, got.QueuedAt, tt.want)
		}
	}
}
