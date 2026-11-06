package store

import (
	"context"
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

// RunnerBundle is a runner version for one arch. The runner versions
// service (internal/runner/versions) writes these rows.
type RunnerBundle struct {
	Version    string     `json:"version"`
	Arch       string     `json:"arch"`
	SHA256     string     `json:"sha256"`
	Patched    bool       `json:"patched"` // the cache patch applied
	Status     string     `json:"status"`
	Error      string     `json:"error"`
	EndDate    *time.Time `json:"end_date"` // GitHub stops jobs for this version after this time
	ReleasedAt *time.Time `json:"released_at"`
	Warned     bool       `json:"warned"` // the end date warning of the version went out
	CreatedAt  time.Time  `json:"created_at"`
}

// EndsSoon reports if GitHub stops the version in less than 14 days. Then
// runplane warns about the version, and the dashboard marks it.
func (b RunnerBundle) EndsSoon(now time.Time) bool {
	return b.EndDate != nil && b.EndDate.Sub(now) < 14*24*time.Hour
}

const bundleCols = "version, arch, sha256, patched, status, error, end_date, released_at, warned, created_at"

func (b *RunnerBundle) cols() []any {
	return []any{&b.Version, &b.Arch, &b.SHA256, &b.Patched, &b.Status, &b.Error, atPtr(&b.EndDate),
		atPtr(&b.ReleasedAt), &b.Warned, at(&b.CreatedAt)}
}

// SetRunnerBundleBuild records the result of a bundle build on one server
// at now: sha256, patched, status, and error. A failure does not change a
// bundle that is ready, because it can be ready on other servers. The dates
// stay.
func (s *Store) SetRunnerBundleBuild(ctx context.Context, b RunnerBundle, now time.Time) error {
	return exec(ctx, s.w, "set runner bundle build", `INSERT INTO runner_bundles
			(version, arch, sha256, patched, status, error, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (version, arch) DO UPDATE SET sha256 = excluded.sha256, patched = excluded.patched,
			status = excluded.status, error = excluded.error
		WHERE excluded.status = 'ready' OR runner_bundles.status != 'ready'`,
		b.Version, b.Arch, b.SHA256, b.Patched, b.Status, b.Error, ms(now))
}

// SetRunnerBundleDates writes the release time and the end date of a runner
// version at now. A new row gets the pending status. A nil time keeps the
// stored value, and the build columns never change, so this cannot undo the
// work of the bundle builder.
func (s *Store) SetRunnerBundleDates(ctx context.Context, version, arch string, releasedAt, endDate *time.Time, now time.Time) error {
	return exec(ctx, s.w, "set runner bundle dates", `INSERT INTO runner_bundles
			(version, arch, status, released_at, end_date, created_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (version, arch) DO UPDATE SET
			released_at = coalesce(excluded.released_at, released_at),
			end_date = coalesce(excluded.end_date, end_date)`,
		version, arch, BundlePending, msPtr(releasedAt), msPtr(endDate), ms(now))
}

// SetRunnerBundleWarned records that the end date warning of a version went
// out, for the rows of all arches.
func (s *Store) SetRunnerBundleWarned(ctx context.Context, version string) error {
	return exec(ctx, s.w, "set runner bundle warned", "UPDATE runner_bundles SET warned = 1 WHERE version = ?", version)
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

// sortBundles sorts by version, highest first, then by arch.
func sortBundles(list []RunnerBundle) {
	slices.SortFunc(list, func(a, b RunnerBundle) int {
		if c := version.Compare(b.Version, a.Version); c != 0 {
			return c
		}
		return strings.Compare(a.Arch, b.Arch)
	})
}
