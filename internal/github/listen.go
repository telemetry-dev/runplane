package github

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/store"
)

// Listener restart waits.
const (
	minBackoff = time.Second
	maxBackoff = time.Minute
)

// setListener runs the listener of one scale set. It is also the
// scheduler.ScaleSetClient of the scale set.
type setListener struct {
	s      *Service
	id     string // scale set row ID
	target target
	log    *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}
	max    atomic.Int64

	mu       sync.Mutex
	cl       *scaleset.Client
	label    string
	githubID int64
	session  *scaleset.MessageSessionClient
	running  *listener.Listener
	status   string // the last written status and error, to skip equal writes
	errText  string
	answered bool // GitHub answered a poll since the last restart
}

var _ scheduler.ScaleSetClient = (*setListener)(nil)

func (s *Service) listener(id string) *setListener {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	return s.listeners[id]
}

// startListener attaches the scale set to the scheduler and starts its
// listener in the background. It returns nil if it cannot.
func (s *Service) startListener(ctx context.Context, row store.ScaleSet, t target) *setListener {
	cl, err := s.scalesetClient(ctx, t)
	if err != nil {
		s.setStatus(ctx, row.ID, store.ScaleSetError, err.Error())
		return nil
	}
	lctx, cancel := context.WithCancel(s.base)
	l := &setListener{
		s: s, id: row.ID, target: t, cl: cl, cancel: cancel, done: make(chan struct{}),
		log:   s.log.With("scale_set", t.pool.Label, "owner", t.owner()),
		label: t.pool.Label, githubID: row.GitHubID, status: row.Status, errText: row.Error,
	}
	scaler := s.sched.Attach(row, l)
	s.lmu.Lock()
	s.listeners[row.ID] = l
	s.lmu.Unlock()
	go l.run(lctx, scaler)
	return l
}

// stopListener stops the listener and waits for it. With detach, the
// scheduler drops the scale set.
func (s *Service) stopListener(id string, detach bool) {
	s.lmu.Lock()
	l := s.listeners[id]
	delete(s.listeners, id)
	s.lmu.Unlock()
	if l == nil {
		return
	}
	l.cancel()
	<-l.done
	if detach {
		s.sched.Detach(id)
	}
}

// stopListeners stops all listeners for shutdown. The scheduler keeps the
// scale sets, because their sandboxes continue across a restart.
func (s *Service) stopListeners() {
	s.lmu.Lock()
	all := s.listeners
	s.listeners = map[string]*setListener{}
	s.lmu.Unlock()
	for _, l := range all {
		l.cancel()
	}
	for _, l := range all {
		<-l.done
	}
}

// run listens until ctx ends. After a failure it waits 1 second, then
// twice as long after each further failure, up to 1 minute.
func (l *setListener) run(ctx context.Context, scaler listener.Scaler) {
	defer close(l.done)
	wait := minBackoff
	for {
		err := l.listen(ctx, scaler)
		if ctx.Err() != nil {
			return
		}
		if l.setStatus(ctx, store.ScaleSetError, err.Error()) {
			l.log.Warn("scale set listener stopped", "err", err)
		}
		if errors.Is(err, scaleset.UnauthorizedError) {
			l.renewClient(ctx)
		}
		l.mu.Lock()
		if l.answered {
			wait = minBackoff
		}
		l.answered = false
		l.mu.Unlock()
		if sleep(ctx, wait) != nil {
			return
		}
		wait = min(2*wait, maxBackoff)
	}
}

// renewClient replaces the client after GitHub refused its token. A
// client keeps its Actions service token until the token expires, and
// GitHub can revoke it sooner.
func (l *setListener) renewClient(ctx context.Context) {
	cl, err := l.s.scalesetClient(ctx, l.target)
	if err != nil {
		l.log.Warn("renew the scale set client", "err", err)
		return
	}
	l.mu.Lock()
	l.cl = cl
	l.mu.Unlock()
}

func (l *setListener) client() *scaleset.Client {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cl
}

