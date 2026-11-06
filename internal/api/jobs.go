package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/ghrest"
	"github.com/runplane/runplane/internal/store"
)

// JobSummary is the list shape of a job, also sent in job.state events.
type JobSummary struct {
	ID           string     `json:"id"`
	JobName      string     `json:"job_name"`
	WorkflowName string     `json:"workflow_name" doc:"Empty until runplane reads the run from GitHub."`
	WorkflowFile string     `json:"workflow_file" doc:"For example \".github/workflows/ci.yml\"."`
	Repository   string     `json:"repository" doc:"owner/name"`
	RunNumber    int64      `json:"run_number" doc:"0 until runplane reads the run from GitHub."`
	HeadBranch   string     `json:"head_branch"`
	HeadSHA      string     `json:"head_sha"`
	PoolID       string     `json:"pool_id"`
	PoolLabel    string     `json:"pool_label" doc:"Empty if the pool is deleted."`
	ServerID     string     `json:"server_id" doc:"Empty until the job has a sandbox."`
	ServerName   string     `json:"server_name"`
	State        string     `json:"state" enum:"queued,assigned,running,completed"`
	Result       *string    `json:"result" enum:"succeeded,failed,cancelled,skipped" doc:"Null until the job completes."`
	Debug        bool       `json:"debug" doc:"The sandbox stays after the job for a debug shell."`
	RejectReason string     `json:"reject_reason" doc:"Why runplane does not take this queued job. Empty if it takes it."`
	QueueMS      int64      `json:"queue_ms" doc:"Time from queued to assigned. For a job that still waits, the time until now."`
	DurationMS   int64      `json:"duration_ms" doc:"Time from start to finish. For a running job, the time until now."`
	QueuedAt     time.Time  `json:"queued_at"`
	StartedAt    *time.Time `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`

	ExpectedDurationMS int64 `json:"expected_duration_ms" doc:"Median run time of the last 20 succeeded runs of this job, 0 if none. Lists and events set it only for jobs that did not complete."`
}

type Job struct {
	JobSummary
	WorkflowRef   string     `json:"workflow_ref" doc:"For example \"owner/name/.github/workflows/ci.yml@refs/heads/main\"."`
	RunID         int64      `json:"run_id"`
	RunAttempt    int        `json:"run_attempt"`
	CommitMessage string     `json:"commit_message"`
	Event         string     `json:"event" doc:"GitHub event, for example \"push\" or \"pull_request\"."`
	Actor         string     `json:"actor"`
	PRNumber      int        `json:"pr_number" doc:"0 if the run is not for a pull request."`
	HTMLURL       string     `json:"html_url" doc:"The job on GitHub, or the page of its run until runplane reads the job. Empty without a run ID."`
	Steps         []JobStep  `json:"steps"`
	Labels        []string   `json:"labels" doc:"The runs-on labels."`
	SandboxID     string     `json:"sandbox_id"`
	SandboxState  string     `json:"sandbox_state" enum:"starting,idle,busy,stopping,gone,failed," doc:"Empty until the job has a sandbox."`
	HoldUntil     *time.Time `json:"hold_until" doc:"A debug sandbox stays until this time after the job."`
	SSHCommand    string     `json:"ssh_command" doc:"Command that opens the debug shell over SSH, for example \"ssh -p 2222 <sandbox-id>@ci.example.com\". Empty unless the sandbox is a debug sandbox that has not stopped."`
	LogState      string     `json:"log_state" enum:"none,live,stored,expired"`
	CPUPeak       *float64   `json:"cpu_peak" doc:"Percent of the vCPU allocation."`
	CPUAvg        *float64   `json:"cpu_avg" doc:"Percent of the vCPU allocation."`
	MemPeakMB     *int       `json:"mem_peak_mb"`
	VCPU          int        `json:"vcpu" doc:"Size of the sandbox, or of the pool before the job has a sandbox."`
	MemoryMB      int        `json:"memory_mb"`
	AssignedAt    *time.Time `json:"assigned_at"`
	SizeAdvice    SizeAdvice `json:"size_advice" doc:"Whether the pool fits the CPU and memory use of the last 10 completed runs of this job (same repository, workflow file, and job name)."`
}

