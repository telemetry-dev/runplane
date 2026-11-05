package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"

	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

// defaultRunnerGroup is the ID of the "Default" runner group. A free
// organization can only use this group, and a repository has only this one.
const defaultRunnerGroup = 1

// target is one scale set that runplane wants on GitHub.
type target struct {
	pool store.Pool
	inst store.Installation
	repo *store.Repository // nil for an organization
}

func (t target) key() string {
	var repo int64
	if t.repo != nil {
		repo = t.repo.ID
	}
	return setKey(t.pool.ID, t.inst.ID, repo)
}

func setKey(poolID string, installationID, repositoryID int64) string {
	return poolID + "/" + strconv.FormatInt(installationID, 10) + "/" + strconv.FormatInt(repositoryID, 10)
}

func rowKey(x store.ScaleSet) string {
	var repo int64
	if x.RepositoryID != nil {
		repo = *x.RepositoryID
	}
	return setKey(x.PoolID, x.InstallationID, repo)
}

// owner is the GitHub path of the runner owner: the organization login or
// the owner/name of the repository.
func (t target) owner() string {
	if t.repo != nil {
		return t.repo.FullName
	}
	return t.inst.AccountLogin
}

// runnersPath is the REST path of the self-hosted runners of the target.
func (t target) runnersPath() string {
	if t.repo != nil {
		return "/repos/" + t.repo.FullName + "/actions/runners"
	}
	return "/orgs/" + url.PathEscape(t.inst.AccountLogin) + "/actions/runners"
}

// world is the GitHub state in the store that decides the scale sets.
type world struct {
	insts map[int64]store.Installation
	repos map[int64]store.Repository // repositories of personal accounts
}

// reachable tells if runplane can still change the scale set on GitHub.
func (w world) reachable(x store.ScaleSet) bool {
	inst, ok := w.insts[x.InstallationID]
	if !ok || inst.SuspendedAt != nil {
		return false
	}
	if x.RepositoryID == nil {
		return true
	}
	repo, ok := w.repos[*x.RepositoryID]
	return ok && !repo.Archived
}

