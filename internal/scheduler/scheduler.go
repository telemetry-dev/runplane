// Package scheduler runs the runner scale sets. It takes jobs from GitHub,
// places sandboxes on servers, keeps warm sandboxes, records the jobs, and
// cleans up what the messages miss.
package scheduler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/joblog"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

const (
	cleanupEvery   = time.Minute
	flushEvery     = 10 * time.Second
	retentionEvery = 24 * time.Hour
	// A running job whose sandbox is gone for this long, with no completion
	// message, is cancelled.
	lostJobAfter = 10 * time.Minute
	// The waiting sandboxes of a server with no connection for this long stop
	// counting as runners. A short loss, such as an agent update, keeps them.
	lostServerAfter = 15 * time.Minute
	// A queued job that runplane did not take for this long is closed.
	unclaimedJobAfter = time.Hour
	// The cleanup runs the finish step again for at most this many jobs at
	// a time.
	resumeFinishBatch = 20
	// CheckJob results stay valid this long for the other jobs of a run.
	checkTTL = 10 * time.Minute
	// A result that a runner reported stays this long for cancelLostJobs.
	resultTTL = time.Hour
	// A debug re-run waits this long for its next attempt.
	rerunTTL = 6 * time.Hour
)

// runnerStates are the states of a sandbox that counts as a runner of its scale set.
var runnerStates = []sandbox.State{sandbox.StateStarting, sandbox.StateIdle, sandbox.StateBusy}

type Config struct {
	Store   *store.Store
	Bus     *events.Bus
	Fleet   Fleet
	GitHub  GitHub
	Log     *slog.Logger // nil: no log
	JobLogs *joblog.Logs
	Now     func() time.Time // nil: time.Now
}

type Scheduler struct {
	st      *store.Store
	bus     *events.Bus
	fleet   Fleet
	gh      GitHub
	log     *slog.Logger
	jobLogs *joblog.Logs
	now     func() time.Time

	// bg is the context of work that outlives the call that starts it.
	bg     context.Context
	stopBg context.CancelFunc
	wg     sync.WaitGroup

	pending atomic.Uint32 // work bits for Run
	wakeCh  chan struct{}

	// The changes of the pools and the settings, from New on. Run resizes
	// the scale sets for them.
	pools    <-chan events.PoolsChanged
	settings <-chan events.SettingsChanged

	placeMu sync.Mutex // one placement at a time, see place

	mu        sync.Mutex
	sets      map[string]*scaleSet      // attached scale sets by ID
	reserved  map[string]reservation    // by server ID
	starting  map[string]string         // runner name by sandbox ID, from before the JIT config until Driver.Start returns
	reruns    map[rerunKey]time.Time    // debug re-runs that wait for their next attempt
	debugJobs map[string][]string       // debug job IDs by scale set ID that wait for a debug sandbox
	checks    map[checkKey]checkEntry   // CheckJob results by run
	metrics   map[string]*metricsBuffer // samples by sandbox ID until the next flush
	finishing map[string]bool           // job IDs with a finish step in progress
	results   map[string]runnerResult   // job results that the runners reported, by job ID
	watching  map[string]chan struct{}  // stop channels of the step reads, by job ID
}

// runnerResult is the result that a runner reported for its job. The
// cleanup uses it if GitHub never reports the job as completed.
type runnerResult struct {
	result string
	at     time.Time
}

// The work bits of Run. Each work updates the max runners, so doMaxRunners
// only wakes Run.
const (
	doMaxRunners uint32 = 1 << iota
	doResize
	doCleanup
)

func New(c Config) *Scheduler {
	bg, stop := context.WithCancel(context.Background())
	s := &Scheduler{
		st:        c.Store,
		bus:       c.Bus,
		fleet:     c.Fleet,
		gh:        c.GitHub,
		log:       cmp.Or(c.Log, slog.New(slog.DiscardHandler)),
		jobLogs:   c.JobLogs,
		now:       c.Now,
		bg:        bg,
		stopBg:    stop,
		wakeCh:    make(chan struct{}, 1),
		pools:     events.Subscribe[events.PoolsChanged](bg, c.Bus),
		settings:  events.Subscribe[events.SettingsChanged](bg, c.Bus),
		sets:      map[string]*scaleSet{},
		reserved:  map[string]reservation{},
		starting:  map[string]string{},
		reruns:    map[rerunKey]time.Time{},
		debugJobs: map[string][]string{},
		checks:    map[checkKey]checkEntry{},
		metrics:   map[string]*metricsBuffer{},
		finishing: map[string]bool{},
		results:   map[string]runnerResult{},
		watching:  map[string]chan struct{}{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// Run does the cleanup at once and then each minute, the retention each
// day, the metrics flush each 10 seconds, and the work that the changes of
// the pools and the settings, CapacityChanged, and sandbox events ask for.
// It returns when ctx ends, after the job finish steps stop.
func (s *Scheduler) Run(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		s.stopBg()
		s.mu.Unlock()
		s.wg.Wait()
	}()
	s.cleanup(ctx)
	s.retention(ctx)
	s.resizeAll(ctx)
	s.updateMaxRunners(ctx)

	cleanup := time.NewTicker(cleanupEvery)
	defer cleanup.Stop()
	flush := time.NewTicker(flushEvery)
	defer flush.Stop()
	retention := time.NewTicker(retentionEvery)
	defer retention.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flushMetrics(context.WithoutCancel(ctx))
			return
		case <-flush.C:
			s.flushMetrics(ctx)
		case <-retention.C:
			s.retention(ctx)
		case <-cleanup.C:
			s.pending.Or(doCleanup)
			s.work(ctx)
		case <-s.wakeCh:
			s.work(ctx)
		case <-s.pools:
			s.wake(doResize)
		case <-s.settings:
			s.wake(doResize)
		}
	}
}

