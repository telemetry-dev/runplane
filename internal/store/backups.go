package store

import (
	"context"
	"fmt"
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

func scanBackup(sc scanner) (Backup, error) {
	var b Backup
	err := sc.Scan(&b.ID, at(&b.CreatedAt), &b.Path, &b.SizeBytes, &b.S3Key, &b.Status, &b.Error)
	return b, err
}

func (s *Store) CreateBackup(ctx context.Context, b Backup) error {
	_, err := s.w.ExecContext(ctx, "INSERT INTO backups ("+backupCols+") VALUES ("+placeholders(7)+")",
		b.ID, ms(b.CreatedAt), b.Path, b.SizeBytes, b.S3Key, b.Status, b.Error)
	if err != nil {
		return fmt.Errorf("create backup: %w", dbErr(err))
	}
	return nil
}

// UpdateBackup writes the path, size, S3 key, status, and error.
func (s *Store) UpdateBackup(ctx context.Context, b Backup) error {
	err := exec1(ctx, s.w, "UPDATE backups SET path = ?, size_bytes = ?, s3_key = ?, status = ?, error = ? WHERE id = ?",
		b.Path, b.SizeBytes, b.S3Key, b.Status, b.Error, b.ID)
	if err != nil {
		return fmt.Errorf("update backup: %w", err)
	}
	return nil
}

func (s *Store) GetBackup(ctx context.Context, id string) (Backup, error) {
	b, err := scanBackup(s.r.QueryRowContext(ctx, "SELECT "+backupCols+" FROM backups WHERE id = ?", id))
	if err != nil {
		return Backup{}, fmt.Errorf("get backup: %w", dbErr(err))
	}
	return b, nil
}

// ListBackups returns the backups, newest first.
func (s *Store) ListBackups(ctx context.Context) ([]Backup, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+backupCols+" FROM backups ORDER BY created_at DESC, id DESC")
	list, err := scanAll(rows, err, scanBackup)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	return list, nil
}

func (s *Store) DeleteBackup(ctx context.Context, id string) error {
	if err := exec1(ctx, s.w, "DELETE FROM backups WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete backup: %w", err)
	}
	return nil
}
