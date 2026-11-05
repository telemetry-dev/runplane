package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
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

// Job log states.
const (
	LogNone    = "none"
	LogLive    = "live"   // the sandbox streams the log
	LogStored  = "stored" // the GitHub log is in the logs folder
	LogExpired = "expired"
)

var ErrBadCursor = errors.New("bad cursor")

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
	Result          string     `json:"result"` // empty until completed
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

// JobMetric is one CPU and memory sample of a job.
type JobMetric struct {
	At         time.Time `json:"at"`
	CPUPercent float64   `json:"cpu_percent"`
	MemoryMB   int       `json:"memory_mb"`
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
	commit_message, event, actor, pr_number, base_ref, labels_json, state, result, reject_reason, queued_at, assigned_at,
	started_at, finished_at, html_url, steps_json, log_state, cpu_peak, cpu_avg, mem_peak_mb, debug, enriched_at`

func scanJob(sc scanner) (Job, error) {
	var j Job
	var result sql.NullString
	err := sc.Scan(&j.ID, &j.RunnerRequestID, &j.GitHubJobID, &j.ScaleSetID, &j.PoolID, &j.SandboxID, &j.ServerID,
		&j.Repository, &j.WorkflowRef, &j.WorkflowFile, &j.WorkflowName, &j.JobName, &j.RunID, &j.RunNumber,
		&j.RunAttempt, &j.HeadBranch, &j.HeadSHA, &j.CommitMessage, &j.Event, &j.Actor, &j.PRNumber,
		&j.BaseRef, jsonCol(&j.Labels), &j.State, &result, &j.RejectReason, at(&j.QueuedAt), atPtr(&j.AssignedAt),
		atPtr(&j.StartedAt), atPtr(&j.FinishedAt), &j.HTMLURL, jsonCol(&j.Steps), &j.LogState, &j.CPUPeak,
		&j.CPUAvg, &j.MemPeakMB, &j.Debug, atPtr(&j.EnrichedAt))
	j.Result = result.String
	return j, err
}

// UpsertJob records a job message. It is safe to call again with the same
// message. The state only moves forward (queued, assigned, running,
// completed), and a completed job keeps its state, result, and finish time,
// except in the two cases that mergeJob describes. A message can still fill
// fields that the job does not have yet.
func (s *Store) UpsertJob(ctx context.Context, m JobMessage) (Job, error) {
	if stateRank(m.State) < 0 {
		return Job{}, fmt.Errorf("upsert job: unknown state %q", m.State)
	}
	var j Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		j, err = scanJob(tx.QueryRowContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE runner_request_id = ?", m.RunnerRequestID))
		if errors.Is(err, sql.ErrNoRows) {
			j = Job{ID: ids.New(), RunnerRequestID: m.RunnerRequestID, State: JobQueued, LogState: LogNone}
		} else if err != nil {
			return err
		}
		mergeJob(&j, m)
		args := jobArgs(j)
		_, err = tx.ExecContext(ctx, "INSERT INTO jobs ("+jobCols+") VALUES ("+placeholders(len(args))+
			") ON CONFLICT (id) DO UPDATE SET "+excludedSet(jobCols), args...)
		return err
	})
	if err != nil {
		return Job{}, fmt.Errorf("upsert job: %w", err)
	}
	return j, nil
}

func stateRank(state string) int {
	return slices.Index([]string{JobQueued, JobAssigned, JobRunning, JobCompleted}, state)
}

func mergeJob(j *Job, m JobMessage) {
	// A cancelled job that never ran can come back. ExpireQueuedJobs closes
	// the jobs that runplane did not take, whatever their reject reason, and
	// GitHub cancels an assignment that no runner takes in time and then
	// offers the job again (actions/scaleset, Job Reassignment). Such a job
	// opens again, and the scheduler checks it again.
	neverRan := j.Result == ResultCancelled && j.StartedAt == nil && j.SandboxID == ""
	if j.State == JobCompleted && neverRan && m.State != JobCompleted {
		j.State, j.Result, j.FinishedAt, j.RejectReason = JobQueued, "", nil, ""
		j.LogState = LogNone
	}
	// GitHub reports a job as completed one time. A second completion with
	// another result replaces a cancelled result: that came from the
	// scheduler, which guesses it for a job whose sandbox it lost.
	if j.State == JobCompleted && j.Result == ResultCancelled && m.State == JobCompleted && m.Result != "" && m.Result != ResultCancelled {
		j.Result = m.Result
		if !m.FinishedAt.IsZero() {
			t := m.FinishedAt.UTC()
			j.FinishedAt = &t
		}
	}
	fill := func(dst *string, v string) {
		if *dst == "" {
			*dst = v
		}
	}
	fillTime := func(dst **time.Time, v time.Time) {
		if *dst == nil && !v.IsZero() {
			t := v.UTC()
			*dst = &t
		}
	}
	fill(&j.ScaleSetID, m.ScaleSetID)
	fill(&j.PoolID, m.PoolID)
	fill(&j.Repository, m.Repository)
	fill(&j.WorkflowRef, m.WorkflowRef)
	fill(&j.WorkflowFile, workflowFile(m.WorkflowRef))
	fill(&j.JobName, m.JobName)
	fill(&j.Event, m.Event)
	if j.RunID == 0 {
		j.RunID = m.RunID
	}
	if len(j.Labels) == 0 {
		j.Labels = m.Labels
	}
	j.Debug = j.Debug || m.Debug
	if j.QueuedAt.IsZero() {
		j.QueuedAt = m.QueuedAt.UTC()
		if m.QueuedAt.IsZero() {
			j.QueuedAt = time.Now().UTC()
		}
	}
	fillTime(&j.AssignedAt, m.AssignedAt)
	fillTime(&j.StartedAt, m.StartedAt)

	if stateRank(m.State) <= stateRank(j.State) {
		return
	}
	j.State = m.State
	if m.State == JobCompleted {
		j.Result = m.Result
		finished := m.FinishedAt
		if finished.IsZero() {
			finished = time.Now()
		}
		fillTime(&j.FinishedAt, finished)
	}
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

func jobArgs(j Job) []any {
	return []any{j.ID, j.RunnerRequestID, j.GitHubJobID, j.ScaleSetID, j.PoolID, j.SandboxID, j.ServerID,
		j.Repository, j.WorkflowRef, j.WorkflowFile, j.WorkflowName, j.JobName, j.RunID, j.RunNumber,
		j.RunAttempt, j.HeadBranch, j.HeadSHA, j.CommitMessage, j.Event, j.Actor, j.PRNumber,
		j.BaseRef, jsonArg(j.Labels), j.State, nullStr(j.Result), j.RejectReason, ms(j.QueuedAt), msPtr(j.AssignedAt),
		msPtr(j.StartedAt), msPtr(j.FinishedAt), j.HTMLURL, jsonArg(j.Steps), j.LogState, j.CPUPeak,
		j.CPUAvg, j.MemPeakMB, j.Debug, msPtr(j.EnrichedAt)}
}

// excludedSet returns "b = excluded.b, c = excluded.c" for the columns "a, b, c".
func excludedSet(cols string) string {
	var set []string
	for _, c := range strings.Split(cols, ",")[1:] {
		c = strings.TrimSpace(c)
		set = append(set, c+" = excluded."+c)
	}
	return strings.Join(set, ", ")
}

func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	j, err := scanJob(s.r.QueryRowContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE id = ?", id))
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", dbErr(err))
	}
	return j, nil
}

func (s *Store) GetJobByRunnerRequestID(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(s.r.QueryRowContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE runner_request_id = ?", id))
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", dbErr(err))
	}
	return j, nil
}

// SetJobSandbox records the sandbox and the server that run the job.
func (s *Store) SetJobSandbox(ctx context.Context, id, sandboxID, serverID string) error {
	err := exec1(ctx, s.w, "UPDATE jobs SET sandbox_id = ?, server_id = ? WHERE id = ?", sandboxID, serverID, id)
	if err != nil {
		return fmt.Errorf("set job sandbox: %w", err)
	}
	return nil
}

// SetJobRunRef writes the run data that gives the cache refs. An empty
// event keeps the event from the job message.
func (s *Store) SetJobRunRef(ctx context.Context, id string, r RunRef) error {
	err := exec1(ctx, s.w, `UPDATE jobs SET event = coalesce(nullif(?, ''), event), head_branch = ?, pr_number = ?,
			base_ref = ?
		WHERE id = ?`, r.Event, r.HeadBranch, r.PRNumber, r.BaseRef, id)
	if err != nil {
		return fmt.Errorf("set job run ref: %w", err)
	}
	return nil
}

// SetJobEnrichment writes the run data and sets enriched_at. An empty event
// keeps the event from the job message.
func (s *Store) SetJobEnrichment(ctx context.Context, id string, e JobEnrichment) error {
	err := exec1(ctx, s.w, `UPDATE jobs SET github_job_id = ?, workflow_name = ?, run_number = ?, run_attempt = ?,
			head_branch = ?, head_sha = ?, commit_message = ?, event = coalesce(nullif(?, ''), event), actor = ?,
			pr_number = ?, base_ref = ?, html_url = ?, steps_json = ?, enriched_at = ?,
			queued_at = CASE WHEN ? > 0 AND ? < queued_at THEN ? ELSE queued_at END
		WHERE id = ?`,
		e.GitHubJobID, e.WorkflowName, e.RunNumber, e.RunAttempt, e.HeadBranch, e.HeadSHA, e.CommitMessage, e.Event,
		e.Actor, e.PRNumber, e.BaseRef, e.HTMLURL, jsonArg(e.Steps), ms(time.Now()),
		ms(e.QueuedAt), ms(e.QueuedAt), ms(e.QueuedAt), id)
	if err != nil {
		return fmt.Errorf("set job enrichment: %w", err)
	}
	return nil
}

// SetJobSteps writes the steps of a running job.
func (s *Store) SetJobSteps(ctx context.Context, id string, steps []JobStep) error {
	if err := exec1(ctx, s.w, "UPDATE jobs SET steps_json = ? WHERE id = ?", jsonArg(steps), id); err != nil {
		return fmt.Errorf("set job steps: %w", err)
	}
	return nil
}

func (s *Store) SetJobMetricsSummary(ctx context.Context, id string, m MetricsSummary) error {
	err := exec1(ctx, s.w, "UPDATE jobs SET cpu_peak = ?, cpu_avg = ?, mem_peak_mb = ? WHERE id = ?",
		m.CPUPeak, m.CPUAvg, m.MemPeakMB, id)
	if err != nil {
		return fmt.Errorf("set job metrics: %w", err)
	}
	return nil
}

func (s *Store) SetJobLogState(ctx context.Context, id, state string) error {
	if err := exec1(ctx, s.w, "UPDATE jobs SET log_state = ? WHERE id = ?", state, id); err != nil {
		return fmt.Errorf("set job log state: %w", err)
	}
	return nil
}

func (s *Store) SetJobRejectReason(ctx context.Context, id, reason string) error {
	if err := exec1(ctx, s.w, "UPDATE jobs SET reject_reason = ? WHERE id = ?", reason, id); err != nil {
		return fmt.Errorf("set job reject reason: %w", err)
	}
	return nil
}

// ListJobs returns one page of jobs, newest first by queued_at and then by
// ID, and the cursor of the next page ("" on the last page).
func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]Job, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)

	where, args, err := jobWhere(f)
	if err != nil {
		return nil, "", fmt.Errorf("list jobs: %w", err)
	}
	if f.Cursor != "" {
		qms, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", fmt.Errorf("list jobs: %w", err)
		}
		where = append(where, "(queued_at < ? OR (queued_at = ? AND id < ?))")
		args = append(args, qms, qms, id)
	}

	query := "SELECT " + jobCols + " FROM jobs"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY queued_at DESC, id DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.r.QueryContext(ctx, query, args...)
	list, err := scanAll(rows, err, scanJob)
	if err != nil {
		return nil, "", fmt.Errorf("list jobs: %w", err)
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
	where, args, err := jobWhere(f)
	if err != nil {
		return 0, fmt.Errorf("count jobs: %w", err)
	}
	query := "SELECT count(*) FROM jobs"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	var n int
	if err := s.r.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count jobs: %w", err)
	}
	return n, nil
}

// jobWhere returns the conditions of f without the cursor, for ListJobs
// and CountJobs.
func jobWhere(f JobFilter) ([]string, []any, error) {
	var where []string
	var args []any
	add := func(cond string, a ...any) {
		where = append(where, cond)
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
		like := "%" + escapeLike(q) + "%"
		add(`(job_name LIKE ? ESCAPE '\' OR workflow_name LIKE ? ESCAPE '\' OR repository LIKE ? ESCAPE '\'
			OR head_branch LIKE ? ESCAPE '\' OR head_sha LIKE ? ESCAPE '\')`, like, like, like, like, escapeLike(q)+"%")
	}
	if !f.Since.IsZero() {
		add("queued_at >= ?", ms(f.Since))
	}
	if !f.Until.IsZero() {
		add("queued_at < ?", ms(f.Until))
	}
	return where, args, nil
}

