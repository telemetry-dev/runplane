// Package versions keeps the runner versions of the primary in the
// runner_bundles rows: the build result of each version and arch, the
// latest actions/runner release, and the end dates. It warns about a
// version in use that GitHub stops soon, and it tells the fleet which
// versions to make ready on a server.
package versions

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/ghrest"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/runner"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

// GitHub gives the credentials of the GitHub App. A read with a token has a
// higher rate limit than an anonymous read.
type GitHub interface {
	// Token returns an installation token, or "" when runplane has no
	// usable installation.
	Token(ctx context.Context) (string, error)
	// ReadScope returns the REST path of an organization or a repository
	// that the app can read, with the auth of its installation. ok is false
	// when runplane has none.
	ReadScope(ctx context.Context) (path string, auth ghrest.Auth, ok bool)
}

type Config struct {
	Store  *store.Store
	Bus    *events.Bus
	GitHub GitHub
	// Notify sends the runner.deprecation alert.
	Notify func(notify.Event)
	// InUse returns the runner versions that the servers give new
	// sandboxes.
	InUse func() []string
	API   ghrest.Client
	Log   *slog.Logger
}

type Service struct {
	cfg Config
	log *slog.Logger

	mu     sync.Mutex
	latest string // the latest actions/runner release of the last check
}

func New(cfg Config) *Service {
	return &Service{cfg: cfg, log: cmp.Or(cfg.Log, slog.New(slog.DiscardHandler))}
}

