package github

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/store"
)

type workflowRun struct {
	Name            string  `json:"name"`
	RunNumber       int64   `json:"run_number"`
	RunAttempt      int     `json:"run_attempt"`
	Event           string  `json:"event"`
	HeadBranch      string  `json:"head_branch"`
	HeadSHA         string  `json:"head_sha"`
	Actor           account `json:"actor"`
	TriggeringActor account `json:"triggering_actor"`
	HeadCommit      struct {
		Message string `json:"message"`
	} `json:"head_commit"`
	PullRequests []struct {
		Number int `json:"number"`
		Base   struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_requests"`
	Repository     store.Repository  `json:"repository"`
	HeadRepository *store.Repository `json:"head_repository"`
}

type workflowJob struct {
	ID          int64      `json:"id"`
	RunAttempt  int        `json:"run_attempt"`
	Name        string     `json:"name"`
	HTMLURL     string     `json:"html_url"`
	RunnerName  string     `json:"runner_name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at"`
	// The GitHub step object has the JSON names of store.JobStep.
	Steps []store.JobStep `json:"steps"`
}

// CheckJob tells if the repository is private and if the run comes from a
// fork, and gives the run data for the cache refs. It reads the run,
// because the scale set message has no head repository; the run also has
// the current private flag. The result is cached by run ID for one hour, so
// the jobs of one run cost one request.
func (s *Service) CheckJob(ctx context.Context, repository string, runID int64) (scheduler.JobCheck, error) {
	s.checkMu.Lock()
	cached, ok := s.checks[runID]
	s.checkMu.Unlock()
	if ok && time.Since(cached.at) <= time.Hour {
		return cached.JobCheck, nil
	}
	run, err := s.run(ctx, repository, runID)
	if err != nil {
		if unreadable(err) {
			err = fmt.Errorf("%w: %w", scheduler.ErrRunUnreadable, err)
		}
		return scheduler.JobCheck{}, err
	}
	c := scheduler.JobCheck{Private: run.Repository.Private, Fork: run.fork(), Ref: run.ref()}
	now := time.Now()
	s.checkMu.Lock()
	maps.DeleteFunc(s.checks, func(_ int64, old runCheck) bool { return now.Sub(old.at) > time.Hour })
	s.checks[runID] = runCheck{c, now}
	s.checkMu.Unlock()
	return c, nil
}

type runCheck struct {
	scheduler.JobCheck
	at time.Time
}

// unreadable tells if an error stays when runplane reads the run again: the
// app has no installation for the repository, or GitHub hides the run (404)
// or refuses it for a reason other than a rate limit (403).
func unreadable(err error) bool {
	var e *apiError
	if errors.As(err, &e) {
		return e.status == http.StatusNotFound || e.status == http.StatusForbidden && e.retryAt.IsZero()
	}
	return errors.Is(err, errNoInstallation)
}

// ref returns the run data for the cache refs. GitHub lists no pull
// request for a run of a fork.
func (run workflowRun) ref() store.RunRef {
	r := store.RunRef{Event: run.Event, HeadBranch: run.HeadBranch}
	if len(run.PullRequests) > 0 {
		r.PRNumber, r.BaseRef = run.PullRequests[0].Number, run.PullRequests[0].Base.Ref
	}
	return r
}

// fork tells if the code of the run comes from a different repository.
// That is the case for pull requests from forks, and for workflow_run runs
// that such a pull request starts. A pull request run without a head
// repository (a deleted fork) counts as a fork.
func (run workflowRun) fork() bool {
	if run.HeadRepository == nil {
		return strings.HasPrefix(run.Event, "pull_request")
	}
	return run.HeadRepository.ID != run.Repository.ID
}

// Enrich reads the run and its jobs and writes the enrichment fields. For a
// cancelled job, it also writes another result that GitHub has: the
// scheduler cancels a job whose sandbox it lost.
func (s *Service) Enrich(ctx context.Context, job store.Job) error {
	_, err := s.enrich(ctx, job)
	return err
}

// enrich is Enrich that returns the GitHub job ID.
func (s *Service) enrich(ctx context.Context, job store.Job) (int64, error) {
	if job.RunID == 0 {
		return 0, fmt.Errorf("enrich job %s: the job has no run ID", job.ID)
	}
	run, err := s.run(ctx, job.Repository, job.RunID)
	if err != nil {
		return 0, err
	}
	a, err := s.repoAuth(ctx, job.Repository)
	if err != nil {
		return 0, err
	}
	jobs, err := list[workflowJob](ctx, s.api, a,
		fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=all&per_page=100", job.Repository, job.RunID), "jobs")
	if err != nil {
		return 0, fmt.Errorf("list jobs of run %d: %w", job.RunID, err)
	}
	var runnerName string
	if job.SandboxID != "" {
		if sb, err := s.st.GetSandbox(ctx, job.SandboxID); err == nil {
			runnerName = sb.RunnerName
		}
	}
	gj, ok := matchJob(jobs, job.JobName, runnerName, job.RunAttempt)
	if !ok {
		return 0, fmt.Errorf("enrich job %s: run %d has no job %q", job.ID, job.RunID, job.JobName)
	}

	ref := run.ref()
	err = s.st.SetJobEnrichment(ctx, job.ID, store.JobEnrichment{
		GitHubJobID: gj.ID, WorkflowName: run.Name, RunNumber: run.RunNumber,
		RunAttempt: cmp.Or(gj.RunAttempt, run.RunAttempt), HeadBranch: run.HeadBranch, HeadSHA: run.HeadSHA,
		CommitMessage: run.HeadCommit.Message, Event: run.Event,
		// The triggering actor started this attempt; the actor started the first.
		Actor:    cmp.Or(run.TriggeringActor.Login, run.Actor.Login),
		PRNumber: ref.PRNumber, BaseRef: ref.BaseRef, HTMLURL: gj.HTMLURL, Steps: gj.Steps, QueuedAt: gj.CreatedAt,
	})
	if err != nil {
		return 0, err
	}
	if r := conclusionResult(gj.Conclusion); job.Result == store.ResultCancelled && gj.Status == "completed" && r != "" && r != store.ResultCancelled {
		m := store.JobMessage{RunnerRequestID: job.RunnerRequestID, State: store.JobCompleted, Result: r}
		if gj.CompletedAt != nil {
			m.FinishedAt = *gj.CompletedAt
		}
		if _, err := s.st.UpsertJob(ctx, m); err != nil {
			return 0, err
		}
	}
	return gj.ID, nil
}

// Steps reads the GitHub job: one request.
func (s *Service) Steps(ctx context.Context, job store.Job) ([]store.JobStep, error) {
	if job.GitHubJobID == 0 {
		return nil, nil
	}
	a, err := s.repoAuth(ctx, job.Repository)
	if err != nil {
		return nil, err
	}
	var gj workflowJob
	err = s.api.json(ctx, call{method: http.MethodGet, path: fmt.Sprintf("/repos/%s/actions/jobs/%d", job.Repository, job.GitHubJobID), auth: a}, &gj)
	if err != nil {
		return nil, fmt.Errorf("read job %d: %w", job.GitHubJobID, err)
	}
	// nil steps tell the scheduler that the job is not on GitHub yet.
	if gj.Steps == nil {
		return []store.JobStep{}, nil
	}
	return gj.Steps, nil
}

// conclusionResult maps the conclusion of a GitHub job to a job result, or
// returns "" if the job has none yet.
func conclusionResult(c string) string {
	switch c {
	case "":
		return ""
	case "success", "neutral":
		return store.ResultSucceeded
	case "cancelled":
		return store.ResultCancelled
	case "skipped":
		return store.ResultSkipped
	}
	return store.ResultFailed
}

// matchJob finds the GitHub job of a runplane job. The runner name is
// unique, so it wins. Without it, the job name decides: in the given run
// attempt, or in the newest attempt when the attempt is 0 (unknown).
func matchJob(jobs []workflowJob, name, runnerName string, attempt int) (workflowJob, bool) {
	if runnerName != "" {
		if i := slices.IndexFunc(jobs, func(j workflowJob) bool { return strings.EqualFold(j.RunnerName, runnerName) }); i >= 0 {
			return jobs[i], true
		}
	}
	var best workflowJob
	found := false
	for _, j := range jobs {
		if j.Name != name {
			continue
		}
		if attempt > 0 && j.RunAttempt == attempt {
			return j, true
		}
		if attempt == 0 && (!found || j.RunAttempt > best.RunAttempt) {
			best, found = j, true
		}
	}
	return best, found
}

// DownloadLog writes the complete log of the job from GitHub to path. It
// writes a temporary file next to path and renames it, so path is never a
// partial log.
func (s *Service) DownloadLog(ctx context.Context, job store.Job, path string) error {
	id, err := s.githubJobID(ctx, job)
	if err != nil {
		return err
	}
	a, err := s.repoAuth(ctx, job.Repository)
	if err != nil {
		return err
	}
	// GitHub answers with a redirect to a short-lived blob URL. The blob
	// store must not get the token, so runplane follows the redirect itself.
	resp, err := s.api.do(ctx, call{
		method: http.MethodGet, path: fmt.Sprintf("/repos/%s/actions/jobs/%d/logs", job.Repository, id),
		auth: a, timeout: logTimeout, noRedirect: true,
	})
	if err != nil {
		return fmt.Errorf("download log of job %d: %w", id, err)
	}
	if loc := resp.Header.Get("Location"); resp.StatusCode >= 300 && loc != "" {
		resp.Body.Close()
		resp, err = s.api.do(ctx, call{method: http.MethodGet, path: loc, timeout: logTimeout})
		if err != nil {
			return fmt.Errorf("download log of job %d: %w", id, err)
		}
	}
	defer resp.Body.Close()
	return writeFile(path, resp.Body)
}

const logTimeout = 10 * time.Minute

func writeFile(path string, r io.Reader) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func (s *Service) Rerun(ctx context.Context, job store.Job) error {
	id, err := s.githubJobID(ctx, job)
	if err != nil {
		return err
	}
	a, err := s.repoAuth(ctx, job.Repository)
	if err != nil {
		return err
	}
	err = s.api.json(ctx, call{method: http.MethodPost, path: fmt.Sprintf("/repos/%s/actions/jobs/%d/rerun", job.Repository, id), auth: a}, nil)
	if err != nil {
		return fmt.Errorf("re-run job %d: %w", id, err)
	}
	return nil
}

// githubJobID returns the GitHub job ID, and enriches the job first if the
// ID is not known yet.
func (s *Service) githubJobID(ctx context.Context, job store.Job) (int64, error) {
	if job.GitHubJobID != 0 {
		return job.GitHubJobID, nil
	}
	return s.enrich(ctx, job)
}

func (s *Service) run(ctx context.Context, repository string, runID int64) (workflowRun, error) {
	a, err := s.repoAuth(ctx, repository)
	if err != nil {
		return workflowRun{}, err
	}
	var run workflowRun
	err = s.api.json(ctx, call{method: http.MethodGet, path: fmt.Sprintf("/repos/%s/actions/runs/%d", repository, runID), auth: a}, &run)
	if err != nil {
		return workflowRun{}, fmt.Errorf("read run %d of %s: %w", runID, repository, err)
	}
	return run, nil
}

var errNoInstallation = errors.New("no installation of the GitHub App can read")

// repoAuth returns the installation that can read the repository: the one
// that lists it, or else the one of the repository owner.
func (s *Service) repoAuth(ctx context.Context, repository string) (auth, error) {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("bad repository name %q", repository)
	}
	if r, err := s.st.GetRepositoryByName(ctx, repository); err == nil {
		return installAuth{s, r.InstallationID}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	insts, err := s.st.ListInstallations(ctx)
	if err != nil {
		return nil, err
	}
	for _, inst := range insts {
		if strings.EqualFold(inst.AccountLogin, owner) {
			return installAuth{s, inst.ID}, nil
		}
	}
	return nil, fmt.Errorf("%w %s", errNoInstallation, repository)
}
