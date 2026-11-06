package github

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/runplane/runplane/internal/ghapp"
	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/store"
)

// setListener runs the listener of one scale set. It is the listener.Scaler
// of the scale set, which gives each message to the scheduler, and the
// scheduler.ScaleSetClient of the scale set.
type setListener struct {
	s      *Service
	id     string // scale set row ID
	target target
	log    *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}
	max    atomic.Int64
	scaler scheduler.Scaler

	mu        sync.Mutex
	cl        *scaleset.Client
	label     string
	githubID  int64
	session   *scaleset.MessageSessionClient
	running   *listener.Listener
	offered   map[int64]int64 // request IDs by job key, of the jobs that the last message offered
	answered  bool            // GitHub answered a poll since the last restart
	listenErr string          // the last error of the listener, "" while it listens
	renameErr string          // the error of the last rename, "" if it worked
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
		label: t.pool.Label, githubID: row.GitHubID,
	}
	l.scaler = s.sched.Attach(row, l)
	s.lmu.Lock()
	s.listeners[row.ID] = l
	s.lmu.Unlock()
	go l.run(lctx)
	return l
}

// stopListener stops the listener and waits for it. Then the scheduler
// drops the scale set.
func (s *Service) stopListener(id string) {
	s.lmu.Lock()
	l := s.listeners[id]
	delete(s.listeners, id)
	s.lmu.Unlock()
	if l == nil {
		return
	}
	l.cancel()
	<-l.done
	s.sched.Detach(id)
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

// run listens until ctx ends.
func (l *setListener) run(ctx context.Context) {
	defer close(l.done)
	wait := time.Second
	for {
		err := l.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		if l.setListenErr(ctx, err.Error()) {
			l.log.Warn("scale set listener stopped", "err", err)
		}
		if errors.Is(err, scaleset.UnauthorizedError) {
			l.renewClient(ctx)
		}
		l.mu.Lock()
		if l.answered {
			wait = time.Second
		}
		l.answered = false
		l.mu.Unlock()
		if sleep(ctx, wait) != nil {
			return
		}
		wait = min(2*wait, time.Minute)
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

func (l *setListener) listen(ctx context.Context) error {
	id, err := l.ensureRemote(ctx)
	if err != nil {
		return fmt.Errorf("make the scale set on GitHub: %w", err)
	}
	session, err := l.client().MessageSessionClient(ctx, int(id), l.s.owner)
	switch {
	case err == nil:
	case errors.Is(err, scaleset.ConflictError) || strings.Contains(err.Error(), "TaskAgentSessionConflictException"):
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

	l.setListenErr(ctx, "")
	if st := session.Session().Statistics; st != nil {
		l.record(ctx, st, time.Now())
	}
	return run.Run(ctx, l)
}

// setListenErr records the error of the listener, "" while it listens, and
// writes the status. It tells if the status changed.
func (l *setListener) setListenErr(ctx context.Context, msg string) bool {
	l.mu.Lock()
	l.listenErr = msg
	l.mu.Unlock()
	return l.report(ctx)
}

// report writes the status of the scale set: an error if the last rename
// or the listener failed, listening while the listener has a session, and
// else nothing. It tells if the status changed. While a listener runs, only
// the listener writes the status of its row.
func (l *setListener) report(ctx context.Context) bool {
	l.mu.Lock()
	msg, listening := cmp.Or(l.renameErr, l.listenErr), l.session != nil
	l.mu.Unlock()
	switch {
	case msg != "":
		return l.s.setStatus(ctx, l.id, store.ScaleSetError, msg)
	case listening:
		return l.s.setStatus(ctx, l.id, store.ScaleSetListening, "")
	}
	return false
}

// rename gives the scale set the label of its pool, on GitHub and in its
// row. A failed rename stays the error of the scale set until a rename
// works.
func (l *setListener) rename(ctx context.Context, row store.ScaleSet, label string) {
	renameErr := ""
	if row.GitHubID != 0 {
		_, err := l.client().UpdateRunnerScaleSet(ctx, int(row.GitHubID), scaleSetSpec(label, int(row.RunnerGroupID)))
		if err != nil {
			renameErr = "rename the scale set to " + label + " on GitHub: " + err.Error()
		}
	}
	if renameErr == "" {
		if err := l.s.st.SetScaleSetRemote(ctx, row.ID, row.GitHubID, row.RunnerGroupID, label, time.Now()); err != nil {
			l.log.Warn("rename scale set", "err", err)
			return
		}
		l.setLabel(label)
	}
	l.mu.Lock()
	l.renameErr = renameErr
	l.mu.Unlock()
	l.report(ctx)
}

func (l *setListener) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	m, offered := message(l.log, msg)
	l.mu.Lock()
	l.offered = offered
	l.mu.Unlock()
	return l.scaler.Scale(ctx, m)
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
	set, err := cl.GetRunnerScaleSet(ctx, ghapp.DefaultRunnerGroup, label)
	if err != nil {
		return 0, err
	}
	hasLabel := func(x scaleset.Label) bool { return strings.EqualFold(x.Name, label) }
	switch {
	case set == nil:
		set, err = cl.CreateRunnerScaleSet(ctx, scaleSetSpec(label, ghapp.DefaultRunnerGroup))
	case !slices.ContainsFunc(set.Labels, hasLabel) || !set.RunnerSetting.DisableUpdate:
		set, err = cl.UpdateRunnerScaleSet(ctx, set.ID, scaleSetSpec(label, cmp.Or(set.RunnerGroupID, ghapp.DefaultRunnerGroup)))
	}
	if err != nil {
		return 0, err
	}
	group := int64(cmp.Or(set.RunnerGroupID, ghapp.DefaultRunnerGroup))
	if err := l.s.st.SetScaleSetRemote(ctx, l.id, int64(set.ID), group, label, time.Now()); err != nil {
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
	if err := l.s.st.SetScaleSetRemote(ctx, l.id, 0, ghapp.DefaultRunnerGroup, label, time.Now()); err != nil {
		l.log.Warn("clear scale set ID", "err", err)
	}
}

func (l *setListener) record(ctx context.Context, st *scaleset.RunnerScaleSetStatistic, at time.Time) {
	if err := l.s.st.SetScaleSetStats(ctx, l.id, statsOf(*st), at); err != nil && !errors.Is(err, store.ErrNotFound) {
		l.log.Warn("record scale set statistics", "err", err)
	}
}

// pollRecorder records each poll of the listener that GitHub answered.
type pollRecorder struct {
	*scaleset.MessageSessionClient
	l *setListener
}

func (p pollRecorder) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	msg, err := p.MessageSessionClient.GetMessage(ctx, lastMessageID, maxCapacity)
	if err != nil {
		return msg, err
	}
	l, now := p.l, time.Now()
	l.mu.Lock()
	l.answered = true
	l.mu.Unlock()
	if msg != nil && msg.Statistics != nil {
		l.record(ctx, msg.Statistics, now)
	} else if err := l.s.st.TouchScaleSetPoll(ctx, l.id, now); err != nil && !errors.Is(err, store.ErrNotFound) {
		l.log.Warn("record scale set poll", "err", err)
	}
	if !l.setListenErr(ctx, "") {
		l.s.polledChanged(now)
	}
	return msg, nil
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

// AcquireJobs acquires the jobs of the last message with these keys. GitHub
// acquires a job by its request ID, and it can send the request ID 0 for
// more than one job (see jobKey). The acquired zeros then go to the keys in
// their order.
func (l *setListener) AcquireJobs(ctx context.Context, keys []int64) ([]int64, error) {
	l.mu.Lock()
	session, offered := l.session, l.offered
	l.mu.Unlock()
	if session == nil {
		return nil, fmt.Errorf("acquire jobs: the listener of scale set %s has no session", l.labelNow())
	}
	var ids []int64
	byID := map[int64][]int64{}
	for _, k := range keys {
		if id, ok := offered[k]; ok {
			ids = append(ids, id)
			byID[id] = append(byID[id], k)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	acquired, err := session.AcquireJobs(ctx, ids)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, id := range acquired {
		if k := byID[id]; len(k) > 0 {
			out = append(out, k[0])
			byID[id] = k[1:]
		}
	}
	return out, nil
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
