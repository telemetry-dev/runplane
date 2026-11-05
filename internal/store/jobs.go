package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/ids"
)

// Job states, in their order. A job never goes back to an earlier state.
const (
	JobQueued    = "queued"
	JobAssigned  = "assigned"
	JobRunning   = "running"
	JobCompleted = "completed"
)

// Job results.
const (
	ResultSucceeded = "succeeded"
	ResultFailed    = "failed"
	ResultCancelled = "cancelled"
	ResultSkipped   = "skipped"
)

// ResultOf returns the job result for a result name of GitHub or of the
// runner, in any case, or "" for "". The scale set messages and the runner
// use names such as Succeeded, SucceededWithIssues, Canceled, and
// Abandoned. The REST API uses conclusions such as success, neutral,
// failure, and cancelled.
func ResultOf(name string) string {
	switch strings.ToLower(name) {
	case "":
		return ""
	case "succeeded", "succeededwithissues", "success", "neutral":
		return ResultSucceeded
	case "canceled", "cancelled", "abandoned":
		return ResultCancelled
	case "skipped":
		return ResultSkipped
	}
	return ResultFailed
}

// Job log states.
const (
	LogNone    = "none"
	LogLive    = "live"   // the sandbox streams the log
	LogStored  = "stored" // the GitHub log is in the logs folder
	LogExpired = "expired"
)

// NotRunReason is the reject reason of a job that ExpireQueuedJobs closed.
const NotRunReason = "runplane did not run this job."

