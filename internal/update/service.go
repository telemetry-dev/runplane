package update

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

// State is the state of the update of this process.
type State string

const (
	Idle       State = "idle"
	Applying   State = "applying"   // runplane downloads and installs the release
	Restarting State = "restarting" // the new binary starts
)

var (
	// ErrNoUpdate is the error of Apply when no newer release exists.
	ErrNoUpdate = errors.New("no newer release")
	// ErrBusy is the error of Apply while an update runs.
	ErrBusy = errors.New("an update runs")
)

// checkTTL keeps the release reads of the dashboard far below the GitHub
// limit of 60 anonymous requests an hour.
const checkTTL = 10 * time.Minute

type Config struct {
	Updater *Updater
	// Restart ends the process after an update, so that systemd starts the
	// new binary. nil: the caller of Apply restarts runplane.
	Restart func()
	// Store and Notify serve Run: the auto_update setting, and the
	// update.available alert.
	Store  *store.Store
	Notify func(notify.Event)
	Log    *slog.Logger     // nil: no log
	Now    func() time.Time // nil: time.Now
}

// Service runs the updates of this process: the update status, an update
// that an admin starts, and the daily check with the auto update. It holds
// the update state, so that two updates never run at the same time.
type Service struct {
	cfg Config
	log *slog.Logger

	checkMu sync.Mutex // one release read at a time
	checked time.Time  // of last, which stays valid for checkTTL
	last    Status

	mu    sync.Mutex
	state State
}

func NewService(cfg Config) *Service {
	return &Service{cfg: cfg, log: cmp.Or(cfg.Log, slog.New(slog.DiscardHandler)), state: Idle}
}

// Check returns the latest release. It reads it at most once in checkTTL,
// and it keeps no error. A repository without releases has no update.
func (s *Service) Check(ctx context.Context) (Status, error) {
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	now := s.now()
	if now.Sub(s.checked) < checkTTL {
		return s.last, nil
	}
	st, err := s.cfg.Updater.Check(ctx)
	if errors.Is(err, ErrNoRelease) {
		st, err = Status{Current: s.cfg.Updater.Current}, nil
	}
	if err != nil {
		return Status{}, err
	}
	s.last, s.checked = st, now
	return st, nil
}

func (s *Service) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Apply installs the latest release and restarts runplane. It returns
// ErrNoUpdate when no newer release exists, and ErrBusy while an update
// runs. The status tells the release.
func (s *Service) Apply(ctx context.Context) (Status, error) {
	st, err := s.Check(ctx)
	if err != nil {
		return st, err
	}
	if !st.Available {
		return st, ErrNoUpdate
	}
	if !s.setState(Idle, Applying) {
		return st, ErrBusy
	}
	if err := s.cfg.Updater.Apply(ctx, st.Latest); err != nil {
		s.setState(Applying, Idle)
		return st, err
	}
	s.setState(Applying, Restarting)
	s.log.Info("runplane restarts to run the new version", "version", st.Latest)
	if s.cfg.Restart != nil {
		s.cfg.Restart()
	}
	return st, nil
}

// setState reports false if the state is not from.
func (s *Service) setState(from, to State) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != from {
		return false
	}
	s.state = to
	return true
}

// Run checks for a new release each day until ctx ends or an update
// restarts runplane. It sends one update.available notification for each
// new version, and with auto_update it installs the release. Only a signed
// release build checks.
func (s *Service) Run(ctx context.Context) {
	u := s.cfg.Updater
	if !version.IsRelease(u.Current) || u.PublicKey == "" {
		s.log.Debug("no update checks: this binary is not a signed release", "version", u.Current)
		return
	}
	told := ""
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 24 * time.Hour
		st, err := s.Check(ctx)
		if err != nil {
			s.log.Warn("check for updates", "err", err)
			continue
		}
		if !st.Available {
			continue
		}
		settings, err := s.cfg.Store.GetSettings(ctx)
		if err != nil {
			s.log.Warn("read settings for an update", "err", err)
			continue
		}
		if settings.AutoUpdate {
			_, err := s.Apply(ctx)
			if err == nil {
				return
			}
			s.log.Error("install update", "version", st.Latest, "err", err)
		}
		if told != st.Latest {
			told = st.Latest
			s.cfg.Notify(notify.UpdateAvailableEvent(st.Current, st.Latest, st.URL))
		}
	}
}

func (s *Service) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}
