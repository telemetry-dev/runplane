package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/store"
)

// runJSON is run 555 of acme/web as GitHub sends it. headRepoID 0 means a
// run without a head repository.
func runJSON(event string, private bool, headRepoID int64) map[string]any {
	run := map[string]any{
		"id": 555, "name": "CI", "run_number": 81, "run_attempt": 2, "event": event,
		"head_branch": "fix-cache", "head_sha": "3f2a9c1e", "head_repository": nil,
		"actor": map[string]any{"login": "alice"}, "triggering_actor": map[string]any{"login": "bob"},
		"head_commit":   map[string]any{"message": "Fix the cache key"},
		"pull_requests": []map[string]any{{"number": 17, "base": map[string]any{"ref": "main"}}},
		"repository":    map[string]any{"id": 100, "full_name": "acme/web", "private": private},
	}
	if headRepoID != 0 {
		run["head_repository"] = map[string]any{"id": headRepoID, "full_name": "someone/web", "private": false}
	}
	return run
}

var testJobs = []map[string]any{
	{"id": 901, "run_attempt": 1, "name": "build", "runner_name": "runplane-old", "html_url": "https://x/901"},
	{"id": 902, "run_attempt": 2, "name": "build", "runner_name": "runplane-01abc", "html_url": "https://x/902", "steps": []any{
		map[string]any{"number": 1, "name": "Set up job", "status": "completed", "conclusion": "success",
			"started_at": "2026-11-04T10:00:00Z", "completed_at": "2026-11-04T10:01:00Z"},
		map[string]any{"number": 2, "name": "Run tests", "status": "completed", "conclusion": "failure",
			"started_at": "2026-11-04T10:01:00Z", "completed_at": "2026-11-04T10:02:00Z"},
	}},
	{"id": 903, "run_attempt": 2, "name": "lint", "runner_name": "runplane-01def", "html_url": "https://x/903"},
}

func (e *env) serveRun(jobs []map[string]any) {
	e.mux.HandleFunc("GET /api/v3/repos/acme/web/actions/runs/555", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, runJSON("push", true, 100))
	})
	e.mux.HandleFunc("GET /api/v3/repos/acme/web/actions/runs/555/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter") != "all" {
			e.t.Errorf("filter = %q, want all attempts", r.URL.Query().Get("filter"))
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(jobs), "jobs": jobs})
	})
}

