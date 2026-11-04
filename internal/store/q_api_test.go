package store

import (
	"context"
	"testing"
	"time"
)

// CountJobs repeats the conditions of ListJobs, so each filter must give
// the count of all pages of the list.
func TestCountJobsMatchesListJobs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	add := func(id int64, state, result, repo, pool, branch, sha, job string, q int) {
		t.Helper()
		j, err := s.UpsertJob(ctx, JobMessage{RunnerRequestID: id, State: state, Result: result, Repository: repo,
			PoolID: pool, JobName: job, QueuedAt: sec(q)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetJobEnrichment(ctx, j.ID, JobEnrichment{HeadBranch: branch, HeadSHA: sha}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetJobSandbox(ctx, j.ID, "sb", "srv-"+repo); err != nil {
			t.Fatal(err)
		}
	}
	add(1, JobQueued, "", "acme/api", "p1", "main", "aaa111", "lint", 1)
	add(2, JobAssigned, "", "acme/api", "p1", "main", "bbb222", "test", 2)
	add(3, JobRunning, "", "acme/web", "p2", "feat/x_y", "ccc333", "build", 3)
	add(4, JobCompleted, ResultSucceeded, "acme/web", "p2", "main", "ddd444", "e2e", 4)
	add(5, JobCompleted, ResultFailed, "acme/api", "p1", "fix", "eee555", "test", 5)
	add(6, JobCompleted, ResultCancelled, "acme/api", "p2", "main", "fff666", "lint", 6)

	listed := func(f JobFilter) int {
		t.Helper()
		f.Limit = 2
		n := 0
		for {
			jobs, next, err := s.ListJobs(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			n += len(jobs)
			if next == "" {
				return n
			}
			f.Cursor = next
		}
	}
	for _, f := range []JobFilter{
		{},
		{Status: "queued"},
		{Status: "running"},
		{Status: ResultSucceeded},
		{Status: ResultFailed},
		{Status: ResultCancelled},
		{Repository: "acme/api"},
		{PoolID: "p2"},
		{ServerID: "srv-acme/web"},
		{Q: "test"},
		{Q: "ddd4"},
		{Q: "x_y"},
		{Since: sec(2), Until: sec(5)},
		{Status: "queued", Repository: "acme/api", Q: "lint"},
	} {
		got, err := s.CountJobs(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		if want := listed(f); got != want {
			t.Errorf("%+v: CountJobs = %d, ListJobs pages have %d", f, got, want)
		}
	}
	// A cursor does not change the total.
	all, err := s.CountJobs(ctx, JobFilter{Cursor: "anything", Limit: 1})
	if err != nil || all != 6 {
		t.Errorf("count with a cursor = %d, %v; want 6", all, err)
	}
}

func TestDoctorResultReplacesTheLastOne(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.CreateServer(ctx, Server{ID: "srv", Name: "srv", Kind: ServerAgent, Status: ServerOnline, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDoctorResult(ctx, "srv"); err == nil {
		t.Fatal("a server without a doctor run has a result")
	}
	for i, status := range []string{"fail", "ok"} {
		err := s.SaveDoctorResult(ctx, DoctorResult{ServerID: "srv", CheckedAt: now.Add(time.Duration(i) * time.Minute),
			Checks: []DoctorCheck{{ID: "clock", Status: status}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.GetDoctorResult(ctx, "srv")
	if err != nil || !r.CheckedAt.Equal(now.Add(time.Minute)) || len(r.Checks) != 1 || r.Checks[0].Status != "ok" {
		t.Fatalf("doctor result %+v, %v; want the second run", r, err)
	}
	// The result goes with its server.
	if err := s.DeleteServer(ctx, "srv"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM server_doctor").Scan(&n); err != nil || n != 0 {
		t.Fatalf("doctor rows after the server is deleted: %d, %v", n, err)
	}
}
