// Package github connects runplane to GitHub: the GitHub App manifest flow,
// app and installation tokens, REST calls, runner scale sets with their
// listeners, job data for the scheduler, and runner releases.
package github

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset/listener"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

// Scheduler takes the messages of the scale set listeners.
type Scheduler interface {
	// Attach gives the scheduler the client of a scale set whose listener
	// starts. The listener sends each message to the returned Scaler.
	Attach(set store.ScaleSet, c scheduler.ScaleSetClient) listener.Scaler
	// Detach tells the scheduler that the scale set has no listener now.
	// The scheduler must not use its client after this call.
	Detach(scaleSetID string)
}

// Bundles makes runner bundles on the servers.
type Bundles interface {
	// EnsureLatest makes the bundles of this runner version on each server.
	EnsureLatest(ctx context.Context, version string) error
}

// Notifier sends an event to the notification channels that want it.
type Notifier interface {
	Notify(ctx context.Context, e notify.Event) error
}

type Config struct {
	Store     *store.Store
	Box       *secret.Box
	Bus       *events.Bus
	Log       *slog.Logger
	Scheduler Scheduler
	Bundles   Bundles  // optional
	Notifier  Notifier // optional
	// HTTP is the client for all GitHub requests. Nil uses a new client.
	HTTP *http.Client
	// APIURL and WebURL default to https://api.github.com and https://github.com.
	APIURL string
	WebURL string
	// Owner names the listener sessions on GitHub. The default is the host name.
	Owner string
}

// Service implements api.GitHub and scheduler.GitHub.
type Service struct {
	st      *store.Store
	box     *secret.Box
	bus     *events.Bus
	log     *slog.Logger
	sched   Scheduler
	bundles Bundles
	notify  Notifier
	api     *rest
	http    *http.Client // for the scaleset library
	web     string
	owner   string

	// ops serializes the changes of installations and scale sets.
	ops  sync.Mutex
	poke chan struct{}

	// base is the parent context of the listeners. Run cancels it.
	base context.Context
	stop context.CancelFunc

	credMu sync.Mutex
	cred   *creds

	tokMu  sync.Mutex
	tokens map[int64]token

	lmu       sync.Mutex
	listeners map[string]*setListener

	checks runChecks

	warnMu sync.Mutex
	warned map[string]bool // runner versions with a sent end date warning

	pollEvent atomic.Int64 // unix ms of the last github.state event for a poll
}

func New(cfg Config) *Service {
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	// The scaleset library has no timeout of its own except for the long
	// poll, so its client gets one. It shares the transport.
	sc := *hc
	if sc.Timeout == 0 {
		sc.Timeout = 2 * time.Minute
	}
	owner := cfg.Owner
	if owner == "" {
		owner, _ = os.Hostname()
	}
	base, stop := context.WithCancel(context.Background())
	return &Service{
		st:        cfg.Store,
		box:       cfg.Box,
		bus:       cfg.Bus,
		log:       cmp.Or(cfg.Log, slog.New(slog.DiscardHandler)),
		sched:     cfg.Scheduler,
		bundles:   cfg.Bundles,
		notify:    cfg.Notifier,
		api:       newREST(hc, cmp.Or(cfg.APIURL, "https://api.github.com")),
		http:      &sc,
		web:       strings.TrimRight(cmp.Or(cfg.WebURL, "https://github.com"), "/"),
		owner:     cmp.Or(owner, "runplane"),
		poke:      make(chan struct{}, 1),
		base:      base,
		stop:      stop,
		tokens:    map[int64]token{},
		listeners: map[string]*setListener{},
		warned:    map[string]bool{},
	}
}

const (
	syncInterval    = 5 * time.Minute
	releaseInterval = time.Hour
)

// Run syncs the installations and starts the scale set listeners, then
// keeps them in step with GitHub, the pools, and the settings. It syncs each
// 5 minutes and checks the runner releases each hour. It returns after ctx
// ends and all listeners have stopped.
func (s *Service) Run(ctx context.Context) {
	settings, unsubscribe := s.bus.Subscribe(64)
	defer unsubscribe()

	var wg sync.WaitGroup
	wg.Go(func() { s.releaseLoop(ctx) })

	s.syncAll(ctx)
	tick := time.NewTicker(syncInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stop()
			s.stopListeners()
			wg.Wait()
			return
		case <-tick.C:
			s.syncAll(ctx)
		case <-s.poke:
			s.reconcileNow(ctx)
		case e := <-settings:
			if e.Type == events.SettingsChanged {
				s.reconcileNow(ctx)
			}
		}
	}
}

// PoolsChanged makes the scale sets agree with the pools soon. It does not
// block.
func (s *Service) PoolsChanged(context.Context) {
	select {
	case s.poke <- struct{}{}:
	default:
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

// syncAll reads the installations from GitHub, then makes the scale sets
// agree. The scale sets also start when GitHub cannot be read.
func (s *Service) syncAll(ctx context.Context) {
	s.ops.Lock()
	defer s.ops.Unlock()
	if err := s.syncInstallations(ctx); err != nil && !errors.Is(err, ErrNotConnected) {
		s.log.Warn("sync GitHub installations", "err", err)
	}
	s.reconcile(ctx)
}

func (s *Service) reconcileNow(ctx context.Context) {
	s.ops.Lock()
	defer s.ops.Unlock()
	s.reconcile(ctx)
}

// anyInstallation returns an installation that is not suspended.
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

// pollEventEvery limits the github.state events that only a new poll time causes.
const pollEventEvery = 30 * time.Second

// changed tells the browsers that the GitHub status changed.
func (s *Service) changed() {
	s.bus.Publish(events.Event{Type: events.GitHubState})
}

// polledChanged tells the browsers about a new poll time, at most once in
// pollEventEvery for all listeners.
func (s *Service) polledChanged(now time.Time) {
	last := s.pollEvent.Load()
	if now.UnixMilli()-last < pollEventEvery.Milliseconds() || !s.pollEvent.CompareAndSwap(last, now.UnixMilli()) {
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
