package scheduler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// scaleSet is the listener.Scaler of one scale set.
type scaleSet struct {
	s      *Scheduler
	set    store.ScaleSet
	client *guardedClient

	// mu makes Scale and resize run one at a time.
	mu    sync.Mutex
	stats store.ScaleSetStats // of the last message with statistics
}

type rerunKey struct {
	repository string
	runID      int64
	jobName    string
}

type checkKey struct {
	repository string
	runID      int64
}

type checkEntry struct {
	JobCheck
	err error // ErrRunUnreadable or nil
	at  time.Time
}

// Attach gives the scale set to the scheduler and returns the Scaler for its
// listener. A second Attach with the same scale set ID replaces the first,
// as Detach does.
func (s *Scheduler) Attach(set store.ScaleSet, c ScaleSetClient) listener.Scaler {
	h := &scaleSet{s: s, set: set, client: &guardedClient{c: c}, stats: set.Stats}
	s.mu.Lock()
	old := s.sets[set.ID]
	s.sets[set.ID] = h
	s.mu.Unlock()
	if old != nil {
		old.client.detach()
	}
	return h
}

// Detach drops the scale set when its listener stops. It returns after the
// calls to its client in progress, and the scheduler makes no more calls.
// The sandboxes stay; the cleanup stops the idle ones if the scale set or
// its pool is deleted.
func (s *Scheduler) Detach(scaleSetID string) {
	s.mu.Lock()
	h := s.sets[scaleSetID]
	delete(s.sets, scaleSetID)
	s.mu.Unlock()
	if h != nil {
		h.client.detach()
	}
}

// errDetached is the error of a client call after Detach.
var errDetached = errors.New("the scale set has no listener")

// guardedClient passes calls to the client of a scale set until detach.
type guardedClient struct {
	mu       sync.RWMutex
	c        ScaleSetClient
	detached bool
}

// detach waits for the calls in progress. Later calls do nothing.
func (g *guardedClient) detach() {
	g.mu.Lock()
	g.detached = true
	g.mu.Unlock()
}

func (g *guardedClient) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.detached {
		return nil, errDetached
	}
	return g.c.AcquireJobs(ctx, requestIDs)
}

func (g *guardedClient) GenerateJITConfig(ctx context.Context, runnerName string) (JITConfig, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.detached {
		return JITConfig{}, errDetached
	}
	return g.c.GenerateJITConfig(ctx, runnerName)
}

func (g *guardedClient) RemoveRunner(ctx context.Context, runnerID int64) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.detached {
		return errDetached
	}
	return g.c.RemoveRunner(ctx, runnerID)
}

func (g *guardedClient) ListRunners(ctx context.Context) ([]Runner, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.detached {
		return nil, errDetached
	}
	return g.c.ListRunners(ctx)
}

func (g *guardedClient) SetMaxRunners(n int) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.detached {
		g.c.SetMaxRunners(n)
	}
}

func (s *Scheduler) attached() []*scaleSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]*scaleSet, 0, len(s.sets))
	for _, h := range s.sets {
		list = append(list, h)
	}
	return list
}

// Scale handles one listener message: it records the jobs, acquires the
// wanted jobs that fit, keeps the statistics, and resizes the scale set.
// The same message can come again after an error, so each step is
// idempotent.
func (h *scaleSet) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.s
	if msg != nil {
		if msg.Statistics != nil {
			h.stats = statsOf(*msg.Statistics)
		}
		if err := h.handle(ctx, msg); err != nil {
			return err
		}
	}
	// A resize error does not fail the message: the next poll resizes again.
	if err := h.resize(ctx); err != nil {
		s.log.Warn("resize scale set", "scale_set", h.set.ID, "err", err)
	}
	s.updateMaxRunners(ctx)
	s.publishPoolStats(ctx, h.set.PoolID)
	return nil
}

