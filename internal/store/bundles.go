package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/version"
)

// RunnerBundle statuses.
const (
	BundlePending = "pending"
	BundleReady   = "ready"
	BundleFailed  = "failed"
)

type RunnerBundle struct {
	Version    string     `json:"version"`
	Arch       string     `json:"arch"`
	SHA256     string     `json:"sha256"`
	Patched    bool       `json:"patched"` // the cache patch applied
	Status     string     `json:"status"`
	Path       string     `json:"path"`
	Error      string     `json:"error"`
	EndDate    *time.Time `json:"end_date"` // GitHub stops jobs for this version after this time
	ReleasedAt *time.Time `json:"released_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

const bundleCols = "version, arch, sha256, patched, status, path, error, end_date, released_at, created_at"

func (b *RunnerBundle) cols() []any {
	return []any{&b.Version, &b.Arch, &b.SHA256, &b.Patched, &b.Status, &b.Path, &b.Error, atPtr(&b.EndDate),
		atPtr(&b.ReleasedAt), at(&b.CreatedAt)}
}

// UpsertRunnerBundle keeps the created_at of a known bundle.
func (s *Store) UpsertRunnerBundle(ctx context.Context, b RunnerBundle) error {
	return exec(ctx, s.w, "save runner bundle", upsert("runner_bundles", bundleCols, "version, arch"), b.cols()...)
}

// SetRunnerBundleBuild records the result of a bundle build on one server:
// sha256, patched, status, path, and error. A failure does not change a
// bundle that is ready, because it can be ready on other servers. The dates
// stay.
func (s *Store) SetRunnerBundleBuild(ctx context.Context, b RunnerBundle) error {
	return exec(ctx, s.w, "set runner bundle build", `INSERT INTO runner_bundles
			(version, arch, sha256, patched, status, path, error, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (version, arch) DO UPDATE SET sha256 = excluded.sha256, patched = excluded.patched,
			status = excluded.status, path = excluded.path, error = excluded.error
		WHERE excluded.status = 'ready' OR runner_bundles.status != 'ready'`,
		b.Version, b.Arch, b.SHA256, b.Patched, b.Status, b.Path, b.Error, ms(time.Now()))
}

// SetRunnerBundleDates writes the release time and the end date of a runner
// version. A new row gets the pending status. A nil time keeps the stored
// value, and the build columns never change, so this cannot undo the work of
// the bundle builder.
func (s *Store) SetRunnerBundleDates(ctx context.Context, version, arch string, releasedAt, endDate *time.Time) error {
	return exec(ctx, s.w, "set runner bundle dates", `INSERT INTO runner_bundles
			(version, arch, status, released_at, end_date, created_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (version, arch) DO UPDATE SET
			released_at = coalesce(excluded.released_at, released_at),
			end_date = coalesce(excluded.end_date, end_date)`,
		version, arch, BundlePending, msPtr(releasedAt), msPtr(endDate), ms(time.Now()))
}

func (s *Store) GetRunnerBundle(ctx context.Context, version, arch string) (RunnerBundle, error) {
	return queryOne(ctx, s.r, "get runner bundle", scanRow[RunnerBundle],
		"SELECT "+bundleCols+" FROM runner_bundles WHERE version = ? AND arch = ?", version, arch)
}

// ListRunnerBundles returns the bundles, newest version first.
func (s *Store) ListRunnerBundles(ctx context.Context) ([]RunnerBundle, error) {
	list, err := queryAll(ctx, s.r, "list runner bundles", scanRow[RunnerBundle], "SELECT "+bundleCols+" FROM runner_bundles")
	sortBundles(list)
	return list, err
}

// LatestReadyBundle returns the ready bundle with the highest version for
// arch, or ErrNotFound.
func (s *Store) LatestReadyBundle(ctx context.Context, arch string) (RunnerBundle, error) {
	list, err := queryAll(ctx, s.r, "get latest runner bundle", scanRow[RunnerBundle],
		"SELECT "+bundleCols+" FROM runner_bundles WHERE arch = ? AND status = ?", arch, BundleReady)
	if err != nil {
		return RunnerBundle{}, err
	}
	if len(list) == 0 {
		return RunnerBundle{}, fmt.Errorf("get latest runner bundle: %w", ErrNotFound)
	}
	sortBundles(list)
	return list[0], nil
}

// sortBundles sorts by version, highest first, then by arch.
func sortBundles(list []RunnerBundle) {
	slices.SortFunc(list, func(a, b RunnerBundle) int {
		if c := version.Compare(b.Version, a.Version); c != 0 {
			return c
		}
		return strings.Compare(a.Arch, b.Arch)
	})
}