type Job struct {
	ID              string     `json:"id"`
	RunnerRequestID int64      `json:"runner_request_id"`
	GitHubJobID     int64      `json:"github_job_id"`
	ScaleSetID      string     `json:"scale_set_id"`
	PoolID          string     `json:"pool_id"`
	SandboxID       string     `json:"sandbox_id"`
	ServerID        string     `json:"server_id"`
	Repository      string     `json:"repository"` // owner/name
	WorkflowRef     string     `json:"workflow_ref"`
	WorkflowFile    string     `json:"workflow_file"` // .github/workflows/ci.yml
	WorkflowName    string     `json:"workflow_name"`
	JobName         string     `json:"job_name"`
	RunID           int64      `json:"run_id"`
	RunNumber       int64      `json:"run_number"`
	RunAttempt      int        `json:"run_attempt"`
	HeadBranch      string     `json:"head_branch"`
	HeadSHA         string     `json:"head_sha"`
	CommitMessage   string     `json:"commit_message"`
	Event           string     `json:"event"`
	Actor           string     `json:"actor"`
	PRNumber        int        `json:"pr_number"` // 0 = not a pull request
	BaseRef         string     `json:"base_ref"`  // base branch of the pull request
	Labels          []string   `json:"labels"`
	State           string     `json:"state"`
	Result          string     `json:"result"`         // empty until completed
	ResultGuessed   bool       `json:"result_guessed"` // runplane guessed it; a result from GitHub replaces it
	RejectReason    string     `json:"reject_reason"`
	QueuedAt        time.Time  `json:"queued_at"`
	AssignedAt      *time.Time `json:"assigned_at"`
	StartedAt       *time.Time `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
	HTMLURL         string     `json:"html_url"`
	Steps           []JobStep  `json:"steps"`
	LogState        string     `json:"log_state"`
	CPUPeak         *float64   `json:"cpu_peak"`
	CPUAvg          *float64   `json:"cpu_avg"`
	MemPeakMB       *int       `json:"mem_peak_mb"`
	Debug           bool       `json:"debug"`
	EnrichedAt      *time.Time `json:"enriched_at"`
}

// JobStep is one step from the GitHub jobs API.
type JobStep struct {
	Number      int        `json:"number"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`     // queued | in_progress | completed
	Conclusion  string     `json:"conclusion"` // success | failure | cancelled | skipped
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

// JobMessage is one scale set job message. State is the state that the
// message reports: queued (JobAvailable), assigned (JobAssigned), running
// (JobStarted), or completed (JobCompleted). Zero values are not in the message.
type JobMessage struct {
	RunnerRequestID int64
	State           string
	Result          string // for completed
	Guessed         bool   // the result is a guess of runplane, not a report of GitHub
	ScaleSetID      string
	PoolID          string
	Repository      string // owner/name
	WorkflowRef     string // owner/name/.github/workflows/ci.yml@refs/heads/main
	JobName         string
	RunID           int64
	Event           string
	Labels          []string
	Debug           bool
	QueuedAt        time.Time
	AssignedAt      time.Time
	StartedAt       time.Time
	FinishedAt      time.Time
}

// RunRef is the run data that gives the cache refs of a job.
type RunRef struct {
	Event      string
	HeadBranch string
	PRNumber   int    // 0 = not a pull request
	BaseRef    string // base branch of the pull request
}

// JobEnrichment is the run data from the GitHub REST API.
type JobEnrichment struct {
	GitHubJobID   int64
	WorkflowName  string
	RunNumber     int64
	RunAttempt    int
	HeadBranch    string
	HeadSHA       string
	CommitMessage string
	Event         string
	Actor         string
	PRNumber      int
	BaseRef       string
	HTMLURL       string
	Steps         []JobStep
	// QueuedAt is when GitHub made the job. The scale set messages come
	// only when a runner can take the job, so their queue time is later.
	QueuedAt time.Time
}

type MetricsSummary struct {
	CPUPeak   float64 `json:"cpu_peak"`
	CPUAvg    float64 `json:"cpu_avg"`
	MemPeakMB int     `json:"mem_peak_mb"`
}

type JobMetric struct {
	At         time.Time `json:"at"`
	CPUPercent float64   `json:"cpu_percent"`
	MemoryMB   int       `json:"memory_mb"`
}

// RunUsage is the CPU and memory use of one completed run of a job, and
// the size of its sandbox.
type RunUsage struct {
	CPUPeak   float64 // percent of VCPU
	CPUAvg    float64 // percent of VCPU
	MemPeakMB int
	VCPU      int
	MemoryMB  int
}

// JobFilter selects jobs for ListJobs. Zero values select all.
type JobFilter struct {
	Status     string // running, queued (also assigned), succeeded, failed, or cancelled
	Repository string
	PoolID     string
	ServerID   string
	Q          string    // text in the job, workflow, repository, or branch name, or a commit SHA prefix
	Since      time.Time // queued_at >= Since
	Until      time.Time // queued_at < Until
	Cursor     string    // from the previous page
	Limit      int       // default 50, at most 200
}

type Facet struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// JobFacets are the values for the filters of the jobs list.
type JobFacets struct {
	Repositories []string `json:"repositories"`
	Pools        []Facet  `json:"pools"` // the name is the pool label
	Servers      []Facet  `json:"servers"`
}

const jobCols = `id, runner_request_id, github_job_id, scale_set_id, pool_id, sandbox_id, server_id, repository,
	workflow_ref, workflow_file, workflow_name, job_name, run_id, run_number, run_attempt, head_branch, head_sha,
	commit_message, event, actor, pr_number, base_ref, labels_json, state, result, result_guessed, reject_reason, queued_at,
	assigned_at, started_at, finished_at, html_url, steps_json, log_state, cpu_peak, cpu_avg, mem_peak_mb, debug, enriched_at`

func (j *Job) cols() []any {
	return []any{&j.ID, &j.RunnerRequestID, &j.GitHubJobID, &j.ScaleSetID, &j.PoolID, &j.SandboxID, &j.ServerID,
		&j.Repository, &j.WorkflowRef, &j.WorkflowFile, &j.WorkflowName, &j.JobName, &j.RunID, &j.RunNumber,
		&j.RunAttempt, &j.HeadBranch, &j.HeadSHA, &j.CommitMessage, &j.Event, &j.Actor, &j.PRNumber,
		&j.BaseRef, jsonCol(&j.Labels), &j.State, text(&j.Result), &j.ResultGuessed, &j.RejectReason, at(&j.QueuedAt),
		atPtr(&j.AssignedAt), atPtr(&j.StartedAt), atPtr(&j.FinishedAt), &j.HTMLURL, jsonCol(&j.Steps), &j.LogState,
		&j.CPUPeak, &j.CPUAvg, &j.MemPeakMB, &j.Debug, atPtr(&j.EnrichedAt)}
}

// Ran tells if the job started on a runner. A job that never ran has no log.
func (j Job) Ran() bool { return j.StartedAt != nil || j.SandboxID != "" }

// UpsertJob records a job message at now. It returns the job, and its state
// before the message, or "" for a new job. It is safe to call again with
// the same message. The state only moves forward (queued, assigned,
// running, completed), and a completed job keeps its state, result, and
// finish time, except in the two cases that mergeJob describes. A message
// can still fill fields that the job does not have yet.
func (s *Store) UpsertJob(ctx context.Context, m JobMessage, now time.Time) (Job, string, error) {
	if stateRank(m.State) < 0 {
		return Job{}, "", fmt.Errorf("upsert job: unknown state %q", m.State)
	}
	var j Job
	var prev string
	err := s.tx(ctx, "upsert job", func(tx *sql.Tx) error {
		var err error
		j, err = scanRow[Job](tx.QueryRowContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE runner_request_id = ?", m.RunnerRequestID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			j = Job{ID: ids.New(), RunnerRequestID: m.RunnerRequestID, State: JobQueued, LogState: LogNone}
		case err != nil:
			return err
		default:
			prev = j.State
		}
		mergeJob(&j, m, now)
		return writeJob(ctx, tx, j)
	})
	if err != nil {
		return Job{}, "", err
	}
	return j, prev, nil
}

func writeJob(ctx context.Context, tx *sql.Tx, j Job) error {
	_, err := tx.ExecContext(ctx, upsert("jobs", jobCols, "id"), j.cols()...)
	return err
}

func stateRank(state string) int {
	return slices.Index([]string{JobQueued, JobAssigned, JobRunning, JobCompleted}, state)
}

func mergeJob(j *Job, m JobMessage, now time.Time) {
	// A cancelled job that never ran can come back. ExpireQueuedJobs closes
	// the jobs that runplane did not take, whatever their reject reason, and
	// GitHub cancels an assignment that no runner takes in time and then
	// offers the job again (actions/scaleset, Job Reassignment). Such a job
	// opens again, and the scheduler checks it again.
	if j.State == JobCompleted && j.Result == ResultCancelled && !j.Ran() && m.State != JobCompleted {
		j.State, j.Result, j.ResultGuessed, j.FinishedAt, j.RejectReason = JobQueued, "", false, nil, ""
		j.LogState = LogNone
	}
	// GitHub reports a job as completed one time, and its result replaces a
	// result that runplane guessed.
	if j.State == JobCompleted && j.ResultGuessed && m.State == JobCompleted && !m.Guessed && m.Result != "" {
		j.Result, j.ResultGuessed = m.Result, false
		if !m.FinishedAt.IsZero() {
			t := m.FinishedAt.UTC()
			j.FinishedAt = &t
		}
	}
	fillTime := func(dst **time.Time, v time.Time) {
		if *dst == nil && !v.IsZero() {
			t := v.UTC()
			*dst = &t
		}
	}
	j.ScaleSetID = cmp.Or(j.ScaleSetID, m.ScaleSetID)
	j.PoolID = cmp.Or(j.PoolID, m.PoolID)
	j.Repository = cmp.Or(j.Repository, m.Repository)
	j.WorkflowRef = cmp.Or(j.WorkflowRef, m.WorkflowRef)
	j.WorkflowFile = cmp.Or(j.WorkflowFile, workflowFile(m.WorkflowRef))
	j.JobName = cmp.Or(j.JobName, m.JobName)
	j.Event = cmp.Or(j.Event, m.Event)
	j.RunID = cmp.Or(j.RunID, m.RunID)
	if len(j.Labels) == 0 {
		j.Labels = m.Labels
	}
	j.Debug = j.Debug || m.Debug
	if j.QueuedAt.IsZero() {
		j.QueuedAt = orNow(m.QueuedAt, now).UTC()
	}
	fillTime(&j.AssignedAt, m.AssignedAt)
	fillTime(&j.StartedAt, m.StartedAt)

	if stateRank(m.State) <= stateRank(j.State) {
		return
	}
	j.State = m.State
	if m.State == JobCompleted {
		j.Result, j.ResultGuessed = m.Result, m.Guessed
		fillTime(&j.FinishedAt, orNow(m.FinishedAt, now))
	}
}

func orNow(t, now time.Time) time.Time {
	if t.IsZero() {
		return now
	}
	return t
}

// workflowFile returns ".github/workflows/ci.yml" for
// "owner/name/.github/workflows/ci.yml@refs/heads/main".
func workflowFile(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	parts := strings.SplitN(ref, "/", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	return queryOne(ctx, s.r, "get job", scanRow[Job], "SELECT "+jobCols+" FROM jobs WHERE id = ?", id)
}

func (s *Store) GetJobByRunnerRequestID(ctx context.Context, id int64) (Job, error) {
	return queryOne(ctx, s.r, "get job", scanRow[Job], "SELECT "+jobCols+" FROM jobs WHERE runner_request_id = ?", id)
}

// SetJobRunRef writes the run data that gives the cache refs. An empty
// event keeps the event from the job message.
func (s *Store) SetJobRunRef(ctx context.Context, id string, r RunRef) error {
	return exec1(ctx, s.w, "set job run ref", `UPDATE jobs SET event = coalesce(nullif(?, ''), event), head_branch = ?,
		pr_number = ?, base_ref = ? WHERE id = ?`, r.Event, r.HeadBranch, r.PRNumber, r.BaseRef, id)
}

// SetJobEnrichment writes the run data and sets enriched_at to now. An
// empty event keeps the event from the job message.
func (s *Store) SetJobEnrichment(ctx context.Context, id string, e JobEnrichment, now time.Time) error {
	return exec1(ctx, s.w, "set job enrichment", `UPDATE jobs SET github_job_id = ?, workflow_name = ?, run_number = ?,
			run_attempt = ?, head_branch = ?, head_sha = ?, commit_message = ?, event = coalesce(nullif(?, ''), event),
			actor = ?, pr_number = ?, base_ref = ?, html_url = ?, steps_json = ?, enriched_at = ?,
			queued_at = CASE WHEN ? > 0 AND ? < queued_at THEN ? ELSE queued_at END
		WHERE id = ?`,
		e.GitHubJobID, e.WorkflowName, e.RunNumber, e.RunAttempt, e.HeadBranch, e.HeadSHA, e.CommitMessage, e.Event,
		e.Actor, e.PRNumber, e.BaseRef, e.HTMLURL, jsonCol(&e.Steps), ms(now),
		ms(e.QueuedAt), ms(e.QueuedAt), ms(e.QueuedAt), id)
}

func (s *Store) SetJobSteps(ctx context.Context, id string, steps []JobStep) error {
	return exec1(ctx, s.w, "set job steps", "UPDATE jobs SET steps_json = ? WHERE id = ?", jsonCol(&steps), id)
}

func (s *Store) SetJobMetricsSummary(ctx context.Context, id string, m MetricsSummary) error {
	return exec1(ctx, s.w, "set job metrics", "UPDATE jobs SET cpu_peak = ?, cpu_avg = ?, mem_peak_mb = ? WHERE id = ?",
		m.CPUPeak, m.CPUAvg, m.MemPeakMB, id)
}

func (s *Store) SetJobLogState(ctx context.Context, id, state string) error {
	return exec1(ctx, s.w, "set job log state", "UPDATE jobs SET log_state = ? WHERE id = ?", state, id)
}

func (s *Store) SetJobRejectReason(ctx context.Context, id, reason string) error {
	return exec1(ctx, s.w, "set job reject reason", "UPDATE jobs SET reject_reason = ? WHERE id = ?", reason, id)
}

// ListJobs returns one page of jobs, newest first by queued_at and then by
// ID, and the cursor of the next page ("" on the last page).
func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]Job, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	conds, args, err := jobWhere(f)
	if err != nil {
		return nil, "", fmt.Errorf("list jobs: %w", err)
	}
	if f.Cursor != "" {
		qms, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", fmt.Errorf("list jobs: %w", err)
		}
		conds = append(conds, "(queued_at < ? OR (queued_at = ? AND id < ?))")
		args = append(args, qms, qms, id)
	}
	list, err := queryAll(ctx, s.r, "list jobs", scanRow[Job], "SELECT "+jobCols+" FROM jobs"+where(conds)+
		" ORDER BY queued_at DESC, id DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return nil, "", err
	}
	if len(list) <= limit {
		return list, "", nil
	}
	list = list[:limit]
	last := list[limit-1]
	return list, encodeCursor(ms(last.QueuedAt), last.ID), nil
}

// CountJobs counts the jobs that ListJobs returns for f over all pages. It
// ignores the cursor and the limit.
func (s *Store) CountJobs(ctx context.Context, f JobFilter) (int, error) {
	conds, args, err := jobWhere(f)
	if err != nil {
		return 0, fmt.Errorf("count jobs: %w", err)
	}
	return queryOne(ctx, s.r, "count jobs", scanValue[int], "SELECT count(*) FROM jobs"+where(conds), args...)
}

func jobWhere(f JobFilter) ([]string, []any, error) {
	var conds []string
	var args []any
	add := func(cond string, a ...any) {
		conds = append(conds, cond)
		args = append(args, a...)
	}
	switch f.Status {
	case "":
	case "running":
		add("state = 'running'")
	case "queued":
		add("state IN ('queued', 'assigned')")
	case ResultSucceeded, ResultFailed, ResultCancelled:
		add("state = 'completed' AND result = ?", f.Status)
	default:
		return nil, nil, fmt.Errorf("unknown status %q", f.Status)
	}
	if f.Repository != "" {
		add("repository = ?", f.Repository)
	}
	if f.PoolID != "" {
		add("pool_id = ?", f.PoolID)
	}
	if f.ServerID != "" {
		add("server_id = ?", f.ServerID)
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		q = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
		like := "%" + q + "%"
		add(`(job_name LIKE ? ESCAPE '\' OR workflow_name LIKE ? ESCAPE '\' OR repository LIKE ? ESCAPE '\'
			OR head_branch LIKE ? ESCAPE '\' OR head_sha LIKE ? ESCAPE '\')`, like, like, like, like, q+"%")
	}
	if !f.Since.IsZero() {
		add("queued_at >= ?", ms(f.Since))
	}
	if !f.Until.IsZero() {
		add("queued_at < ?", ms(f.Until))
	}
	return conds, args, nil
}