// Run checks the runner releases and the end dates each hour until ctx
// ends.
func (s *Service) Run(ctx context.Context) {
	for {
		if err := s.check(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("check runner releases", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// Versions returns the runner versions to try on a server of arch, best
// first: the latest release, then up to two older versions that are ready
// on other servers of the arch. The latest release is the newer one of the
// last check and of the rows. Versions returns nil while runplane knows no
// version.
func (s *Service) Versions(ctx context.Context, arch string) ([]string, error) {
	bundles, err := s.cfg.Store.ListRunnerBundles(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	latest := s.latest
	s.mu.Unlock()
	if len(bundles) > 0 && version.Compare(bundles[0].Version, latest) > 0 {
		latest = bundles[0].Version
	}
	if latest == "" {
		return nil, nil
	}
	list := []string{latest}
	for _, b := range bundles { // newest first
		if len(list) == 3 {
			break
		}
		if b.Arch == arch && b.Status == store.BundleReady && version.Compare(b.Version, latest) < 0 {
			list = append(list, b.Version)
		}
	}
	return list, nil
}

// Built records the build of a bundle on one server: a ready row with its
// SHA-256, or a failed row with the error. It publishes the row when its
// status or its error changed. A failure does not change a bundle that is
// ready, because it can be ready on other servers.
func (s *Service) Built(ctx context.Context, b store.RunnerBundle) {
	before, _ := s.cfg.Store.GetRunnerBundle(ctx, b.Version, b.Arch)
	if err := s.cfg.Store.SetRunnerBundleBuild(ctx, b, time.Now()); err != nil {
		s.log.Warn("record runner bundle", "version", b.Version, "arch", b.Arch, "err", err)
		return
	}
	after, err := s.cfg.Store.GetRunnerBundle(ctx, b.Version, b.Arch)
	if err != nil || after.Status == before.Status && after.Error == before.Error {
		return
	}
	s.cfg.Bus.Publish(events.RunnerVersion(after))
}

func (s *Service) check(ctx context.Context) error {
	rel, err := runner.Releases{API: s.cfg.API, Auth: runner.TokenAuth(s.cfg.GitHub.Token)}.Latest(ctx)
	if err != nil {
		return fmt.Errorf("read the latest runner release: %w", err)
	}
	s.mu.Lock()
	s.latest = rel.Version
	s.mu.Unlock()
	// The rows of the arches that the servers have show the release.
	arches, err := s.serverArches(ctx)
	if err != nil {
		return err
	}
	for _, arch := range arches {
		s.setDates(ctx, rel.Version, arch, rel.PublishedAt, nil)
	}
	if err := s.readEndDates(ctx); err != nil {
		s.log.Warn("read runner end dates", "err", err)
	}
	return s.warnEnds(ctx)
}

// readEndDates reads the end date of each known runner version with the
// runner deprecations API (GitHub changelog, 3 September 2026). The API
// needs an organization or a repository, so it uses an installation.
func (s *Service) readEndDates(ctx context.Context) error {
	scope, auth, ok := s.cfg.GitHub.ReadScope(ctx)
	if !ok {
		return nil
	}
	bundles, err := s.cfg.Store.ListRunnerBundles(ctx)
	if err != nil {
		return err
	}
	var errs []error
	done := map[string]bool{}
	for _, b := range bundles {
		if done[b.Version] {
			continue
		}
		done[b.Version] = true
		var d struct {
			RegistrationDeprecatesAt *time.Time `json:"registration_deprecates_at"`
			RuntimeDeprecatesAt      *time.Time `json:"runtime_deprecates_at"`
		}
		err := s.cfg.API.JSON(ctx, ghrest.Call{
			Method: http.MethodGet, Path: scope + "/actions/runners/deprecations/" + url.PathEscape(b.Version), Auth: auth,
		}, &d)
		var apiErr *ghrest.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		end := earliest(d.RuntimeDeprecatesAt, d.RegistrationDeprecatesAt)
		if end == nil {
			continue
		}
		for _, x := range bundles {
			if x.Version == b.Version {
				s.setDates(ctx, x.Version, x.Arch, nil, end)
			}
		}
	}
	return errors.Join(errs...)
}

// Each new sandbox registers a JIT runner and then runs a job, so a version
// ends at the first of its registration and runtime end dates.
func earliest(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil || a.Before(*b):
		return a
	}
	return b
}

// setDates writes the dates of one bundle row and publishes the row when it
// changed.
func (s *Service) setDates(ctx context.Context, version, arch string, releasedAt, endDate *time.Time) {
	before, berr := s.cfg.Store.GetRunnerBundle(ctx, version, arch)
	if err := s.cfg.Store.SetRunnerBundleDates(ctx, version, arch, releasedAt, endDate, time.Now()); err != nil {
		s.log.Warn("record runner version", "version", version, "arch", arch, "err", err)
		return
	}
	after, err := s.cfg.Store.GetRunnerBundle(ctx, version, arch)
	if err != nil {
		return
	}
	if berr == nil && sameTime(before.ReleasedAt, after.ReleasedAt) && sameTime(before.EndDate, after.EndDate) {
		return
	}
	s.cfg.Bus.Publish(events.RunnerVersion(after))
}

// warnEnds sends one notification for each version that the servers give
// new sandboxes and that ends soon. The rows keep the sent warnings.
func (s *Service) warnEnds(ctx context.Context) error {
	bundles, err := s.cfg.Store.ListRunnerBundles(ctx)
	if err != nil {
		return err
	}
	inUse := s.cfg.InUse()
	now := time.Now()
	warned := map[string]bool{}
	ends := map[string]time.Time{}
	for _, b := range bundles {
		warned[b.Version] = warned[b.Version] || b.Warned
		if slices.Contains(inUse, b.Version) && b.EndsSoon(now) {
			ends[b.Version] = *b.EndDate
		}
	}
	for version, end := range ends {
		if warned[version] {
			continue
		}
		s.log.Warn("runner version ends soon", "version", version, "end_date", end)
		s.cfg.Notify(notify.RunnerDeprecationEvent(version, end))
		if err := s.cfg.Store.SetRunnerBundleWarned(ctx, version); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) serverArches(ctx context.Context) ([]string, error) {
	servers, err := s.cfg.Store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	var arches []string
	for _, srv := range servers {
		if srv.Arch != "" && !slices.Contains(arches, srv.Arch) {
			arches = append(arches, srv.Arch)
		}
	}
	return arches, nil
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
