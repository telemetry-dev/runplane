package store

import (
	"context"
	"database/sql"
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
	UpdatedAt time.Time `json:"updated_at"`
}

const imageCols = "id, name, ref, is_default, source, created_at, updated_at"

func (i *Image) cols() []any {
	return []any{&i.ID, &i.Name, &i.Ref, &i.IsDefault, &i.Source, at(&i.CreatedAt), at(&i.UpdatedAt)}
}

// CreateImage returns ErrConflict if an image has the same ref. If the new
// image is the default, the earlier default stops being the default.
func (s *Store) CreateImage(ctx context.Context, i Image) error {
	return s.tx(ctx, "create image", func(tx *sql.Tx) error {
		if i.IsDefault {
			if _, err := tx.ExecContext(ctx, "UPDATE images SET is_default = 0 WHERE is_default = 1"); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, insert("images", imageCols), i.cols()...)
		return dbErr(err)
	})
}

func (s *Store) GetImage(ctx context.Context, id string) (Image, error) {
	return queryOne(ctx, s.r, "get image", scanRow[Image], "SELECT "+imageCols+" FROM images WHERE id = ?", id)
}

func (s *Store) ListImages(ctx context.Context) ([]Image, error) {
	return queryAll(ctx, s.r, "list images", scanRow[Image], "SELECT "+imageCols+" FROM images ORDER BY is_default DESC, name, id")
}

// UpdateImage writes the name, the ref, and the update time.
func (s *Store) UpdateImage(ctx context.Context, i Image) error {
	return exec1(ctx, s.w, "update image", "UPDATE images SET name = ?, ref = ?, updated_at = ? WHERE id = ?",
		i.Name, i.Ref, ms(i.UpdatedAt), i.ID)
}

func (s *Store) SetDefaultImage(ctx context.Context, id string, now time.Time) error {
	return s.tx(ctx, "set default image", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE images SET is_default = 0 WHERE is_default = 1 AND id != ?", id); err != nil {
			return err
		}
		return exec1(ctx, tx, "mark default", "UPDATE images SET is_default = 1, updated_at = ? WHERE id = ?", ms(now), id)
	})
}

// DeleteImage also deletes its builds. Pools with the image change to the default image.
func (s *Store) DeleteImage(ctx context.Context, id string) error {
	return exec1(ctx, s.w, "delete image", "DELETE FROM images WHERE id = ?", id)
}

const imageBuildCols = "image_id, server_id, digest, status, error, size_bytes, updated_at"

func (b *ImageBuild) cols() []any {
	return []any{&b.ImageID, &b.ServerID, &b.Digest, &b.Status, &b.Error, &b.SizeBytes, at(&b.UpdatedAt)}
}

func (s *Store) UpsertImageBuild(ctx context.Context, b ImageBuild) error {
	return exec(ctx, s.w, "save image build", upsert("image_builds", imageBuildCols, "image_id, server_id"), b.cols()...)
}

func (s *Store) GetImageBuild(ctx context.Context, imageID, serverID string) (ImageBuild, error) {
	return queryOne(ctx, s.r, "get image build", scanRow[ImageBuild],
		"SELECT "+imageBuildCols+" FROM image_builds WHERE image_id = ? AND server_id = ?", imageID, serverID)
}

func (s *Store) ListImageBuilds(ctx context.Context) ([]ImageBuild, error) {
	return queryAll(ctx, s.r, "list image builds", scanRow[ImageBuild],
		"SELECT "+imageBuildCols+" FROM image_builds ORDER BY image_id, server_id")
}