// JobStep is one step from the GitHub jobs API.
type JobStep struct {
	Number      int        `json:"number"`
	Name        string     `json:"name"`
	Status      string     `json:"status" doc:"queued, in_progress, or completed."`
	Conclusion  string     `json:"conclusion" doc:"For example success, failure, cancelled, or skipped. Empty until the step completes."`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

type JobPage struct {
	Items      []JobSummary `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty" doc:"Cursor of the next page. Absent on the last page."`
	Total      int          `json:"total" doc:"Jobs that match the filters, on all pages."`
}

func jobSummary(j store.Job, r refs, now time.Time) JobSummary {
	var result *string
	if j.Result != "" {
		result = &j.Result
	}
	return JobSummary{
		ID: j.ID, JobName: j.JobName, WorkflowName: j.WorkflowName, WorkflowFile: j.WorkflowFile,
		Repository: j.Repository, RunNumber: j.RunNumber, HeadBranch: j.HeadBranch, HeadSHA: j.HeadSHA,
		PoolID: j.PoolID, PoolLabel: r.pools[j.PoolID].Label, ServerID: j.ServerID, ServerName: r.servers[j.ServerID].Name,
		State: j.State, Result: result, Debug: j.Debug, RejectReason: j.RejectReason,
		QueueMS: queueMS(j, now), DurationMS: durationMS(j, now),
		QueuedAt: j.QueuedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
	}
}

func queueMS(j store.Job, now time.Time) int64 {
	end := now
	switch {
	case j.AssignedAt != nil:
		end = *j.AssignedAt
	case j.StartedAt != nil:
		end = *j.StartedAt
	case j.FinishedAt != nil: // cancelled while queued
		end = *j.FinishedAt
	}
	return max(0, end.Sub(j.QueuedAt).Milliseconds())
}

func durationMS(j store.Job, now time.Time) int64 {
	if j.StartedAt == nil {
		return 0
	}
	end := now
	if j.FinishedAt != nil {
		end = *j.FinishedAt
	}
	return max(0, end.Sub(*j.StartedAt).Milliseconds())
}

// jobSummaries converts jobs and sets the expected duration of the jobs
// that did not complete.
func (h *Handler) jobSummaries(ctx context.Context, jobs []store.Job) ([]JobSummary, error) {
	r, err := h.loadRefs(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	type key struct{ repo, file, name string }
	expected := map[key]int64{}
	list := make([]JobSummary, len(jobs))
	for i, j := range jobs {
		list[i] = jobSummary(j, r, now)
		if j.State == store.JobCompleted {
			continue
		}
		k := key{j.Repository, j.WorkflowFile, j.JobName}
		ms, ok := expected[k]
		if !ok {
			if ms, err = h.d.Store.ExpectedDurationMS(ctx, k.repo, k.file, k.name); err != nil {
				return nil, err
			}
			expected[k] = ms
		}
		list[i].ExpectedDurationMS = ms
	}
	return list, nil
}

type listJobsInput struct {
	Status     string `query:"status" enum:"running,queued,succeeded,failed,cancelled" doc:"queued also has the assigned jobs."`
	Repository string `query:"repository" maxLength:"200"`
	PoolID     string `query:"pool_id" maxLength:"100"`
	ServerID   string `query:"server_id" maxLength:"100"`
	Q          string `query:"q" maxLength:"200" doc:"Text in the job, workflow, repository, or branch name, or the start of a commit SHA."`
	Range      string `query:"range" enum:"24h,7d,30d" doc:"Only the jobs queued in this time. All jobs if absent."`
	Cursor     string `query:"cursor" maxLength:"200" doc:"next_cursor of the previous page."`
	Limit      int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}

// ranges are the time ranges of the job list and the overview, in buckets
// of the overview chart.
var ranges = map[string]struct {
	bucket time.Duration
	count  int
}{
	"24h": {time.Hour, 24},
	"7d":  {6 * time.Hour, 28},
	"30d": {24 * time.Hour, 30},
}

func (h *Handler) listJobs(ctx context.Context, in *listJobsInput) (*body[JobPage], error) {
	f := store.JobFilter{
		Status: in.Status, Repository: in.Repository, PoolID: in.PoolID, ServerID: in.ServerID,
		Q: in.Q, Cursor: in.Cursor, Limit: in.Limit,
	}
	if rg, ok := ranges[in.Range]; ok {
		f.Since = time.Now().Add(-time.Duration(rg.count) * rg.bucket)
	}
	jobs, next, err := h.d.Store.ListJobs(ctx, f)
	if errors.Is(err, store.ErrBadCursor) {
		return nil, invalid("query.cursor", "The cursor is not valid.", in.Cursor)
	}
	if err != nil {
		return nil, err
	}
	total, err := h.d.Store.CountJobs(ctx, f)
	if err != nil {
		return nil, err
	}
	items, err := h.jobSummaries(ctx, jobs)
	if err != nil {
		return nil, err
	}
	return &body[JobPage]{JobPage{Items: items, NextCursor: next, Total: total}}, nil
}

type Facet struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type JobFacets struct {
	Repositories []string `json:"repositories"`
	Pools        []Facet  `json:"pools" doc:"The name is the pool label."`
	Servers      []Facet  `json:"servers"`
}

func (h *Handler) getJobFacets(ctx context.Context, _ *struct{}) (*body[JobFacets], error) {
	f, err := h.d.Store.JobFacets(ctx)
	if err != nil {
		return nil, err
	}
	facets := func(list []store.Facet) []Facet {
		out := make([]Facet, len(list))
		for i, x := range list {
			out[i] = Facet(x)
		}
		return out
	}
	return &body[JobFacets]{JobFacets{
		Repositories: orEmpty(f.Repositories), Pools: facets(f.Pools), Servers: facets(f.Servers),
	}}, nil
}

func (h *Handler) getJob(ctx context.Context, in *PathID) (*body[Job], error) {
	j, err := h.d.Store.GetJob(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No job has this ID.")
	}
	r, err := h.loadRefs(ctx)
	if err != nil {
		return nil, err
	}
	out := Job{
		JobSummary:  jobSummary(j, r, time.Now()),
		WorkflowRef: j.WorkflowRef, RunID: j.RunID, RunAttempt: j.RunAttempt, CommitMessage: j.CommitMessage,
		Event: j.Event, Actor: j.Actor, PRNumber: j.PRNumber, HTMLURL: jobURL(j),
		Steps: make([]JobStep, len(j.Steps)), Labels: orEmpty(j.Labels), SandboxID: j.SandboxID,
		LogState: j.LogState, CPUPeak: j.CPUPeak, CPUAvg: j.CPUAvg, MemPeakMB: j.MemPeakMB, AssignedAt: j.AssignedAt,
	}
	for i, st := range j.Steps {
		out.Steps[i] = JobStep(st)
	}
	if out.ExpectedDurationMS, err = h.d.Store.ExpectedDurationMS(ctx, j.Repository, j.WorkflowFile, j.JobName); err != nil {
		return nil, err
	}
	runs, err := h.d.Store.ListRunUsage(ctx, j.Repository, j.WorkflowFile, j.JobName, adviceRuns)
	if err != nil {
		return nil, err
	}
	var pool *store.Pool
	if p, ok := r.pools[j.PoolID]; ok {
		pool = &p
	}
	out.SizeAdvice = sizeAdvice(runs, pool, slices.Collect(maps.Values(r.pools)))
	b, err := h.jobSandbox(ctx, j)
	if err != nil {
		return nil, err
	}
	out.VCPU, out.MemoryMB = jobSize(j, b, r)
	if b != nil {
		out.SandboxState, out.HoldUntil = string(b.State), b.HoldUntil
		if b.Debug && !b.State.Done() {
			if out.SSHCommand, err = h.sshCommand(ctx, b.ID); err != nil {
				return nil, err
			}
		}
	}
	return &body[Job]{out}, nil
}

// jobURL is the page of the job on GitHub. Before the enrichment reads the
// job, it is the page of the run.
func jobURL(j store.Job) string {
	if j.HTMLURL != "" || j.RunID == 0 || j.Repository == "" {
		return j.HTMLURL
	}
	u := fmt.Sprintf("%s/%s/actions/runs/%d", ghrest.Web, j.Repository, j.RunID)
	if j.RunAttempt > 1 {
		u += fmt.Sprintf("/attempts/%d", j.RunAttempt)
	}
	return u
}

// jobSandbox returns the sandbox of the job, or nil.
func (h *Handler) jobSandbox(ctx context.Context, j store.Job) (*store.Sandbox, error) {
	if j.SandboxID == "" {
		return nil, nil
	}
	b, err := h.d.Store.GetSandbox(ctx, j.SandboxID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func jobSize(j store.Job, b *store.Sandbox, r refs) (vcpu, memoryMB int) {
	if b != nil {
		return b.VCPU, b.MemoryMB
	}
	p := r.pools[j.PoolID]
	return p.VCPU, p.MemoryMB
}

type JobMetrics struct {
	VCPU     int            `json:"vcpu"`
	MemoryMB int            `json:"memory_mb"`
	Samples  []MetricSample `json:"samples" doc:"One sample each 5 seconds while the job runs, oldest first."`
}

type MetricSample struct {
	TMS        int64   `json:"t_ms" doc:"Time of the sample in unix milliseconds."`
	CPUPercent float64 `json:"cpu_percent" doc:"Percent of the vCPU allocation, 0 to 100."`
	MemoryMB   int     `json:"memory_mb"`
}

func (h *Handler) getJobMetrics(ctx context.Context, in *PathID) (*body[JobMetrics], error) {
	j, err := h.d.Store.GetJob(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No job has this ID.")
	}
	samples, err := h.d.Store.ListJobMetrics(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	r, err := h.loadRefs(ctx)
	if err != nil {
		return nil, err
	}
	b, err := h.jobSandbox(ctx, j)
	if err != nil {
		return nil, err
	}
	out := JobMetrics{Samples: make([]MetricSample, len(samples))}
	out.VCPU, out.MemoryMB = jobSize(j, b, r)
	for i, m := range samples {
		out.Samples[i] = MetricSample{TMS: m.At.UnixMilli(), CPUPercent: m.CPUPercent, MemoryMB: m.MemoryMB}
	}
	return &body[JobMetrics]{out}, nil
}

type RerunRequest struct {
	Debug bool `json:"debug,omitempty" doc:"Keep the sandbox of the new attempt after the job for a debug shell."`
}

func (h *Handler) rerunJob(ctx context.Context, in *struct {
	PathID
	Body RerunRequest
}) (*empty, error) {
	j, err := h.d.Store.GetJob(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No job has this ID.")
	}
	if j.State != store.JobCompleted {
		return nil, huma.Error409Conflict("Only a completed job can run again.")
	}
	if err := h.d.Scheduler.RerunJob(ctx, j.ID, in.Body.Debug); err != nil {
		return nil, serviceErr(err)
	}
	h.record(ctx, audit.JobRerun, j.ID, jobDetail(map[string]any{"debug": in.Body.Debug}, j))
	return nil, nil
}

type logInput struct {
	PathID
	Step   int  `query:"step" minimum:"0" doc:"Only this step of a stored log. 0 or absent: all steps. The log of a running job always has all steps; then the response has the header X-Runplane-Step-Filter: ignored."`
	Follow bool `query:"follow" doc:"Stream new lines until the log ends."`
}

func (h *Handler) jobLog(ctx context.Context, in *logInput) (*huma.StreamResponse, error) {
	if _, err := h.d.Store.GetJob(ctx, in.ID); err != nil {
		return nil, notFound(err, "No job has this ID.")
	}
	rc, live, err := h.d.Logs.Job(ctx, in.ID, in.Step, in.Follow)
	if err != nil {
		return nil, h.logErr(err)
	}
	resp := h.stream(rc, "text/plain; charset=utf-8", in.Follow)
	if live && in.Step > 0 {
		send := resp.Body
		resp.Body = func(hctx huma.Context) {
			hctx.SetHeader(stepFilterHeader, stepFilterIgnored)
			send(hctx)
		}
	}
	return resp, nil
}

// stepFilterHeader with the value stepFilterIgnored tells that the job log
// has all steps although the request asked for one: the live log of a
// running job has no step marks.
const (
	stepFilterHeader  = "X-Runplane-Step-Filter"
	stepFilterIgnored = "ignored"
)

type Overview struct {
	Range    string           `json:"range" enum:"24h,7d,30d"`
	BucketMS int64            `json:"bucket_ms" doc:"Length of each bucket: 1 hour for 24h, 6 hours for 7d, 1 day for 30d."`
	Stats    OverviewStats    `json:"stats"`
	Running  []JobSummary     `json:"running" doc:"All running jobs, newest first."`
	Servers  []ServerSummary  `json:"servers" doc:"The primary first."`
	Buckets  []OverviewBucket `json:"buckets" doc:"Completed jobs by queue time, oldest first, in UTC: 24 buckets of 1 hour for 24h, 28 of 6 hours for 7d, 30 of 1 day for 30d. The last bucket ends after now."`
	Recent   []JobSummary     `json:"recent" doc:"The last 10 completed jobs, newest first."`
}

// OverviewStats cover the jobs queued in the time of the buckets.
type OverviewStats struct {
	Jobs          int      `json:"jobs"`
	VCPUInUse     int      `json:"vcpu_in_use" doc:"vCPUs of the live sandboxes on the online and draining servers."`
	VCPUTotal     int      `json:"vcpu_total" doc:"vCPUs of the online and draining servers."`
	MedianQueueMS *int64   `json:"median_queue_ms" doc:"Median time from queued to assigned. Null when no job of the range has a runner yet."`
	FailureRate   *float64 `json:"failure_rate" doc:"failed / (succeeded + failed), 0 to 1. Null when no job of the range succeeded or failed."`
	CacheHitRate  *float64 `json:"cache_hit_rate" doc:"Actions cache hits / (hits + misses), 0 to 1. Null when the range has no cache lookups."`
	DurationMS    int64    `json:"duration_ms" doc:"Sum of the run times of the completed jobs."`
}

type OverviewBucket struct {
	Start     time.Time `json:"start"`
	Succeeded int       `json:"succeeded"`
	Failed    int       `json:"failed"`
	Cancelled int       `json:"cancelled"`
}

type overviewInput struct {
	Range string `query:"range" enum:"24h,7d,30d" default:"24h"`
}

func (h *Handler) getOverview(ctx context.Context, in *overviewInput) (*body[Overview], error) {
	rg := ranges[in.Range]
	// The buckets start on whole hours, quarter days, or days in UTC, and
	// the last bucket has now.
	since := time.Now().UTC().Truncate(rg.bucket).Add(-time.Duration(rg.count-1) * rg.bucket)
	until := since.Add(time.Duration(rg.count) * rg.bucket)
	o, err := h.d.Store.Overview(ctx, since, until, rg.bucket)
	if err != nil {
		return nil, err
	}
	out := Overview{
		Range: in.Range, BucketMS: rg.bucket.Milliseconds(),
		Stats: OverviewStats{
			Jobs: o.Jobs, MedianQueueMS: o.MedianQueueMS, FailureRate: o.FailureRate, CacheHitRate: o.CacheHitRate,
			DurationMS: o.DurationMS,
		},
		Buckets: make([]OverviewBucket, len(o.Buckets)),
	}
	for i, x := range o.Buckets {
		out.Buckets[i] = OverviewBucket(x)
	}
	if out.Running, err = h.jobSummaries(ctx, o.Running); err != nil {
		return nil, err
	}
	if out.Recent, err = h.jobSummaries(ctx, o.Recent); err != nil {
		return nil, err
	}
	list, err := h.d.Store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	servers, err := h.servers(ctx, list)
	if err != nil {
		return nil, err
	}
	out.Servers = make([]ServerSummary, len(servers))
	for i, v := range servers {
		out.Servers[i] = v.ServerSummary
		// An unreachable server keeps its sandbox rows until it connects
		// again, so its sandboxes count only with its vCPUs.
		if v.Status == store.ServerOnline || v.Status == store.ServerDraining {
			out.Stats.VCPUInUse += v.VCPUInUse
			out.Stats.VCPUTotal += v.VCPU
		}
	}
	return &body[Overview]{out}, nil
}
