package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/actions/scaleset"

	"github.com/runplane/runplane/internal/ghapp"
	"github.com/runplane/runplane/internal/ghrest"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/policy"
	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

// target is one scale set that runplane wants on GitHub. An organization
// has one for each pool, and a personal account one for each pool and
// repository.
type target struct {
	pool store.Pool
	inst store.Installation
	repo *store.Repository // nil for an organization
}

// row returns the scale set row of the target, without ID and status.
func (t target) row() store.ScaleSet {
	x := store.ScaleSet{PoolID: t.pool.ID, InstallationID: t.inst.ID, Name: t.pool.Label}
	if t.repo != nil {
		x.RepositoryID = &t.repo.ID
	}
	return x
}

// rowKey identifies a scale set by pool, installation, and repository, like
// the unique index of the rows.
func rowKey(x store.ScaleSet) string {
	var repo int64
	if x.RepositoryID != nil {
		repo = *x.RepositoryID
	}
	return fmt.Sprintf("%s/%d/%d", x.PoolID, x.InstallationID, repo)
}

// owner is the GitHub path of the runner owner: the organization login or
// the owner/name of the repository.
func (t target) owner() string {
	if t.repo != nil {
		return t.repo.FullName
	}
	return t.inst.AccountLogin
}

// path is the REST path of the runner owner.
func (t target) path() string {
	if t.repo != nil {
		return "/repos/" + t.repo.FullName
	}
	return "/orgs/" + url.PathEscape(t.inst.AccountLogin)
}

// targets returns the scale sets that the pools, installations, and
// settings ask for, by key.
func (s *Service) targets(ctx context.Context) (map[string]target, error) {
	pools, err := s.st.ListPools(ctx)
	if err != nil {
		return nil, err
	}
	insts, err := s.st.ListInstallations(ctx)
	if err != nil {
		return nil, err
	}
	settings, err := s.st.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	byInst := map[int64][]store.Repository{}
	for _, inst := range insts {
		if inst.AccountType != ghapp.User {
			continue
		}
		repos, err := s.st.ListRepositories(ctx, inst.ID)
		if err != nil {
			return nil, err
		}
		byInst[inst.ID] = repos
	}
	out := map[string]target{}
	for _, pool := range pools {
		if !pool.Enabled {
			continue
		}
		for _, inst := range insts {
			if inst.AccountType == ghapp.Organization {
				t := target{pool: pool, inst: inst}
				out[rowKey(t.row())] = t
				continue
			}
			// A repository whose jobs runplane refuses gets no scale set.
			for _, r := range byInst[inst.ID] {
				if !policy.TakesJobs(settings, r) {
					continue
				}
				t := target{pool: pool, inst: inst, repo: &r}
				out[rowKey(t.row())] = t
			}
		}
	}
	return out, nil
}

// reconcile makes the scale sets and their listeners agree with the
// targets. Errors go to the scale set rows. s.ops must be held.
func (s *Service) reconcile(ctx context.Context) {
	if err := s.reconcileSets(ctx); err != nil {
		s.log.Warn("update scale sets", "err", err)
	}
	s.changed()
}

