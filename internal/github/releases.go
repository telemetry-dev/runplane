package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/notify"
)

// endWarning is how long before an end date runplane warns.
const endWarning = 14 * 24 * time.Hour

func (s *Service) releaseLoop(ctx context.Context) {
	for {
		if err := s.checkReleases(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("check runner releases", "err", err)
		}
		if sleep(ctx, releaseInterval) != nil {
			return
		}
	}
}

// checkReleases records the latest actions/runner release for the server
// arches, asks for its bundles, reads the end dates of all known versions,
// and warns about a version in use that ends soon.
func (s *Service) checkReleases(ctx context.Context) error {
	var a auth // a token has a higher rate limit than anonymous reads
	if inst, ok := s.anyInstallation(ctx); ok {
		a = installAuth{s, inst.ID}
	}
	var rel struct {
		TagName     string     `json:"tag_name"`
		PublishedAt *time.Time `json:"published_at"`
	}
	if err := s.api.json(ctx, call{method: http.MethodGet, path: "/repos/actions/runner/releases/latest", auth: a}, &rel); err != nil {
		return fmt.Errorf("read the latest runner release: %w", err)
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	if latest == "" {
		return errors.New("read the latest runner release: no tag name")
	}
	arches, err := s.serverArches(ctx)
	if err != nil {
		return err
	}
	// The row comes first, so that the bundle builder updates it.
	for _, arch := range arches {
		s.setBundleDates(ctx, latest, arch, rel.PublishedAt, nil)
	}
	if s.bundles != nil {
		if err := s.bundles.EnsureLatest(ctx, latest); err != nil {
			s.log.Warn("make runner bundles", "version", latest, "err", err)
		}
	}
	if err := s.readEndDates(ctx); err != nil {
		s.log.Warn("read runner end dates", "err", err)
	}
	return s.warnEnds(ctx, arches)
}

// readEndDates reads the end date of each known runner version with the
// runner deprecations API (GitHub changelog, 3 September 2026). The API
// needs an organization or a repository, so it uses an installation.
func (s *Service) readEndDates(ctx context.Context) error {
	scope, a, ok := s.deprecationScope(ctx)
	if !ok {
		return nil
	}
	bundles, err := s.st.ListRunnerBundles(ctx)
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
		err := s.api.json(ctx, call{
			method: http.MethodGet, path: scope + "/actions/runners/deprecations/" + url.PathEscape(b.Version), auth: a,
		}, &d)
		if isStatus(err, http.StatusNotFound) {
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
				s.setBundleDates(ctx, x.Version, x.Arch, nil, end)
			}
		}
	}
	return errors.Join(errs...)
}

// earliest returns the earlier time. Each new sandbox registers a JIT
// runner and then runs a job, so the version ends at the first of the
// registration and runtime end dates.
func earliest(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil || a.Before(*b):
		return a
	}
	return b
}

// deprecationScope returns the REST path of an organization or repository
// that the app can read, with its installation. An organization comes
// first, because its answer does not depend on one repository.
func (s *Service) deprecationScope(ctx context.Context) (string, auth, bool) {
	insts, err := s.st.ListInstallations(ctx)
	if err != nil {
		return "", nil, false
	}
	var repoScope string
	var repoAuth auth
	for _, inst := range insts {
		switch {
		case inst.SuspendedAt != nil:
		case inst.AccountType == "Organization":
			return "/orgs/" + url.PathEscape(inst.AccountLogin), installAuth{s, inst.ID}, true
		case repoScope == "":
			if repos, err := s.st.ListRepositories(ctx, inst.ID); err == nil && len(repos) > 0 {
				repoScope, repoAuth = "/repos/"+repos[0].FullName, installAuth{s, inst.ID}
			}
		}
	}
	return repoScope, repoAuth, repoScope != ""
}

// setBundleDates writes the dates of one bundle row and publishes the row
// when it changed.
func (s *Service) setBundleDates(ctx context.Context, version, arch string, releasedAt, endDate *time.Time) {
	before, berr := s.st.GetRunnerBundle(ctx, version, arch)
	if err := s.st.SetRunnerBundleDates(ctx, version, arch, releasedAt, endDate); err != nil {
		s.log.Warn("record runner version", "version", version, "arch", arch, "err", err)
		return
	}
	after, err := s.st.GetRunnerBundle(ctx, version, arch)
	if err != nil {
		return
	}
	if berr == nil && sameTime(before.ReleasedAt, after.ReleasedAt) && sameTime(before.EndDate, after.EndDate) {
		return
	}
	s.bus.Publish(events.Event{Type: events.RunnerVersion, Data: after})
}

// warnEnds sends one notification for each runner version that new
// sandboxes use and that GitHub stops in less than 14 days.
func (s *Service) warnEnds(ctx context.Context, arches []string) error {
	settings, err := s.st.GetSettings(ctx)
	if err != nil {
		return err
	}
	for _, arch := range arches {
		b, err := s.st.LatestReadyBundle(ctx, arch)
		if err != nil || b.EndDate == nil || time.Until(*b.EndDate) > endWarning {
			continue
		}
		s.warnMu.Lock()
		sent := s.warned[b.Version]
		s.warnMu.Unlock()
		if sent {
			continue
		}
		s.log.Warn("runner version ends soon", "version", b.Version, "end_date", b.EndDate)
		if s.notify != nil {
			if err := s.notify.Notify(ctx, notify.RunnerDeprecationEvent(settings.PublicURL, b.Version, *b.EndDate)); err != nil {
				return fmt.Errorf("send runner end date warning: %w", err)
			}
		}
		s.warnMu.Lock()
		s.warned[b.Version] = true
		s.warnMu.Unlock()
	}
	return nil
}

func (s *Service) serverArches(ctx context.Context) ([]string, error) {
	servers, err := s.st.ListServers(ctx)
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