func encodeCursor(queuedMS int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(queuedMS, 10) + "." + id))
}

func decodeCursor(c string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", ErrBadCursor
	}
	n, id, ok := strings.Cut(string(b), ".")
	qms, err := strconv.ParseInt(n, 10, 64)
	if !ok || err != nil || id == "" {
		return 0, "", ErrBadCursor
	}
	return qms, id, nil
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Store) JobFacets(ctx context.Context) (JobFacets, error) {
	var f JobFacets
	rows, err := s.r.QueryContext(ctx, "SELECT DISTINCT repository FROM jobs WHERE repository != '' ORDER BY repository")
	if f.Repositories, err = scanAll(rows, err, scanString); err != nil {
		return JobFacets{}, fmt.Errorf("list job facets: %w", err)
	}
	scanFacet := func(sc scanner) (Facet, error) {
		var x Facet
		return x, sc.Scan(&x.ID, &x.Name)
	}
	rows, err = s.r.QueryContext(ctx, `SELECT j.pool_id, coalesce(p.label, j.pool_id) FROM
		(SELECT DISTINCT pool_id FROM jobs WHERE pool_id != '') j LEFT JOIN pools p ON p.id = j.pool_id ORDER BY 2`)
	if f.Pools, err = scanAll(rows, err, scanFacet); err != nil {
		return JobFacets{}, fmt.Errorf("list job facets: %w", err)
	}
	rows, err = s.r.QueryContext(ctx, `SELECT j.server_id, coalesce(v.name, j.server_id) FROM
		(SELECT DISTINCT server_id FROM jobs WHERE server_id != '') j LEFT JOIN servers v ON v.id = j.server_id ORDER BY 2`)
	if f.Servers, err = scanAll(rows, err, scanFacet); err != nil {
		return JobFacets{}, fmt.Errorf("list job facets: %w", err)
	}
	return f, nil
}