func (s *Scheduler) work(ctx context.Context) {
	w := s.pending.Swap(0)
	if w&doCleanup != 0 {
		s.cleanup(ctx)
	}
	if w&(doCleanup|doResize) != 0 {
		s.resizeAll(ctx)
	}
	s.updateMaxRunners(ctx)
}

// wake asks Run to do the work bits soon.
func (s *Scheduler) wake(work uint32) {
	s.pending.Or(work)
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

// CapacityChanged compares the sandboxes with the rows again and resizes all
// scale sets. The fleet calls it when a server comes or goes.
func (s *Scheduler) CapacityChanged() { s.wake(doCleanup | doResize) }

// StopSandbox removes the runner from GitHub and stops the sandbox. It also
// stops a busy sandbox, because an admin asks for it.
func (s *Scheduler) StopSandbox(ctx context.Context, id string) error {
	b, err := s.st.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	if b.State.Done() {
		return nil
	}
	return s.stop(ctx, b, "stopped by a user")
}

// RerunJob asks GitHub to run the job again. With debug, the next attempt of
// the same run and job name gets a debug sandbox.
func (s *Scheduler) RerunJob(ctx context.Context, jobID string, debug bool) error {
	job, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	key := rerunKey{job.Repository, job.RunID, job.JobName}
	// Remember the key first: the next attempt can arrive before Rerun returns.
	if debug {
		s.mu.Lock()
		s.reruns[key] = s.now()
		s.mu.Unlock()
	}
	if err := s.gh.Rerun(ctx, job); err != nil {
		if debug {
			s.mu.Lock()
			delete(s.reruns, key)
			s.mu.Unlock()
		}
		return fmt.Errorf("rerun job: %w", err)
	}
	return nil
}

// stop removes the runner, also a busy one, and stops the sandbox.
func (s *Scheduler) stop(ctx context.Context, b store.Sandbox, reason string) error {
	if err := s.removeRunner(ctx, b); err != nil && !errors.Is(err, ErrRunnerBusy) {
		s.log.Warn("remove runner", "sandbox", b.ID, "runner", b.RunnerName, "err", err)
	}
	return s.stopNow(ctx, b, reason)
}

// stopNow sets the sandbox to stopping and stops it, with no GitHub call.
func (s *Scheduler) stopNow(ctx context.Context, b store.Sandbox, reason string) error {
	s.setState(ctx, b.ID, sandbox.StateStopping, reason)
	d := s.fleet.Driver(b.ServerID)
	if d == nil {
		return fmt.Errorf("stop sandbox: server %s is not connected", b.ServerID)
	}
	if err := d.Stop(ctx, b.ID); err != nil {
		return fmt.Errorf("stop sandbox: %w", err)
	}
	return nil
}

// removeRunner removes the runner of the sandbox from GitHub. Without an
// attached scale set it does nothing: GitHub has no listener for it, and
// the cleanup removes the runner when the scale set comes back.
func (s *Scheduler) removeRunner(ctx context.Context, b store.Sandbox) error {
	s.mu.Lock()
	h := s.sets[b.ScaleSetID]
	s.mu.Unlock()
	if h == nil || b.RunnerID == 0 {
		return nil
	}
	return h.client.RemoveRunner(ctx, b.RunnerID)
}

// setState moves the sandbox forward to state and publishes the change.
func (s *Scheduler) setState(ctx context.Context, id string, state sandbox.State, reason string) (store.Sandbox, bool) {
	return s.setStateAt(ctx, id, state, reason, s.now())
}

// setStateAt is setState at t. A sandbox that is done gets no more samples,
// so its buffered samples go to its job at once.
func (s *Scheduler) setStateAt(ctx context.Context, id string, state sandbox.State, reason string, t time.Time) (store.Sandbox, bool) {
	b, changed, err := s.st.AdvanceSandboxState(ctx, id, state, reason, t)
	if err != nil {
		s.log.Warn("set sandbox state", "sandbox", id, "state", state, "err", err)
		return b, false
	}
	if !changed {
		return b, false
	}
	s.bus.Publish(events.SandboxState(b))
	s.publishPoolStats(ctx, b.PoolID)
	if state.Done() {
		s.flushSandbox(ctx, b.ID)
		s.wake(doMaxRunners)
	}
	return b, true
}

// goBg runs fn in the background until Run returns.
func (s *Scheduler) goBg(fn func(ctx context.Context)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Run cancels bg under mu before it waits, so no goroutine starts after the wait.
	if s.bg.Err() != nil {
		return
	}
	s.wg.Go(func() { fn(s.bg) })
}

func (s *Scheduler) publishPoolStats(ctx context.Context, poolID string) {
	counts, err := s.st.PoolCounts(ctx, s.now())
	if err != nil {
		s.log.Warn("count pool runners and jobs", "err", err)
		return
	}
	p := counts[poolID]
	p.PoolID = poolID
	s.bus.Publish(events.PoolStats(p))
}

// timeLimit is the time limit of a job of the pool. A warm sandbox of the
// pool waits for a job for at most this long.
func timeLimit(pool store.Pool, st store.Settings) time.Duration {
	return time.Duration(cmp.Or(pool.TimeLimitMinutes, st.DefaultTimeLimitMinutes)) * time.Minute
}
