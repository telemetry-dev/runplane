// Package github connects runplane to GitHub: the GitHub App manifest flow,
// app and installation tokens, REST calls, runner scale sets with their
// listeners, and job data for the scheduler.
package github

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/ghrest"
	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

// Scheduler takes the messages of the scale set listeners.
type Scheduler interface {
	// Attach gives the scheduler the client of a scale set whose listener
	// starts. The listener sends each message to the returned Scaler.
	Attach(set store.ScaleSet, c scheduler.ScaleSetClient) scheduler.Scaler
	// Detach tells the scheduler that the scale set has no listener now.
	// The scheduler must not use its client after this call.
	Detach(scaleSetID string)
}

type Config struct {
	Store     *store.Store
	Box       *secret.Box
	Bus       *events.Bus
	Log       *slog.Logger
	Scheduler Scheduler
}

// Service implements api.GitHub and scheduler.GitHub.
type Service struct {
	st    *store.Store
	box   *secret.Box
	bus   *events.Bus
	log   *slog.Logger
	sched Scheduler
	api   ghrest.Client
	web   string
	owner string // names the listener sessions on GitHub

	// ops serializes the changes of installations and scale sets.
	ops sync.Mutex
	// The changes of the settings and the pools, from New on, so that Run
	// misses no change that comes before it starts.
	settings <-chan events.SettingsChanged
	pools    <-chan events.PoolsChanged

	// base is the parent context of the listeners and of the subscription.
	// Run cancels it.
	base context.Context
	stop context.CancelFunc

	credMu sync.Mutex
	cred   *creds

	tokMu  sync.Mutex
	tokens map[int64]token

	lmu       sync.Mutex
	listeners map[string]*setListener

	pollEvent atomic.Int64 // unix ms of the last github.state event for a poll
}

func New(cfg Config) *Service {
	owner, _ := os.Hostname()
	base, stop := context.WithCancel(context.Background())
	return &Service{
		st:        cfg.Store,
		box:       cfg.Box,
		bus:       cfg.Bus,
		log:       cfg.Log,
		sched:     cfg.Scheduler,
		web:       ghrest.Web,
		owner:     cmp.Or(owner, "runplane"),
		settings:  events.Subscribe[events.SettingsChanged](base, cfg.Bus),
		pools:     events.Subscribe[events.PoolsChanged](base, cfg.Bus),
		base:      base,
		stop:      stop,
		tokens:    map[int64]token{},
		listeners: map[string]*setListener{},
	}
}

// Run syncs the installations and starts the scale set listeners, then
// keeps them in step with GitHub, the pools, and the settings. It syncs each
// 5 minutes, and does one reconcile for the changes of the pools and the
// settings that wait. It returns after ctx ends and all listeners have
// stopped.
func (s *Service) Run(ctx context.Context) {
	s.syncAll(ctx)
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stop()
			s.stopListeners()
			return
		case <-tick.C:
			s.syncAll(ctx)
		case <-s.pools:
			s.dropChanges()
			s.reconcileNow(ctx)
		case <-s.settings:
			s.dropChanges()
			s.reconcileNow(ctx)
		}
	}
}

// dropChanges takes the changes of the settings and the pools that wait,
// because the next reconcile reads the pools and the settings after all of
// them.
func (s *Service) dropChanges() {
	for {
		select {
		case <-s.pools:
		case <-s.settings:
		default:
			return
		}
	}
}

// Token returns an installation token for public GitHub reads, such as the
// runner releases, or "" when runplane has no usable installation.
func (s *Service) Token(ctx context.Context) (string, error) {
	inst, ok := s.anyInstallation(ctx)
	if !ok {
		return "", nil
	}
	return s.installationToken(ctx, inst.ID)
}

// syncAll is Sync for the loop. The scale sets also start when GitHub
// cannot be read.
func (s *Service) syncAll(ctx context.Context) {
	if err := s.Sync(ctx); err != nil && !errors.Is(err, errNotConnected) {
		s.log.Warn("sync GitHub installations", "err", err)
	}
}

func (s *Service) reconcileNow(ctx context.Context) {
	s.ops.Lock()
	defer s.ops.Unlock()
	s.reconcile(ctx)
}

func (s *Service) anyInstallation(ctx context.Context) (store.Installation, bool) {
	list, err := s.st.ListInstallations(ctx)
	if err != nil {
		return store.Installation{}, false
	}
	for _, inst := range list {
		if inst.SuspendedAt == nil {
			return inst, true
		}
	}
	return store.Installation{}, false
}

// changed tells the browsers that the GitHub status changed.
func (s *Service) changed() {
	s.bus.Publish(events.GitHubState{})
}

// polledChanged tells the browsers about a new poll time, at most once in
// 30 seconds for all listeners.
func (s *Service) polledChanged(now time.Time) {
	last := s.pollEvent.Load()
	if now.UnixMilli()-last < (30*time.Second).Milliseconds() || !s.pollEvent.CompareAndSwap(last, now.UnixMilli()) {
		return
	}
	s.changed()
}

// quiet drops log records below Warn. The scaleset library logs each poll
// and each token refresh at Info.
type quiet struct{ slog.Handler }

func (q quiet) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn && q.Handler.Enabled(ctx, l)
}

func (q quiet) WithAttrs(a []slog.Attr) slog.Handler { return quiet{q.Handler.WithAttrs(a)} }
func (q quiet) WithGroup(n string) slog.Handler      { return quiet{q.Handler.WithGroup(n)} }

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