// ExpectedDurationMS is the median run time of the last 20 succeeded runs of
// the job, or 0 if it has none.
func (s *Store) ExpectedDurationMS(ctx context.Context, repository, workflowFile, jobName string) (int64, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT finished_at - started_at FROM jobs
		WHERE repository = ? AND workflow_file = ? AND job_name = ? AND state = 'completed' AND result = 'succeeded'
			AND started_at IS NOT NULL AND finished_at IS NOT NULL
		ORDER BY finished_at DESC LIMIT 20`, repository, workflowFile, jobName)
	list, err := scanAll(rows, err, scanInt64)
	if err != nil {
		return 0, fmt.Errorf("get expected duration: %w", err)
	}
	return median(list), nil
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

// ListRunUsage returns the use of the last limit completed runs of a job
// that have metrics, newest first. The size comes from the sandbox of the
// run, or from its pool when the sandbox row is gone.
func (s *Store) ListRunUsage(ctx context.Context, repository, workflowFile, jobName string, limit int) ([]RunUsage, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT j.cpu_peak, j.cpu_avg, j.mem_peak_mb,
			coalesce(nullif(b.vcpu, 0), p.vcpu, 0), coalesce(nullif(b.memory_mb, 0), p.memory_mb, 0)
		FROM jobs j LEFT JOIN sandboxes b ON b.id = j.sandbox_id LEFT JOIN pools p ON p.id = j.pool_id
		WHERE j.repository = ? AND j.workflow_file = ? AND j.job_name = ? AND j.state = 'completed'
			AND j.cpu_peak IS NOT NULL AND j.cpu_avg IS NOT NULL AND j.mem_peak_mb IS NOT NULL
		ORDER BY j.finished_at DESC LIMIT ?`, repository, workflowFile, jobName, limit)
	list, err := scanAll(rows, err, func(sc scanner) (RunUsage, error) {
		var u RunUsage
		return u, sc.Scan(&u.CPUPeak, &u.CPUAvg, &u.MemPeakMB, &u.VCPU, &u.MemoryMB)
	})
	if err != nil {
		return nil, fmt.Errorf("list run usage: %w", err)
	}
	return list, nil
}