// listen makes the scale set on GitHub if necessary, opens a session, and
// runs the listener on it.
func (l *setListener) listen(ctx context.Context, scaler listener.Scaler) error {
	id, err := l.ensureRemote(ctx)
	if err != nil {
		return fmt.Errorf("make the scale set on GitHub: %w", err)
	}
	session, err := l.client().MessageSessionClient(ctx, int(id), l.s.owner)
	switch {
	case err == nil:
	case isSessionConflict(err):
		return fmt.Errorf("another listener has the session of scale set %s in %s: a different runplane or "+
			"Actions Runner Controller uses the same scale set name. Stop it or change the pool label. "+
			"runplane tries again", l.labelNow(), l.target.owner())
	case errors.Is(err, scaleset.NotFoundError):
		l.clearRemote(ctx)
		return fmt.Errorf("the scale set is gone on GitHub, runplane makes it again: %w", err)
	default:
		return fmt.Errorf("open a listener session: %w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := session.Close(cctx); err != nil {
			l.log.Debug("close listener session", "err", err)
		}
	}()

	run, err := listener.New(pollRecorder{session, l}, listener.Config{
		ScaleSetID: int(id),
		MaxRunners: int(l.max.Load()),
		Logger:     slog.New(quiet{l.log.Handler()}),
	})
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.session, l.running = session, run
	l.mu.Unlock()
	// The scheduler can change the value while New runs.
	run.SetMaxRunners(int(l.max.Load()))
	defer func() {
		l.mu.Lock()
		l.session, l.running = nil, nil
		l.mu.Unlock()
	}()

	l.setStatus(ctx, store.ScaleSetListening, "")
	if st := session.Session().Statistics; st != nil {
		l.record(ctx, st, time.Now())
	}
	return run.Run(ctx, scaler)
}

// ensureRemote returns the GitHub ID of the scale set. It finds the scale
// set by name, or makes it, so that a restart or a second try does not make
// a second scale set.
func (l *setListener) ensureRemote(ctx context.Context) (int64, error) {
	l.mu.Lock()
	id, label := l.githubID, l.label
	l.mu.Unlock()
	if id != 0 {
		return id, nil
	}
	cl := l.client()
	set, err := cl.GetRunnerScaleSet(ctx, defaultRunnerGroup, label)
	if err != nil {
		return 0, err
	}
	switch {
	case set == nil:
		set, err = cl.CreateRunnerScaleSet(ctx, scaleSetSpec(label, defaultRunnerGroup))
	case !hasLabel(set.Labels, label) || !set.RunnerSetting.DisableUpdate:
		set, err = cl.UpdateRunnerScaleSet(ctx, set.ID, scaleSetSpec(label, cmp.Or(set.RunnerGroupID, defaultRunnerGroup)))
	}
	if err != nil {
		return 0, err
	}
	group := int64(cmp.Or(set.RunnerGroupID, defaultRunnerGroup))
	if err := l.s.st.SetScaleSetRemote(ctx, l.id, int64(set.ID), group, label); err != nil {
		return 0, err
	}
	l.mu.Lock()
	l.githubID = int64(set.ID)
	l.mu.Unlock()
	return int64(set.ID), nil
}

func (l *setListener) clearRemote(ctx context.Context) {
	l.mu.Lock()
	l.githubID = 0
	label := l.label
	l.mu.Unlock()
	if err := l.s.st.SetScaleSetRemote(ctx, l.id, 0, defaultRunnerGroup, label); err != nil {
		l.log.Warn("clear scale set ID", "err", err)
	}
}