func (s *Service) reconcileSets(ctx context.Context) error {
	rows, err := s.st.ListScaleSets(ctx)
	if err != nil {
		return err
	}
	if _, err := s.creds(ctx); errors.Is(err, errNotConnected) {
		for _, row := range rows {
			s.stopListener(row.ID)
			if err := s.st.DeleteScaleSet(ctx, row.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return nil
	} else if err != nil {
		return err
	}
	want, err := s.targets(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		t, ok := want[rowKey(row)]
		if !ok {
			s.removeSet(ctx, row)
			continue
		}
		delete(want, rowKey(row))
		s.keepSet(ctx, row, t)
	}
	for _, k := range slices.Sorted(maps.Keys(want)) {
		t := want[k]
		row := t.row()
		row.ID, row.Status, row.UpdatedAt = ids.New(), store.ScaleSetCreating, time.Now()
		if err := s.st.UpsertScaleSet(ctx, row); err != nil {
			return err
		}
		s.keepSet(ctx, row, t)
	}
	return nil
}

// keepSet makes a wanted scale set listen, pauses it while its installation
// is suspended, and renames it after a label change.
func (s *Service) keepSet(ctx context.Context, row store.ScaleSet, t target) {
	if t.inst.SuspendedAt != nil {
		s.stopListener(row.ID)
		s.setStatus(ctx, row.ID, store.ScaleSetError, "GitHub suspended the app installation on "+t.inst.AccountLogin+
			". runplane listens again when the installation is active.")
		return
	}
	s.lmu.Lock()
	l := s.listeners[row.ID]
	s.lmu.Unlock()
	if l == nil {
		if l = s.startListener(ctx, row, t); l == nil {
			return
		}
	}
	if row.Name != t.pool.Label {
		l.rename(ctx, row, t.pool.Label)
	}
}

// removeSet stops the listener and deletes the scale set on GitHub, then
// the row. When runplane can no longer reach the scale set (installation or
// repository gone, suspended, or archived), it only deletes the row.
func (s *Service) removeSet(ctx context.Context, row store.ScaleSet) {
	s.stopListener(row.ID)
	t, err := s.rowTarget(ctx, row)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("delete scale set", "scale_set", row.ID, "err", err)
		return
	}
	if err == nil && t.inst.SuspendedAt == nil && (t.repo == nil || !t.repo.Archived) {
		s.setStatus(ctx, row.ID, store.ScaleSetDeleting, "")
		if err := s.deleteRemote(ctx, row.ID); err != nil {
			s.setStatus(ctx, row.ID, store.ScaleSetDeleting, "delete the scale set on GitHub: "+err.Error())
			return
		}
	}
	if err := s.st.DeleteScaleSet(ctx, row.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("delete scale set row", "scale_set", row.ID, "err", err)
	}
}

// deleteRemote removes the idle runners of the scale set and then the
// scale set on GitHub. Call it after the listener stops: it reads the row
// again for the GitHub ID that the listener may have written.
func (s *Service) deleteRemote(ctx context.Context, id string) error {
	row, err := s.st.GetScaleSet(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.GitHubID == 0 {
		return nil
	}
	t, err := s.rowTarget(ctx, row)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	cl, err := s.scalesetClient(ctx, t)
	if err != nil {
		return err
	}
	runners, err := s.listRunners(ctx, t, row.Name)
	if err != nil {
		return err
	}
	for _, r := range runners {
		if r.Busy {
			continue
		}
		if err := removeRunnerErr(cl.RemoveRunner(ctx, r.ID)); err != nil && !errors.Is(err, scheduler.ErrRunnerBusy) {
			return err
		}
	}
	err = cl.DeleteRunnerScaleSet(ctx, int(row.GitHubID))
	if errors.Is(err, scaleset.NotFoundError) {
		return nil
	}
	return err
}

func (s *Service) rowTarget(ctx context.Context, row store.ScaleSet) (target, error) {
	inst, err := s.st.GetInstallation(ctx, row.InstallationID)
	if err != nil {
		return target{}, err
	}
	t := target{pool: store.Pool{ID: row.PoolID, Label: row.Name}, inst: inst}
	if row.RepositoryID == nil {
		return t, nil
	}
	repos, err := s.st.ListRepositories(ctx, inst.ID)
	if err != nil {
		return target{}, err
	}
	i := slices.IndexFunc(repos, func(r store.Repository) bool { return r.ID == *row.RepositoryID })
	if i < 0 {
		return target{}, fmt.Errorf("repository %d of scale set %s: %w", *row.RepositoryID, row.Name, store.ErrNotFound)
	}
	t.repo = &repos[i]
	return t, nil
}

func (s *Service) scalesetClient(ctx context.Context, t target) (*scaleset.Client, error) {
	c, err := s.creds(ctx)
	if err != nil {
		return nil, err
	}
	return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: s.web + "/" + t.owner(),
		GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: c.issuer(), InstallationID: t.inst.ID, PrivateKey: c.pem},
		SystemInfo: scaleset.SystemInfo{
			System: "runplane", Version: version.Version, CommitSHA: version.Commit, Subsystem: "listener",
		},
	},
		// The scaleset library has no timeout of its own except for the
		// long poll, so its client gets one.
		scaleset.WithHTTPClient(&http.Client{Timeout: 2 * time.Minute}),
		scaleset.WithLogger(slog.New(quiet{s.log.Handler()}).With("scale_set", t.pool.Label, "owner", t.owner())),
	)
}

func scaleSetSpec(label string, runnerGroupID int) *scaleset.RunnerScaleSet {
	return &scaleset.RunnerScaleSet{
		Name:          label,
		RunnerGroupID: runnerGroupID,
		Labels:        []scaleset.Label{{Name: label, Type: "System"}},
		// runplane brings new runner versions in new sandboxes.
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}
}

type runner struct {
	ID     int64         `json:"id"`
	Name   string        `json:"name"`
	Busy   bool          `json:"busy"`
	Labels []runnerLabel `json:"labels"`
}

type runnerLabel struct {
	Name string `json:"name"`
}

// listRunners returns the runners of one scale set. The REST runner object
// has no scale set ID, so a runner belongs to the scale set when it has the
// scale set label and its name scheduler.IsRunnerOf the label.
func (s *Service) listRunners(ctx context.Context, t target, label string) ([]scheduler.Runner, error) {
	all, err := ghrest.List[runner](ctx, s.api, installAuth{s, t.inst.ID}, t.path()+"/actions/runners?per_page=100", "runners")
	if err != nil {
		return nil, err
	}
	hasLabel := func(l runnerLabel) bool { return strings.EqualFold(l.Name, label) }
	var out []scheduler.Runner
	for _, r := range all {
		if scheduler.IsRunnerOf(r.Name, label) && slices.ContainsFunc(r.Labels, hasLabel) {
			out = append(out, scheduler.Runner{ID: r.ID, Name: r.Name, Busy: r.Busy})
		}
	}
	return out, nil
}

// removeRunnerErr maps the errors of RemoveRunner to the scheduler contract.
func removeRunnerErr(err error) error {
	if err == nil || errors.Is(err, scaleset.RunnerNotFoundError) || errors.Is(err, scaleset.NotFoundError) {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if errors.Is(err, scaleset.JobStillRunningError) ||
		strings.Contains(msg, "is busy") || strings.Contains(msg, "job in progress") || strings.Contains(msg, "currently running a job") {
		return fmt.Errorf("%w: %v", scheduler.ErrRunnerBusy, err)
	}
	return fmt.Errorf("remove runner: %w", err)
}

// setStatus writes the status and the error of a scale set row, and tells
// if they changed. The listener of the row calls it through report; the
// service calls it only for a row without a listener.
func (s *Service) setStatus(ctx context.Context, id, status, msg string) bool {
	changed, err := s.st.SetScaleSetStatus(ctx, id, status, truncate(msg, 1000), time.Now())
	if err != nil {
		s.log.Warn("set scale set status", "scale_set", id, "err", err)
		return false
	}
	if changed {
		s.changed()
	}
	return changed
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
