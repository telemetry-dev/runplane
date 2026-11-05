package scheduler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/policy"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// scaleSet is the Scaler of one scale set.
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
func (s *Scheduler) Attach(set store.ScaleSet, c ScaleSetClient) Scaler {
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

var errDetached = errors.New("the scale set has no listener")

// guardedClient passes calls to the client of a scale set until detach.
type guardedClient struct {
	mu sync.RWMutex
	c  ScaleSetClient // nil after detach
}

// detach waits for the calls in progress. Later calls do nothing.
func (g *guardedClient) detach() {
	g.mu.Lock()
	g.c = nil
	g.mu.Unlock()
}

func (g *guardedClient) AcquireJobs(ctx context.Context, keys []int64) ([]int64, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.c == nil {
		return nil, errDetached
	}
	return g.c.AcquireJobs(ctx, keys)
}

func (g *guardedClient) GenerateJITConfig(ctx context.Context, runnerName string) (JITConfig, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.c == nil {
		return JITConfig{}, errDetached
	}
	return g.c.GenerateJITConfig(ctx, runnerName)
}

func (g *guardedClient) RemoveRunner(ctx context.Context, runnerID int64) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.c == nil {
		return errDetached
	}
	return g.c.RemoveRunner(ctx, runnerID)
}

func (g *guardedClient) ListRunners(ctx context.Context) ([]Runner, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.c == nil {
		return nil, errDetached
	}
	return g.c.ListRunners(ctx)
}

func (g *guardedClient) SetMaxRunners(n int) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.c != nil {
		g.c.SetMaxRunners(n)
	}
}

func (s *Scheduler) attached() []*scaleSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Collect(maps.Values(s.sets))
}

// Scale handles one listener message: it records the jobs, acquires the
// wanted jobs that fit, keeps the statistics, and resizes the scale set.
// The same message can come again after an error, so each step is
// idempotent.
func (h *scaleSet) Scale(ctx context.Context, m Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.s
	if m.Stats != nil {
		h.stats = *m.Stats
	}
	if err := h.handle(ctx, m); err != nil {
		return err
	}
	// A resize error does not fail the message: the next poll resizes again.
	if err := h.resize(ctx); err != nil {
		s.log.Warn("resize scale set", "scale_set", h.set.ID, "err", err)
	}
	s.updateMaxRunners(ctx)
	s.publishPoolStats(ctx, h.set.PoolID)
	return nil
}

func (h *scaleSet) handle(ctx context.Context, m Message) error {
	if err := h.handleAvailable(ctx, m.Available); err != nil {
		return err
	}
	for _, jm := range m.Assigned {
		jm := h.own(jm)
		jm.AssignedAt = cmp.Or(jm.AssignedAt, h.s.now())
		job, _, err := h.s.st.UpsertJob(ctx, jm.JobMessage, h.s.now())
		if err != nil {
			return err
		}
		h.s.bus.Publish(events.JobState(job))
	}
	for _, jm := range m.Started {
		if err := h.handleStarted(ctx, h.own(jm)); err != nil {
			return err
		}
	}
	for _, jm := range m.Completed {
		if err := h.handleCompleted(ctx, h.own(jm)); err != nil {
			return err
		}
	}
	return nil
}

func (h *scaleSet) own(jm JobMessage) JobMessage {
	jm.ScaleSetID, jm.PoolID = h.set.ID, h.set.PoolID
	return jm
}