func (s *Store) JobFacets(ctx context.Context) (JobFacets, error) {
	const op = "list job facets"
	repos, err := queryAll(ctx, s.r, op, scanValue[string],
		"SELECT DISTINCT repository FROM jobs WHERE repository != '' ORDER BY repository")
	if err != nil {
		return JobFacets{}, err
	}
	pools, err := queryAll(ctx, s.r, op, scanFacet, `SELECT j.pool_id, coalesce(p.label, j.pool_id) FROM
		(SELECT DISTINCT pool_id FROM jobs WHERE pool_id != '') j LEFT JOIN pools p ON p.id = j.pool_id ORDER BY 2`)
	if err != nil {
		return JobFacets{}, err
	}
	servers, err := queryAll(ctx, s.r, op, scanFacet, `SELECT j.server_id, coalesce(v.name, j.server_id) FROM
		(SELECT DISTINCT server_id FROM jobs WHERE server_id != '') j LEFT JOIN servers v ON v.id = j.server_id ORDER BY 2`)
	if err != nil {
		return JobFacets{}, err
	}
	return JobFacets{Repositories: repos, Pools: pools, Servers: servers}, nil
}

func scanFacet(sc scanner) (Facet, error) {
	var x Facet
	err := sc.Scan(&x.ID, &x.Name)
	return x, err
}

