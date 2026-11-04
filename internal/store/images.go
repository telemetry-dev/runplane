package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Image struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Ref       string    `json:"ref"` // OCI reference
	IsDefault bool      `json:"is_default"`
	Source    string    `json:"source"` // registry | builtin
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Image build statuses.
const (
	BuildPending    = "pending"
	BuildPulling    = "pulling"
	BuildBuilding   = "building"
	BuildConverting = "converting"
	BuildReady      = "ready"
	BuildFailed     = "failed"
)

// ImageBuild is the state of one image on one server.
type ImageBuild struct {
	ImageID   string    `json:"image_id"`
	ServerID  string    `json:"server_id"`
	Digest    string    `json:"digest"`
	Status    string    `json:"status"`
	Error     string    `json:"error"`
	SizeBytes int64     `json:"size_bytes"`
	Path      string    `json:"path"` // ext4 root disk in VM mode
	UpdatedAt time.Time `json:"updated_at"`
}

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

const imageCols = "id, name, ref, is_default, source, created_at, updated_at"

func scanImage(sc scanner) (Image, error) {
	var i Image
	err := sc.Scan(&i.ID, &i.Name, &i.Ref, &i.IsDefault, &i.Source, at(&i.CreatedAt), at(&i.UpdatedAt))
	return i, err
}

// CreateImage returns ErrConflict if an image has the same ref. If the new
// image is the default, the earlier default stops being the default.
func (s *Store) CreateImage(ctx context.Context, i Image) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if i.IsDefault {
			if _, err := tx.ExecContext(ctx, "UPDATE images SET is_default = 0 WHERE is_default = 1"); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO images ("+imageCols+") VALUES ("+placeholders(7)+")",
			i.ID, i.Name, i.Ref, i.IsDefault, i.Source, ms(i.CreatedAt), ms(i.UpdatedAt))
		return dbErr(err)
	})
	if err != nil {
		return fmt.Errorf("create image: %w", err)
	}
	return nil
}

func (s *Store) GetImage(ctx context.Context, id string) (Image, error) {
	i, err := scanImage(s.r.QueryRowContext(ctx, "SELECT "+imageCols+" FROM images WHERE id = ?", id))
	if err != nil {
		return Image{}, fmt.Errorf("get image: %w", dbErr(err))
	}
	return i, nil
}

// GetDefaultImage returns ErrNotFound if no image is the default.
func (s *Store) GetDefaultImage(ctx context.Context) (Image, error) {
	i, err := scanImage(s.r.QueryRowContext(ctx, "SELECT "+imageCols+" FROM images WHERE is_default = 1"))
	if err != nil {
		return Image{}, fmt.Errorf("get default image: %w", dbErr(err))
	}
	return i, nil
}

func (s *Store) ListImages(ctx context.Context) ([]Image, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+imageCols+" FROM images ORDER BY is_default DESC, name, id")
	list, err := scanAll(rows, err, scanImage)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	return list, nil
}

// UpdateImage writes the name and the ref.
func (s *Store) UpdateImage(ctx context.Context, i Image) error {
	err := exec1(ctx, s.w, "UPDATE images SET name = ?, ref = ?, updated_at = ? WHERE id = ?",
		i.Name, i.Ref, ms(time.Now()), i.ID)
	if err != nil {
		return fmt.Errorf("update image: %w", err)
	}
	return nil
}

func (s *Store) SetDefaultImage(ctx context.Context, id string) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE images SET is_default = 0 WHERE is_default = 1 AND id != ?", id); err != nil {
			return err
		}
		return exec1(ctx, tx, "UPDATE images SET is_default = 1, updated_at = ? WHERE id = ?", ms(time.Now()), id)
	})
	if err != nil {
		return fmt.Errorf("set default image: %w", err)
	}
	return nil
}

// DeleteImage also deletes its builds. Pools with the image change to the default image.
func (s *Store) DeleteImage(ctx context.Context, id string) error {
	if err := exec1(ctx, s.w, "DELETE FROM images WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete image: %w", err)
	}
	return nil
}

const imageBuildCols = "image_id, server_id, digest, status, error, size_bytes, path, updated_at"

func scanImageBuild(sc scanner) (ImageBuild, error) {
	var b ImageBuild
	err := sc.Scan(&b.ImageID, &b.ServerID, &b.Digest, &b.Status, &b.Error, &b.SizeBytes, &b.Path, at(&b.UpdatedAt))
	return b, err
}