// handleAvailable records the offered jobs, decides which to take, and
// acquires the oldest ones that the capacity allows.
func (h *scaleSet) handleAvailable(ctx context.Context, list []JobMessage) error {
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
	var modes []sandbox.Mode
	if pool != nil && pool.Enabled {
		if modes, err = h.runnerModes(ctx, *pool); err != nil {
			return err
		}
	}
	list = slices.Clone(list)
	slices.SortStableFunc(list, func(a, b JobMessage) int { return a.QueuedAt.Compare(b.QueuedAt) })

	var wanted []int64
	for _, jm := range list {
		jm := h.own(jm)
		key := rerunKey{jm.Repository, jm.RunID, jm.JobName}
		s.mu.Lock()
		_, jm.Debug = s.reruns[key]
		s.mu.Unlock()

		job, _, err := s.st.UpsertJob(ctx, jm.JobMessage, s.now())
		if err != nil {
			return err
		}
		reason, err := h.check(ctx, pool, st, modes, &job)
		if errors.Is(err, errCheckLater) {
			// Another job of the message must not wait for this one.
			s.log.Warn("skip a job until GitHub offers it again", "job", job.ID, "err", err)
			s.bus.Publish(events.JobState(job))
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
		s.bus.Publish(events.JobState(job))
		// A job past queued was acquired before this message came again.
		if reason == "" && job.State == store.JobQueued {
			wanted = append(wanted, jm.RunnerRequestID)
		}
	}
	if len(wanted) == 0 {
		return nil
	}

	// The room is the runners that the scale set can have now, less the jobs
	// that GitHub assigned to it.
	live, err := h.live(ctx)
	if err != nil {
		return err
	}
	room := min(pool.MaxRunners, len(live)+s.fits(s.hosts(ctx), *pool, st)) - h.stats.AssignedJobs
	wanted = wanted[:max(0, min(room, len(wanted)))]
	if len(wanted) == 0 {
		return nil
	}
	acquired, err := h.client.AcquireJobs(ctx, wanted)
	if err != nil {
		return fmt.Errorf("acquire jobs: %w", err)
	}
	for _, key := range acquired {
		job, _, err := s.st.UpsertJob(ctx, store.JobMessage{RunnerRequestID: key, State: store.JobAssigned, AssignedAt: s.now()}, s.now())
		if err != nil {
			return err
		}
		s.bus.Publish(events.JobState(job))
	}
	return nil
}

// errCheckLater wraps a CheckJob error that can pass, such as a GitHub
// outage. The scheduler skips the job and checks it at the next offer.
var errCheckLater = errors.New("the job check failed")

// check returns why the scheduler does not take the job, or "" to take it.
// modes are the modes of the runners that can get a job of the pool. For a
// job of an enabled pool it reads the run and records the run data that
// gives the cache refs of the job.
func (h *scaleSet) check(ctx context.Context, pool *store.Pool, st store.Settings, modes []sandbox.Mode, job *store.Job) (string, error) {
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
	return policy.Refusal(st, policy.Job{Private: c.Private, Fork: c.Fork}, modes), nil
}

// runnerModes returns the modes of each runner that can get a job of the
// pool: the live sandboxes of the scale set, and the new sandboxes of the
// pool on the online servers.
func (h *scaleSet) runnerModes(ctx context.Context, pool store.Pool) ([]sandbox.Mode, error) {
	var modes []sandbox.Mode
	for _, host := range h.s.fleet.Online() {
		if canHost(host, pool) {
			modes = append(modes, host.Driver.Mode())
		}
	}
	live, err := h.live(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range live {
		modes = append(modes, b.Mode)
	}
	return modes, nil
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

func (h *scaleSet) handleStarted(ctx context.Context, jm JobMessage) error {
	s := h.s
	jm.StartedAt = cmp.Or(jm.StartedAt, s.now())
	job, _, err := s.st.UpsertJob(ctx, jm.JobMessage, s.now())
	if err != nil {
		return err
	}
	b, ok, err := h.link(ctx, &job, jm.RunnerName)
	if err != nil {
		return err
	}
	if ok {
		if err := s.jobLogs.MarkLive(ctx, &job); err != nil {
			return err
		}
		s.setState(ctx, b.ID, sandbox.StateBusy, "")
		if job.Debug && !b.Debug {
			s.holdForDebug(ctx, b)
		}
	}
	s.bus.Publish(events.JobState(job))
	s.enrich(job)
	s.watchSteps(job.ID)
	return nil
}

func (h *scaleSet) handleCompleted(ctx context.Context, jm JobMessage) error {
	s := h.s
	jm.FinishedAt = cmp.Or(jm.FinishedAt, s.now())
	job, prev, err := s.st.UpsertJob(ctx, jm.JobMessage, s.now())
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.results, job.ID)
	s.mu.Unlock()
	if job.SandboxID == "" && jm.RunnerName != "" {
		if _, _, err := h.link(ctx, &job, jm.RunnerName); err != nil {
			return err
		}
	}
	// A JIT runner takes one job, so its sandbox stops counting as a runner.
	// Else the resize after this message stops a warm sandbox in its place.
	if job.SandboxID != "" {
		s.setState(ctx, job.SandboxID, sandbox.StateStopping, "job completed")
	}
	s.bus.Publish(events.JobState(job))
	// A message that comes again after the log is stored needs no second finish step.
	if prev != store.JobCompleted || job.LogState != store.LogStored {
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
	if job.SandboxID == b.ID && b.JobID == job.ID {
		return b, true, nil
	}
	linked, b, err := s.st.LinkJobSandbox(ctx, job.ID, b.ID)
	if err != nil {
		return store.Sandbox{}, false, err
	}
	*job = linked
	return b, true, nil
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