// ExpectedDurationMS is the median run time of the last 20 succeeded runs of
// the job, or 0 if it has none.
func (s *Store) ExpectedDurationMS(ctx context.Context, repository, workflowFile, jobName string) (int64, error) {
	list, err := queryAll(ctx, s.r, "get expected duration", scanValue[int64], `SELECT finished_at - started_at FROM jobs
		WHERE repository = ? AND workflow_file = ? AND job_name = ? AND state = 'completed' AND result = 'succeeded'
			AND started_at IS NOT NULL AND finished_at IS NOT NULL
		ORDER BY finished_at DESC LIMIT 20`, repository, workflowFile, jobName)
	return median(list), err
}

// ListRunUsage returns the use of the last limit completed runs of a job
// that have metrics, newest first. The size comes from the sandbox of the
// run, or from its pool when the sandbox row is gone.
func (s *Store) ListRunUsage(ctx context.Context, repository, workflowFile, jobName string, limit int) ([]RunUsage, error) {
	return queryAll(ctx, s.r, "list run usage", func(sc scanner) (RunUsage, error) {
		var u RunUsage
		err := sc.Scan(&u.CPUPeak, &u.CPUAvg, &u.MemPeakMB, &u.VCPU, &u.MemoryMB)
		return u, err
	}, `SELECT j.cpu_peak, j.cpu_avg, j.mem_peak_mb,
			coalesce(nullif(b.vcpu, 0), p.vcpu, 0), coalesce(nullif(b.memory_mb, 0), p.memory_mb, 0)
		FROM jobs j LEFT JOIN sandboxes b ON b.id = j.sandbox_id LEFT JOIN pools p ON p.id = j.pool_id
		WHERE j.repository = ? AND j.workflow_file = ? AND j.job_name = ? AND j.state = 'completed'
			AND j.cpu_peak IS NOT NULL AND j.cpu_avg IS NOT NULL AND j.mem_peak_mb IS NOT NULL
		ORDER BY j.finished_at DESC LIMIT ?`, repository, workflowFile, jobName, limit)
}