func (h *scaleSet) handle(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	for _, m := range msg.JobAvailableMessages {
		h.s.log.Debug("job message", "type", "available", "request", m.RunnerRequestID, "job", m.JobID, "name", m.JobDisplayName)
	}
	for _, m := range msg.JobAssignedMessages {
		h.s.log.Debug("job message", "type", "assigned", "request", m.RunnerRequestID, "job", m.JobID, "name", m.JobDisplayName)
	}
	for _, m := range msg.JobStartedMessages {
		h.s.log.Debug("job message", "type", "started", "request", m.RunnerRequestID, "job", m.JobID, "name", m.JobDisplayName, "runner", m.RunnerName)
	}
	for _, m := range msg.JobCompletedMessages {
		h.s.log.Debug("job message", "type", "completed", "request", m.RunnerRequestID, "job", m.JobID, "name", m.JobDisplayName, "result", m.Result)
	}
	if err := h.handleAvailable(ctx, msg.JobAvailableMessages); err != nil {
		return err
	}
	for _, m := range msg.JobAssignedMessages {
		jm := h.jobMessage(store.JobAssigned, m.JobMessageBase)
		jm.AssignedAt = cmp.Or(jm.AssignedAt, h.s.now())
		job, err := h.s.st.UpsertJob(ctx, jm)
		if err != nil {
			return err
		}
		h.s.publish(events.JobState, job)
	}
	for _, m := range msg.JobStartedMessages {
		if err := h.handleStarted(ctx, m); err != nil {
			return err
		}
	}
	for _, m := range msg.JobCompletedMessages {
		if err := h.handleCompleted(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// handleAvailable records the offered jobs, decides which to take, and
// acquires the oldest ones that the capacity allows.
func (h *scaleSet) handleAvailable(ctx context.Context, list []*scaleset.JobAvailable) error {
	if len(list) == 0 {
		return nil
	}
	s := h.s
	pool, err := s.pool(ctx, h.set.PoolID)
	if err != nil {
		return err
	}
	st, err := s.st.GetSettings(ctx)
	if err != nil {
		return err
	}
	list = slices.Clone(list)
	slices.SortStableFunc(list, func(a, b *scaleset.JobAvailable) int { return a.QueueTime.Compare(b.QueueTime) })

	var wanted []*scaleset.JobAvailable
	for _, m := range list {
		jm := h.jobMessage(store.JobQueued, m.JobMessageBase)
		key := rerunKey{jm.Repository, jm.RunID, jm.JobName}
		s.mu.Lock()
		_, jm.Debug = s.reruns[key]
		s.mu.Unlock()

		job, err := s.st.UpsertJob(ctx, jm)
		if err != nil {
			return err
		}
		reason, err := h.check(ctx, pool, st, &job)
		if errors.Is(err, errCheckLater) {
			// Another job of the message must not wait for this one.
			s.log.Warn("skip a job until GitHub offers it again", "job", job.ID, "err", err)
			s.publish(events.JobState, job)
			continue
		}
		if err != nil {
			return err
		}
		if jm.Debug {
			s.mu.Lock()
			delete(s.reruns, key)
			if reason == "" {
				s.debugJobs[h.set.ID] = append(s.debugJobs[h.set.ID], job.ID)
			}
			s.mu.Unlock()
		}
		// The reason is for a job that waits: a job past queued keeps its own.
		if job.State == store.JobQueued && job.RejectReason != reason {
			if err := s.st.SetJobRejectReason(ctx, job.ID, reason); err != nil {
				return err
			}
			job.RejectReason = reason
		}
		s.publish(events.JobState, job)
		// A job past queued was acquired before this message came again.
		if reason == "" && job.State == store.JobQueued {
			wanted = append(wanted, m)
		}
	}
	if len(wanted) == 0 {
		return nil
	}

	room, err := h.room(ctx, pool, st)
	if err != nil {
		return err
	}
	wanted = wanted[:max(0, min(room, len(wanted)))]
	if len(wanted) == 0 {
		return nil
	}
	// GitHub acquires by its request IDs; the job rows use jobKey.
	ids := make([]int64, len(wanted))
	keys := make(map[int64][]int64)
	for i, m := range wanted {
		ids[i] = m.RunnerRequestID
		keys[m.RunnerRequestID] = append(keys[m.RunnerRequestID], jobKey(m.JobMessageBase))
	}
	acquired, err := h.client.AcquireJobs(ctx, ids)
	if err != nil {
		return fmt.Errorf("acquire jobs: %w", err)
	}
	for _, id := range acquired {
		k := keys[id]
		if len(k) == 0 {
			continue
		}
		keys[id] = k[1:]
		job, err := s.st.UpsertJob(ctx, store.JobMessage{RunnerRequestID: k[0], State: store.JobAssigned, AssignedAt: s.now()})
		if err != nil {
			return err
		}
		s.publish(events.JobState, job)
	}
	return nil
}

// errCheckLater wraps a CheckJob error that can pass, such as a GitHub
// outage. The scheduler skips the job and checks it at the next offer.
var errCheckLater = errors.New("the job check failed")

// check returns why the scheduler does not take the job, or "" to take it.
// For a job of an enabled pool it reads the run and records the run data
// that gives the cache refs of the job.
func (h *scaleSet) check(ctx context.Context, pool *store.Pool, st store.Settings, job *store.Job) (string, error) {
	switch {
	case pool == nil:
		return "The pool no longer exists.", nil
	case !pool.Enabled:
		return "The pool is disabled.", nil
	}
	c, err := h.s.checkJob(ctx, job.Repository, job.RunID)
	switch {
	case errors.Is(err, ErrRunUnreadable):
		return "runplane cannot read the run: install the GitHub App on this repository.", nil
	case err != nil:
		return "", fmt.Errorf("%w: %w", errCheckLater, err)
	}
	if err := h.s.st.SetJobRunRef(ctx, job.ID, c.Ref); err != nil {
		return "", err
	}
	job.Event = cmp.Or(c.Ref.Event, job.Event)
	job.HeadBranch, job.PRNumber, job.BaseRef = c.Ref.HeadBranch, c.Ref.PRNumber, c.Ref.BaseRef

	switch {
	case !c.Private && !st.AllowPublicRepos:
		return "Public repository: the setting allow_public_repos is off.", nil
	case c.Fork && !st.AllowForkPRs:
		return "Fork pull request: the setting allow_fork_prs is off.", nil
	case !c.Private:
		vm, err := h.vmOnly(ctx, *pool)
		if err != nil {
			return "", err
		}
		if !vm {
			return "Public repository: a server for this pool does not run VM mode.", nil
		}
	}
	return "", nil
}

// vmOnly tells if each runner that can get a job of the pool is in a VM:
// the live sandboxes of the scale set, and the new sandboxes of the pool on
// the online servers. The setting mode does not count, because a server
// runs its own mode, and a mode change applies only after a restart.
func (h *scaleSet) vmOnly(ctx context.Context, pool store.Pool) (bool, error) {
	if !h.s.serversVM(pool) {
		return false, nil
	}
	live, err := h.live(ctx)
	if err != nil {
		return false, err
	}
	return allVM(live), nil
}

// serversVM tells if each online server that can take sandboxes of the pool
// runs VM mode.
func (s *Scheduler) serversVM(pool store.Pool) bool {
	for _, host := range s.fleet.Online() {
		if canHost(host, pool) && host.Driver.Mode() != sandbox.ModeVM {
			return false
		}
	}
	return true
}

func allVM(list []store.Sandbox) bool {
	return !slices.ContainsFunc(list, func(b store.Sandbox) bool { return b.Mode != sandbox.ModeVM })
}

// checkJob calls GitHub.CheckJob one time for each run in checkTTL. It
// keeps ErrRunUnreadable too, but no other error.
func (s *Scheduler) checkJob(ctx context.Context, repository string, runID int64) (JobCheck, error) {
	key := checkKey{repository, runID}
	s.mu.Lock()
	e, ok := s.checks[key]
	s.mu.Unlock()
	if ok && s.now().Sub(e.at) < checkTTL {
		return e.JobCheck, e.err
	}
	c, err := s.gh.CheckJob(ctx, repository, runID)
	if err != nil && !errors.Is(err, ErrRunUnreadable) {
		return JobCheck{}, fmt.Errorf("check job: %w", err)
	}
	s.mu.Lock()
	s.checks[key] = checkEntry{c, err, s.now()}
	s.mu.Unlock()
	return c, err
}

// room is the count of jobs that the scale set can still take: the runners
// that it can have now, less the jobs that GitHub assigned to it.
func (h *scaleSet) room(ctx context.Context, pool *store.Pool, st store.Settings) (int, error) {
	live, err := h.live(ctx)
	if err != nil {
		return 0, err
	}
	return min(pool.MaxRunners, len(live)+h.s.fits(h.s.hosts(ctx), *pool, st)) - h.stats.AssignedJobs, nil
}

func (h *scaleSet) handleStarted(ctx context.Context, m *scaleset.JobStarted) error {
	s := h.s
	jm := h.jobMessage(store.JobRunning, m.JobMessageBase)
	jm.StartedAt = cmp.Or(jm.StartedAt, s.now())
	job, err := s.st.UpsertJob(ctx, jm)
	if err != nil {
		return err
	}
	b, ok, err := h.link(ctx, &job, m.RunnerName)
	if err != nil {
		return err
	}
	if ok {
		if job.LogState == store.LogNone {
			if err := s.st.SetJobLogState(ctx, job.ID, store.LogLive); err != nil {
				return err
			}
			job.LogState = store.LogLive
		}
		s.setState(ctx, b.ID, sandbox.StateBusy, "")
		if job.Debug && !b.Debug {
			s.holdForDebug(ctx, b)
		}
	}
	s.publish(events.JobState, job)
	s.enrich(job)
	s.watchSteps(job.ID)
	return nil
}

func (h *scaleSet) handleCompleted(ctx context.Context, m *scaleset.JobCompleted) error {
	s := h.s
	jm := h.jobMessage(store.JobCompleted, m.JobMessageBase)
	jm.Result = jobResult(m.Result)
	jm.FinishedAt = cmp.Or(jm.FinishedAt, s.now())
	prev, err := s.st.GetJobByRunnerRequestID(ctx, jm.RunnerRequestID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	job, err := s.st.UpsertJob(ctx, jm)
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.results, job.ID)
	s.mu.Unlock()
	if job.SandboxID == "" && m.RunnerName != "" {
		if _, _, err := h.link(ctx, &job, m.RunnerName); err != nil {
			return err
		}
	}
	// A JIT runner takes one job, so its sandbox stops counting as a runner.
	// Else the resize after this message stops a warm sandbox in its place.
	if job.SandboxID != "" {
		s.setState(ctx, job.SandboxID, sandbox.StateStopping, "job completed")
	}
	s.publish(events.JobState, job)
	// A message that comes again after the log is stored needs no second finish step.
	if prev.State != store.JobCompleted || prev.LogState != store.LogStored {
		s.finish(job.ID)
	}
	return nil
}

// link records that the sandbox with the runner name runs the job. It
// returns false if no sandbox has the name, for example a runner that
// runplane did not start.
func (h *scaleSet) link(ctx context.Context, job *store.Job, runnerName string) (store.Sandbox, bool, error) {
	s := h.s
	b, err := s.st.GetSandboxByRunnerName(ctx, runnerName)
	if errors.Is(err, store.ErrNotFound) {
		s.log.Info("job on an unknown runner", "job", job.ID, "runner", runnerName)
		return store.Sandbox{}, false, nil
	}
	if err != nil {
		return store.Sandbox{}, false, err
	}
	if job.SandboxID != b.ID || job.ServerID != b.ServerID {
		if err := s.st.SetJobSandbox(ctx, job.ID, b.ID, b.ServerID); err != nil {
			return store.Sandbox{}, false, err
		}
		job.SandboxID, job.ServerID = b.ID, b.ServerID
	}
	if b.JobID != job.ID {
		if err := s.st.SetSandboxJob(ctx, b.ID, job.ID); err != nil {
			return store.Sandbox{}, false, err
		}
		b.JobID = job.ID
	}
	return b, true, nil
}

func (h *scaleSet) jobMessage(state string, m scaleset.JobMessageBase) store.JobMessage {
	repo := m.RepositoryName
	if m.OwnerName != "" {
		repo = m.OwnerName + "/" + m.RepositoryName
	}
	return store.JobMessage{
		RunnerRequestID: jobKey(m),
		State:           state,
		ScaleSetID:      h.set.ID,
		PoolID:          h.set.PoolID,
		Repository:      repo,
		WorkflowRef:     m.JobWorkflowRef,
		JobName:         m.JobDisplayName,
		RunID:           m.WorkflowRunID,
		Event:           m.EventName,
		Labels:          m.RequestLabels,
		QueuedAt:        m.QueueTime,
		AssignedAt:      m.ScaleSetAssignTime,
		StartedAt:       m.RunnerAssignTime,
		FinishedAt:      m.FinishTime,
	}
}

// jobKey gives the key of the job row of a message. GitHub sends
// runnerRequestId 0 in scale set messages and names each job by its jobId,
// a UUID. For such a message the key comes from the jobId: a negative
// number, so it cannot match a real request ID.
func jobKey(m scaleset.JobMessageBase) int64 {
	if m.RunnerRequestID != 0 || m.JobID == "" {
		return m.RunnerRequestID
	}
	h := fnv.New64a()
	h.Write([]byte(m.JobID))
	return -int64(h.Sum64()>>1) - 1
}

// jobResult maps the result of a JobCompleted message to a job result.
func jobResult(r string) string {
	switch strings.ToLower(r) {
	case "succeeded", "succeededwithissues":
		return store.ResultSucceeded
	case "canceled", "cancelled", "abandoned":
		return store.ResultCancelled
	case "skipped":
		return store.ResultSkipped
	}
	return store.ResultFailed
}

func statsOf(x scaleset.RunnerScaleSetStatistic) store.ScaleSetStats {
	return store.ScaleSetStats{
		AvailableJobs:     x.TotalAvailableJobs,
		AcquiredJobs:      x.TotalAcquiredJobs,
		AssignedJobs:      x.TotalAssignedJobs,
		RunningJobs:       x.TotalRunningJobs,
		RegisteredRunners: x.TotalRegisteredRunners,
		BusyRunners:       x.TotalBusyRunners,
		IdleRunners:       x.TotalIdleRunners,
	}
}

// pool returns the pool, or nil if it is deleted.
func (s *Scheduler) pool(ctx context.Context, id string) (*store.Pool, error) {
	p, err := s.st.GetPool(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}