func hasLabel(labels []scaleset.Label, name string) bool {
	for _, l := range labels {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

// isSessionConflict tells if another client owns the message session.
func isSessionConflict(err error) bool {
	return errors.Is(err, scaleset.ConflictError) || strings.Contains(err.Error(), "TaskAgentSessionConflictException")
}

// setStatus writes the status if it changed and tells if it did.
func (l *setListener) setStatus(ctx context.Context, status, msg string) bool {
	msg = truncate(msg, 1000)
	l.mu.Lock()
	same := l.status == status && l.errText == msg
	l.status, l.errText = status, msg
	l.mu.Unlock()
	if !same {
		l.s.setStatus(ctx, l.id, status, msg)
	}
	return !same
}

// record writes the statistics of a message or a session.
func (l *setListener) record(ctx context.Context, st *scaleset.RunnerScaleSetStatistic, at time.Time) {
	err := l.s.st.SetScaleSetStats(ctx, l.id, store.ScaleSetStats{
		AvailableJobs: st.TotalAvailableJobs, AcquiredJobs: st.TotalAcquiredJobs, AssignedJobs: st.TotalAssignedJobs,
		RunningJobs: st.TotalRunningJobs, RegisteredRunners: st.TotalRegisteredRunners,
		BusyRunners: st.TotalBusyRunners, IdleRunners: st.TotalIdleRunners,
	}, at)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		l.log.Warn("record scale set statistics", "err", err)
	}
}

// polled records one poll that GitHub answered.
func (l *setListener) polled(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) {
	now := time.Now()
	l.mu.Lock()
	l.answered = true
	l.mu.Unlock()
	if msg != nil && msg.Statistics != nil {
		l.record(ctx, msg.Statistics, now)
	} else if err := l.s.st.TouchScaleSetPoll(ctx, l.id, now); err != nil && !errors.Is(err, store.ErrNotFound) {
		l.log.Warn("record scale set poll", "err", err)
	}
	if !l.setStatus(ctx, store.ScaleSetListening, "") {
		l.s.polledChanged(now)
	}
}

// pollRecorder records each answered poll of the listener.
type pollRecorder struct {
	*scaleset.MessageSessionClient
	l *setListener
}

func (p pollRecorder) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	msg, err := p.MessageSessionClient.GetMessage(ctx, lastMessageID, maxCapacity)
	if err == nil {
		p.l.polled(ctx, msg)
	}
	return msg, err
}

func (l *setListener) setLabel(label string) {
	l.mu.Lock()
	l.label = label
	l.mu.Unlock()
}

func (l *setListener) labelNow() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.label
}

// AcquireJobs acquires job requests through the listener session.
func (l *setListener) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	l.mu.Lock()
	session := l.session
	l.mu.Unlock()
	if session == nil {
		return nil, fmt.Errorf("acquire jobs: the listener of scale set %s has no session", l.labelNow())
	}
	return session.AcquireJobs(ctx, requestIDs)
}

func (l *setListener) GenerateJITConfig(ctx context.Context, runnerName string) (scheduler.JITConfig, error) {
	l.mu.Lock()
	id := l.githubID
	l.mu.Unlock()
	if id == 0 {
		return scheduler.JITConfig{}, fmt.Errorf("make JIT config: scale set %s is not on GitHub yet", l.labelNow())
	}
	cfg, err := l.client().GenerateJitRunnerConfig(ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{Name: runnerName, WorkFolder: "_work"}, int(id))
	if err != nil {
		return scheduler.JITConfig{}, fmt.Errorf("make JIT config: %w", err)
	}
	if cfg.Runner == nil || cfg.EncodedJITConfig == "" {
		return scheduler.JITConfig{}, errors.New("make JIT config: GitHub sent no runner")
	}
	return scheduler.JITConfig{Encoded: cfg.EncodedJITConfig, RunnerID: int64(cfg.Runner.ID), RunnerName: cfg.Runner.Name}, nil
}

func (l *setListener) RemoveRunner(ctx context.Context, runnerID int64) error {
	return removeRunnerErr(l.client().RemoveRunner(ctx, runnerID))
}

func (l *setListener) ListRunners(ctx context.Context) ([]scheduler.Runner, error) {
	return l.s.listRunners(ctx, l.target, l.labelNow())
}

func (l *setListener) SetMaxRunners(n int) {
	n = min(max(n, 0), math.MaxInt32)
	l.max.Store(int64(n))
	l.mu.Lock()
	run := l.running
	l.mu.Unlock()
	if run != nil {
		run.SetMaxRunners(n)
	}
}