func TestCheckJob(t *testing.T) {
	ref := func(event string) store.RunRef {
		return store.RunRef{Event: event, HeadBranch: "fix-cache", PRNumber: 17, BaseRef: "main"}
	}
	tests := []struct {
		name       string
		event      string
		private    bool
		headRepoID int64
		want       scheduler.JobCheck
	}{
		{"push in the repository", "push", true, 100, scheduler.JobCheck{Private: true, Ref: ref("push")}},
		{"push without head repository", "push", true, 0, scheduler.JobCheck{Private: true, Ref: ref("push")}},
		{"pull request from a fork", "pull_request", true, 777, scheduler.JobCheck{Private: true, Fork: true, Ref: ref("pull_request")}},
		{"pull request from a deleted fork", "pull_request", false, 0, scheduler.JobCheck{Fork: true, Ref: ref("pull_request")}},
		{"workflow_run started by a fork", "workflow_run", true, 777, scheduler.JobCheck{Private: true, Fork: true, Ref: ref("workflow_run")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := acmeEnv(t)
			e.mux.HandleFunc("GET /api/v3/repos/acme/web/actions/runs/555", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, runJSON(tt.event, tt.private, tt.headRepoID))
			})
			got, err := e.svc.CheckJob(context.Background(), "acme/web", 555)
			if err != nil || got != tt.want {
				t.Errorf("CheckJob = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestCheckJobErrors(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		status     int
		limited    bool
		unreadable bool
	}{
		{"no installation for the repository", "stranger/web", 0, false, true},
		{"run hidden from the app", "acme/web", http.StatusNotFound, false, true},
		{"app may not read the repository", "acme/web", http.StatusForbidden, false, true},
		{"rate limit", "acme/web", http.StatusForbidden, true, false},
		{"GitHub fails", "acme/web", http.StatusBadGateway, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := acmeEnv(t)
			e.svc.api.RetryWait = time.Millisecond
			e.mux.HandleFunc("GET /api/v3/repos/acme/web/actions/runs/555", func(w http.ResponseWriter, r *http.Request) {
				if tt.limited {
					w.Header().Set("X-RateLimit-Remaining", "0")
					w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
				}
				writeJSON(w, tt.status, map[string]any{"message": "Not Found"})
			})
			_, err := e.svc.CheckJob(context.Background(), tt.repository, 555)
			if err == nil || errors.Is(err, scheduler.ErrRunUnreadable) != tt.unreadable {
				t.Errorf("CheckJob error = %v, want ErrRunUnreadable %v", err, tt.unreadable)
			}
		})
	}
}

func TestEnrich(t *testing.T) {
	e := acmeEnv(t)
	ctx := context.Background()
	e.serveRun(testJobs)
	if err := e.st.InsertSandbox(ctx, store.Sandbox{ID: "sb1", PoolID: "p1", ServerID: "s1", Mode: sandbox.ModeContainer,
		State: sandbox.StateBusy, RunnerName: "runplane-01abc", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	job, err := e.st.UpsertJob(ctx, store.JobMessage{RunnerRequestID: 1, State: store.JobRunning, Repository: "acme/web",
		JobName: "build", RunID: 555, QueuedAt: time.Now()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	job.SandboxID = "sb1"
	got, err := e.svc.Enrich(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if got.GitHubJobID != 902 || got.RunAttempt != 2 || got.HTMLURL != "https://x/902" {
		t.Errorf("matched job %d attempt %d %s, want 902 by runner name", got.GitHubJobID, got.RunAttempt, got.HTMLURL)
	}
	if got.WorkflowName != "CI" || got.RunNumber != 81 || got.HeadBranch != "fix-cache" || got.HeadSHA != "3f2a9c1e" ||
		got.CommitMessage != "Fix the cache key" || got.Actor != "bob" || got.PRNumber != 17 || got.Result != "" {
		t.Errorf("enrichment = %+v", got)
	}
	if len(got.Steps) != 2 || got.Steps[1].Name != "Run tests" || got.Steps[1].Conclusion != "failure" || got.Steps[1].StartedAt == nil {
		t.Errorf("steps = %+v", got.Steps)
	}
	// The scheduler records the data: Enrich does not change the job.
	if stored, _ := e.st.GetJob(ctx, job.ID); stored.GitHubJobID != 0 || stored.EnrichedAt != nil {
		t.Errorf("stored job = GitHub job %d, enriched at %v; want no change", stored.GitHubJobID, stored.EnrichedAt)
	}
}

// Enrich gives the result of a job that GitHub completed, so that the
// scheduler can replace a result that it guessed.
func TestEnrichGivesTheGitHubResult(t *testing.T) {
	e := acmeEnv(t)
	ctx := context.Background()
	e.serveRun([]map[string]any{{"id": 905, "run_attempt": 2, "name": "build", "status": "completed",
		"conclusion": "failure", "completed_at": "2026-11-04T10:07:00Z"}})
	job := store.Job{ID: "j1", RunnerRequestID: 1, State: store.JobCompleted, Result: store.ResultCancelled, ResultGuessed: true,
		Repository: "acme/web", JobName: "build", RunID: 555}
	got, err := e.svc.Enrich(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	finished := time.Date(2026, 11, 4, 10, 7, 0, 0, time.UTC)
	if got.Result != store.ResultFailed || !got.FinishedAt.Equal(finished) {
		t.Errorf("result %q, finished %v; want failed from GitHub at %v", got.Result, got.FinishedAt, finished)
	}
}

func TestMatchJob(t *testing.T) {
	jobs := []workflowJob{
		{ID: 901, RunAttempt: 1, Name: "build", RunnerName: "runplane-old"},
		{ID: 902, RunAttempt: 2, Name: "build", RunnerName: "runplane-01abc"},
		{ID: 903, RunAttempt: 2, Name: "lint", RunnerName: "runplane-01def"},
		{ID: 904, RunAttempt: 2, Name: "deploy"},
	}
	tests := []struct {
		name, job, runner string
		attempt           int
		want              int64
	}{
		{"runner name wins over the newest attempt", "build", "runplane-old", 0, 901},
		{"runner name without regard to case", "build", "RUNPLANE-01ABC", 0, 902},
		{"name in the newest attempt without a runner", "build", "", 0, 902},
		{"name in the known attempt", "build", "", 1, 901},
		{"unknown runner falls back to the name", "lint", "runplane-zzz", 0, 903},
		{"no job of the attempt", "deploy", "", 1, 0},
		{"no job of the name", "test", "", 0, 0},
	}
	for _, tt := range tests {
		got, ok := matchJob(jobs, tt.job, tt.runner, tt.attempt)
		if ok != (tt.want != 0) || got.ID != tt.want {
			t.Errorf("%s: matchJob = %d, %v; want %d", tt.name, got.ID, ok, tt.want)
		}
	}
}

func TestDownloadLogFollowsRedirectWithoutToken(t *testing.T) {
	e := acmeEnv(t)
	// GitHub sends the log from a blob store on a different host.
	blob := strings.Replace(e.srv.URL, "127.0.0.1", "localhost", 1)
	e.mux.HandleFunc("GET /api/v3/repos/acme/web/actions/jobs/902/logs", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_10_") {
			t.Errorf("log request without the installation token")
		}
		http.Redirect(w, r, blob+"/blob/job-902.txt?sig=abc", http.StatusFound)
	})
	e.mux.HandleFunc("GET /blob/job-902.txt", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.URL.Query().Get("sig") != "abc" {
			t.Errorf("blob request with Authorization %q and sig %q, want none and abc", r.Header.Get("Authorization"), r.URL.Query().Get("sig"))
		}
		io.WriteString(w, "2026-11-04T10:00:00Z Run tests\nok\n")
	})
	log, err := e.svc.DownloadLog(context.Background(), store.Job{ID: "job1", Repository: "acme/web", GitHubJobID: 902})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if b, err := io.ReadAll(log); err != nil || string(b) != "2026-11-04T10:00:00Z Run tests\nok\n" {
		t.Errorf("log = %q, %v", b, err)
	}
}

func TestDownloadLogOfAnExpiredLog(t *testing.T) {
	e := acmeEnv(t)
	e.mux.HandleFunc("GET /api/v3/repos/acme/web/actions/jobs/902/logs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusGone, map[string]any{"message": "Log has expired"})
	})
	if log, err := e.svc.DownloadLog(context.Background(), store.Job{ID: "job1", Repository: "acme/web", GitHubJobID: 902}); err == nil {
		log.Close()
		t.Fatal("DownloadLog succeeded for an expired log")
	}
}

func TestRerunFindsJobIDFirst(t *testing.T) {
	e := acmeEnv(t)
	ctx := context.Background()
	e.serveRun(testJobs)
	var rerun []string
	e.mux.HandleFunc("POST /api/v3/repos/acme/web/actions/jobs/{id}/rerun", func(w http.ResponseWriter, r *http.Request) {
		rerun = append(rerun, r.PathValue("id"))
		writeJSON(w, http.StatusCreated, map[string]any{})
	})
	job, err := e.st.UpsertJob(ctx, store.JobMessage{RunnerRequestID: 2, State: store.JobCompleted, Result: store.ResultFailed,
		Repository: "acme/web", JobName: "lint", RunID: 555, QueuedAt: time.Now()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Rerun(ctx, job); err != nil {
		t.Fatal(err)
	}
	if len(rerun) != 1 || rerun[0] != "903" {
		t.Errorf("re-run jobs = %v, want [903]", rerun)
	}
	if got, _ := e.st.GetJob(ctx, job.ID); got.GitHubJobID != 0 || got.Result != store.ResultFailed {
		t.Errorf("job after the re-run = GitHub job %d, result %q; want no change", got.GitHubJobID, got.Result)
	}
}

func TestSteps(t *testing.T) {
	e := acmeEnv(t)
	ctx := context.Background()
	e.mux.HandleFunc("GET /api/v3/repos/acme/web/actions/jobs/902", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, testJobs[1])
	})
	got, err := e.svc.Steps(ctx, store.Job{Repository: "acme/web", GitHubJobID: 902})
	if err != nil || len(got) != 2 || got[1].Name != "Run tests" || got[1].Conclusion != "failure" {
		t.Errorf("steps = %+v, %v; want Set up job and Run tests", got, err)
	}
	if got, err := e.svc.Steps(ctx, store.Job{Repository: "acme/web"}); got != nil || err != nil {
		t.Errorf("Steps of a job with no GitHub job ID = %v, %v; want nil, nil", got, err)
	}
}
