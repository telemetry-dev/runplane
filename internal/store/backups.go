package store

import (
	"context"
	"time"
)

// Backup statuses.
const (
	BackupRunning   = "running"
	BackupDone      = "done"
	BackupUploading = "uploading"
	BackupUploaded  = "uploaded"
	BackupFailed    = "failed"
)

type Backup struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Path      string    `json:"-"`
	SizeBytes int64     `json:"size_bytes"`
	S3Key     string    `json:"s3_key"` // empty if not uploaded
	Status    string    `json:"status"`
	Error     string    `json:"error"`
}

const backupCols = "id, created_at, path, size_bytes, s3_key, status, error"

func (b *Backup) cols() []any {
	return []any{&b.ID, at(&b.CreatedAt), &b.Path, &b.SizeBytes, &b.S3Key, &b.Status, &b.Error}
}

func (s *Store) CreateBackup(ctx context.Context, b Backup) error {
	return exec(ctx, s.w, "create backup", insert("backups", backupCols), b.cols()...)
}

// UpdateBackup writes the path, size, S3 key, status, and error.
func (s *Store) UpdateBackup(ctx context.Context, b Backup) error {
	return exec1(ctx, s.w, "update backup",
		"UPDATE backups SET path = ?, size_bytes = ?, s3_key = ?, status = ?, error = ? WHERE id = ?",
		b.Path, b.SizeBytes, b.S3Key, b.Status, b.Error, b.ID)
}

func (s *Store) GetBackup(ctx context.Context, id string) (Backup, error) {
	return queryOne(ctx, s.r, "get backup", scanRow[Backup], "SELECT "+backupCols+" FROM backups WHERE id = ?", id)
}

// ListBackups returns the backups, newest first.
func (s *Store) ListBackups(ctx context.Context) ([]Backup, error) {
	return queryAll(ctx, s.r, "list backups", scanRow[Backup], "SELECT "+backupCols+" FROM backups ORDER BY created_at DESC, id DESC")
}

func (s *Store) DeleteBackup(ctx context.Context, id string) error {
	return exec1(ctx, s.w, "delete backup", "DELETE FROM backups WHERE id = ?", id)
}