// ListStaleRunningJobs returns the running jobs whose sandbox stopped before before.
func (s *Store) ListStaleRunningJobs(ctx context.Context, before time.Time) ([]Job, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+prefixCols("j", jobCols)+` FROM jobs j
		JOIN sandboxes b ON b.id = j.sandbox_id
		WHERE j.state = 'running' AND b.state IN ('gone', 'failed') AND b.stopped_at < ?`, ms(before))
	list, err := scanAll(rows, err, scanJob)
	if err != nil {
		return nil, fmt.Errorf("list stale jobs: %w", err)
	}
	return list, nil
}

// ListUnfinishedJobs returns up to limit completed jobs, newest first, that
// finished after since and still have log_state live: their finish step did
// not end, for example because runplane stopped during it.
func (s *Store) ListUnfinishedJobs(ctx context.Context, since time.Time, limit int) ([]Job, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs
		WHERE state = 'completed' AND log_state = 'live' AND finished_at > ?
		ORDER BY finished_at DESC LIMIT ?`, ms(since), limit)
	list, err := scanAll(rows, err, scanJob)
	if err != nil {
		return nil, fmt.Errorf("list unfinished jobs: %w", err)
	}
	return list, nil
}

// AppendJobMetrics stores samples. A sample with a known time replaces the earlier one.
func (s *Store) AppendJobMetrics(ctx context.Context, jobID string, samples []JobMetric) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
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
	if err != nil {
		return fmt.Errorf("append job metrics: %w", err)
	}
	return nil
}

// ListJobMetrics returns the samples of a job, oldest first.
func (s *Store) ListJobMetrics(ctx context.Context, jobID string) ([]JobMetric, error) {
	rows, err := s.r.QueryContext(ctx,
		"SELECT t_ms, cpu_percent, memory_mb FROM job_metrics WHERE job_id = ? ORDER BY t_ms", jobID)
	list, err := scanAll(rows, err, func(sc scanner) (JobMetric, error) {
		var m JobMetric
		return m, sc.Scan(at(&m.At), &m.CPUPercent, &m.MemoryMB)
	})
	if err != nil {
		return nil, fmt.Errorf("list job metrics: %w", err)
	}
	return list, nil
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

func scanInt64(sc scanner) (int64, error) {
	var n int64
	return n, sc.Scan(&n)
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

// prefixCols returns "p.a, p.b" for the columns "a, b".
func prefixCols(p, cols string) string {
	list := strings.Split(cols, ",")
	for i, c := range list {
		list[i] = p + "." + strings.TrimSpace(c)
	}
	return strings.Join(list, ", ")
}