// ListStaleRunningJobs returns the running jobs whose sandbox stopped before before.
func (s *Store) ListStaleRunningJobs(ctx context.Context, before time.Time) ([]Job, error) {
	return queryAll(ctx, s.r, "list stale jobs", scanRow[Job], "SELECT "+jobCols+` FROM jobs WHERE state = 'running'
		AND sandbox_id IN (SELECT id FROM sandboxes WHERE `+doneState+` AND stopped_at < ?)`, ms(before))
}

// ListUnfinishedJobs returns up to limit completed jobs, newest first, that
// finished after since and still have log_state live: their finish step did
// not end, for example because runplane stopped during it.
func (s *Store) ListUnfinishedJobs(ctx context.Context, since time.Time, limit int) ([]Job, error) {
	return queryAll(ctx, s.r, "list unfinished jobs", scanRow[Job], "SELECT "+jobCols+` FROM jobs
		WHERE state = 'completed' AND log_state = 'live' AND finished_at > ?
		ORDER BY finished_at DESC LIMIT ?`, ms(since), limit)
}

// ExpireQueuedJobs completes the jobs that are queued since before:
// runplane did not take them, so it guesses that they are cancelled at now.
// A job without a reject reason gets NotRunReason. It returns the changed
// jobs.
func (s *Store) ExpireQueuedJobs(ctx context.Context, before, now time.Time) ([]Job, error) {
	var list []Job
	err := s.tx(ctx, "expire queued jobs", func(tx *sql.Tx) error {
		var err error
		list, err = queryAll(ctx, tx, "list queued jobs", scanRow[Job],
			"SELECT "+jobCols+" FROM jobs WHERE state = 'queued' AND queued_at < ?", ms(before))
		if err != nil {
			return err
		}
		for i := range list {
			mergeJob(&list[i], JobMessage{State: JobCompleted, Result: ResultCancelled, Guessed: true}, now)
			list[i].RejectReason = cmp.Or(list[i].RejectReason, NotRunReason)
			if err := writeJob(ctx, tx, list[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return list, err
}

// DeleteJobsBefore deletes the jobs queued before before, and their
// samples. It returns the IDs of the deleted jobs, so the caller can remove
// their log files.
func (s *Store) DeleteJobsBefore(ctx context.Context, before time.Time) ([]string, error) {
	return queryAll(ctx, s.w, "delete old jobs", scanValue[string],
		"DELETE FROM jobs WHERE queued_at < ? RETURNING id", ms(before))
}

// ExpireJobLogs sets log_state to expired for the stored logs of the jobs
// that finished before before. It returns the IDs of these jobs, so the
// caller can remove their log files.
func (s *Store) ExpireJobLogs(ctx context.Context, before time.Time) ([]string, error) {
	return queryAll(ctx, s.w, "expire job logs", scanValue[string], `UPDATE jobs SET log_state = 'expired'
		WHERE log_state = 'stored' AND finished_at < ? RETURNING id`, ms(before))
}

// AppendJobMetrics stores samples. A sample with a known time replaces the earlier one.
func (s *Store) AppendJobMetrics(ctx context.Context, jobID string, samples []JobMetric) error {
	return s.tx(ctx, "append job metrics", func(tx *sql.Tx) error {
		for _, m := range samples {
			_, err := tx.ExecContext(ctx,
				"INSERT OR REPLACE INTO job_metrics (job_id, t_ms, cpu_percent, memory_mb) VALUES (?, ?, ?, ?)",
				jobID, ms(m.At), m.CPUPercent, m.MemoryMB)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// ListJobMetrics returns the samples of a job, oldest first.
func (s *Store) ListJobMetrics(ctx context.Context, jobID string) ([]JobMetric, error) {
	return queryAll(ctx, s.r, "list job metrics", func(sc scanner) (JobMetric, error) {
		var m JobMetric
		err := sc.Scan(at(&m.At), &m.CPUPercent, &m.MemoryMB)
		return m, err
	}, "SELECT t_ms, cpu_percent, memory_mb FROM job_metrics WHERE job_id = ? ORDER BY t_ms", jobID)
}

// JobMetricsSummary computes the peak and average of the samples of a job.
// It returns ErrNotFound if the job has no samples.
func (s *Store) JobMetricsSummary(ctx context.Context, jobID string) (MetricsSummary, error) {
	var peak, avg sql.NullFloat64
	var mem sql.NullInt64
	err := s.r.QueryRowContext(ctx,
		"SELECT max(cpu_percent), avg(cpu_percent), max(memory_mb) FROM job_metrics WHERE job_id = ?", jobID).
		Scan(&peak, &avg, &mem)
	if err != nil {
		return MetricsSummary{}, fmt.Errorf("summarize job metrics: %w", err)
	}
	if !peak.Valid {
		return MetricsSummary{}, fmt.Errorf("summarize job metrics: %w", ErrNotFound)
	}
	return MetricsSummary{CPUPeak: peak.Float64, CPUAvg: avg.Float64, MemPeakMB: int(mem.Int64)}, nil
}

// median sorts list in place.
func median(list []int64) int64 {
	if len(list) == 0 {
		return 0
	}
	slices.Sort(list)
	mid := len(list) / 2
	if len(list)%2 == 1 {
		return list[mid]
	}
	return (list[mid-1] + list[mid]) / 2
}