func (s *Store) UpsertImageBuild(ctx context.Context, b ImageBuild) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO image_builds (`+imageBuildCols+`) VALUES (`+placeholders(8)+`)
		ON CONFLICT (image_id, server_id) DO UPDATE SET digest = excluded.digest, status = excluded.status,
			error = excluded.error, size_bytes = excluded.size_bytes, path = excluded.path, updated_at = excluded.updated_at`,
		b.ImageID, b.ServerID, b.Digest, b.Status, b.Error, b.SizeBytes, b.Path, ms(b.UpdatedAt))
	if err != nil {
		return fmt.Errorf("save image build: %w", dbErr(err))
	}
	return nil
}

func (s *Store) GetImageBuild(ctx context.Context, imageID, serverID string) (ImageBuild, error) {
	b, err := scanImageBuild(s.r.QueryRowContext(ctx,
		"SELECT "+imageBuildCols+" FROM image_builds WHERE image_id = ? AND server_id = ?", imageID, serverID))
	if err != nil {
		return ImageBuild{}, fmt.Errorf("get image build: %w", dbErr(err))
	}
	return b, nil
}

// ListImageBuilds returns the builds of all images on all servers.
func (s *Store) ListImageBuilds(ctx context.Context) ([]ImageBuild, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+imageBuildCols+" FROM image_builds ORDER BY image_id, server_id")
	list, err := scanAll(rows, err, scanImageBuild)
	if err != nil {
		return nil, fmt.Errorf("list image builds: %w", err)
	}
	return list, nil
}

const bundleCols = "version, arch, sha256, patched, status, path, error, end_date, released_at, created_at"

func scanBundle(sc scanner) (RunnerBundle, error) {
	var b RunnerBundle
	err := sc.Scan(&b.Version, &b.Arch, &b.SHA256, &b.Patched, &b.Status, &b.Path, &b.Error,
		atPtr(&b.EndDate), atPtr(&b.ReleasedAt), at(&b.CreatedAt))
	return b, err
}

// UpsertRunnerBundle keeps the created_at of a known bundle.
func (s *Store) UpsertRunnerBundle(ctx context.Context, b RunnerBundle) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO runner_bundles (`+bundleCols+`) VALUES (`+placeholders(10)+`)
		ON CONFLICT (version, arch) DO UPDATE SET sha256 = excluded.sha256, patched = excluded.patched,
			status = excluded.status, path = excluded.path, error = excluded.error, end_date = excluded.end_date,
			released_at = excluded.released_at`,
		b.Version, b.Arch, b.SHA256, b.Patched, b.Status, b.Path, b.Error, msPtr(b.EndDate), msPtr(b.ReleasedAt), ms(b.CreatedAt))
	if err != nil {
		return fmt.Errorf("save runner bundle: %w", dbErr(err))
	}
	return nil
}

func (s *Store) GetRunnerBundle(ctx context.Context, version, arch string) (RunnerBundle, error) {
	b, err := scanBundle(s.r.QueryRowContext(ctx,
		"SELECT "+bundleCols+" FROM runner_bundles WHERE version = ? AND arch = ?", version, arch))
	if err != nil {
		return RunnerBundle{}, fmt.Errorf("get runner bundle: %w", dbErr(err))
	}
	return b, nil
}

// ListRunnerBundles returns the bundles, newest version first.
func (s *Store) ListRunnerBundles(ctx context.Context) ([]RunnerBundle, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+bundleCols+" FROM runner_bundles")
	list, err := scanAll(rows, err, scanBundle)
	if err != nil {
		return nil, fmt.Errorf("list runner bundles: %w", err)
	}
	sortBundles(list)
	return list, nil
}

// LatestReadyBundle returns the ready bundle with the highest version for
// arch, or ErrNotFound.
func (s *Store) LatestReadyBundle(ctx context.Context, arch string) (RunnerBundle, error) {
	rows, err := s.r.QueryContext(ctx,
		"SELECT "+bundleCols+" FROM runner_bundles WHERE arch = ? AND status = ?", arch, BundleReady)
	list, err := scanAll(rows, err, scanBundle)
	if err != nil {
		return RunnerBundle{}, fmt.Errorf("get latest runner bundle: %w", err)
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
		if c := compareVersions(b.Version, a.Version); c != 0 {
			return c
		}
		return strings.Compare(a.Arch, b.Arch)
	})
}

// compareVersions compares dotted numbers such as "2.330.0", so "2.330.0" > "2.99.0".
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if c := cmp.Compare(x, y); c != 0 {
			return c
		}
	}
	return 0
}