// targets returns the scale sets that the pools, installations, and
// settings ask for, by key.
func (s *Service) targets(ctx context.Context) (map[string]target, world, error) {
	w := world{insts: map[int64]store.Installation{}, repos: map[int64]store.Repository{}}
	pools, err := s.st.ListPools(ctx)
	if err != nil {
		return nil, w, err
	}
	insts, err := s.st.ListInstallations(ctx)
	if err != nil {
		return nil, w, err
	}
	settings, err := s.st.GetSettings(ctx)
	if err != nil {
		return nil, w, err
	}
	// The scheduler takes a public job only if each runner of its pool is in
	// a VM. The setting mode does not tell that: each server runs its own
	// mode, and a mode change applies after a restart.
	public := settings.AllowPublicRepos
	byInst := map[int64][]store.Repository{}
	for _, inst := range insts {
		w.insts[inst.ID] = inst
		if inst.AccountType != "User" {
			continue
		}
		repos, err := s.st.ListRepositories(ctx, inst.ID)
		if err != nil {
			return nil, w, err
		}
		byInst[inst.ID] = repos
		for _, r := range repos {
			w.repos[r.ID] = r
		}
	}
	out := map[string]target{}
	for _, pool := range pools {
		if !pool.Enabled {
			continue
		}
		for _, inst := range insts {
			if inst.AccountType == "Organization" {
				t := target{pool: pool, inst: inst}
				out[t.key()] = t
				continue
			}
			for _, r := range byInst[inst.ID] {
				if r.Archived || (!r.Private && !public) {
					continue
				}
				t := target{pool: pool, inst: inst, repo: &r}
				out[t.key()] = t
			}
		}
	}
	return out, w, nil
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
	if _, err := s.creds(ctx); errors.Is(err, ErrNotConnected) {
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
	want, w, err := s.targets(ctx)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, row := range rows {
		t, ok := want[rowKey(row)]
		if !ok {
			s.removeSet(ctx, row, w.reachable(row))
			continue
		}
		have[rowKey(row)] = true
		s.keepSet(ctx, row, t)
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		if !have[k] {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		t := want[k]
		row := store.ScaleSet{
			ID: ids.New(), PoolID: t.pool.ID, InstallationID: t.inst.ID, Name: t.pool.Label,
			Status: store.ScaleSetCreating, UpdatedAt: time.Now(),
		}
		if t.repo != nil {
			row.RepositoryID = &t.repo.ID
		}
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
		msg := "GitHub suspended the app installation on " + t.inst.AccountLogin +
			". runplane listens again when the installation is active."
		if row.Status != store.ScaleSetError || row.Error != msg {
			s.setStatus(ctx, row.ID, store.ScaleSetError, msg)
		}
		return
	}
	l := s.listener(row.ID)
	if l == nil {
		if l = s.startListener(ctx, row, t); l == nil {
			return
		}
	}
	if row.Name != t.pool.Label {
		s.rename(ctx, l, row, t.pool.Label)
	}
}

func (s *Service) rename(ctx context.Context, l *setListener, row store.ScaleSet, label string) {
	if row.GitHubID != 0 {
		_, err := l.client().UpdateRunnerScaleSet(ctx, int(row.GitHubID), scaleSetSpec(label, int(row.RunnerGroupID)))
		if err != nil {
			s.setStatus(ctx, row.ID, store.ScaleSetError, "rename the scale set to "+label+" on GitHub: "+err.Error())
			return
		}
	}
	if err := s.st.SetScaleSetRemote(ctx, row.ID, row.GitHubID, row.RunnerGroupID, label); err != nil {
		s.log.Warn("rename scale set", "scale_set", row.ID, "err", err)
		return
	}
	l.setLabel(label)
}

// removeSet stops the listener and deletes the scale set on GitHub, then
// the row. When runplane can no longer reach the scale set (installation or
// repository gone), it only deletes the row.
func (s *Service) removeSet(ctx context.Context, row store.ScaleSet, reachable bool) {
	s.stopListener(row.ID)
	if reachable {
		if row.Status != store.ScaleSetDeleting {
			s.setStatus(ctx, row.ID, store.ScaleSetDeleting, "")
		}
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

// rowTarget reads the installation and repository of a scale set row.
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
		scaleset.WithHTTPClient(s.http),
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
// scale set label and a name that starts with "<label>-", the form that the
// scheduler gives runner names.
func (s *Service) listRunners(ctx context.Context, t target, label string) ([]scheduler.Runner, error) {
	all, err := list[runner](ctx, s.api, installAuth{s, t.inst.ID}, t.runnersPath()+"?per_page=100", "runners")
	if err != nil {
		return nil, err
	}
	var out []scheduler.Runner
	for _, r := range all {
		if inScaleSet(r, label) {
			out = append(out, scheduler.Runner{ID: r.ID, Name: r.Name, Busy: r.Busy})
		}
	}
	return out, nil
}

func inScaleSet(r runner, label string) bool {
	if !strings.HasPrefix(strings.ToLower(r.Name), strings.ToLower(label)+"-") {
		return false
	}
	return slices.ContainsFunc(r.Labels, func(l runnerLabel) bool { return strings.EqualFold(l.Name, label) })
}

// removeRunnerErr maps the errors of RemoveRunner to the scheduler contract.
func removeRunnerErr(err error) error {
	if err == nil || errors.Is(err, scaleset.RunnerNotFoundError) || errors.Is(err, scaleset.NotFoundError) {
		return nil
	}
	if errors.Is(err, scaleset.JobStillRunningError) {
		return fmt.Errorf("%w: %v", scheduler.ErrRunnerBusy, err)
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "is busy") || strings.Contains(msg, "job in progress") || strings.Contains(msg, "currently running a job") {
		return fmt.Errorf("%w: %v", scheduler.ErrRunnerBusy, err)
	}
	return fmt.Errorf("remove runner: %w", err)
}

func (s *Service) setStatus(ctx context.Context, id, status, msg string) {
	if err := s.st.SetScaleSetStatus(ctx, id, status, truncate(msg, 1000)); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("set scale set status", "scale_set", id, "err", err)
		}
		return
	}
	s.changed()
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
