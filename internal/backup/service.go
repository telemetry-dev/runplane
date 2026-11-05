package backup

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

type Config struct {
	Store *store.Store
	Box   *secret.Box // opens the S3 secret key
	Bus   *events.Bus
	Dir   string // the backups folder
	// Notify sends a notification. It must not wait for the delivery.
	Notify func(notify.Event)
	Log    *slog.Logger // nil: no log
}

// Service makes the backups of the database: when an admin asks, and each
// day while backup_enabled is on. Each backup has a row, and each change of
// the row goes out as a backup.state event.
type Service struct {
	cfg Config
	log *slog.Logger
	mu  sync.Mutex // one backup at a time
}

func New(cfg Config) *Service {
	return &Service{cfg: cfg, log: cmp.Or(cfg.Log, slog.New(slog.DiscardHandler))}
}

// Run makes a backup each day at 03:00 UTC while backup_enabled is on,
// until ctx ends.
func (s *Service) Run(ctx context.Context) {
	daily(ctx, func(ctx context.Context) {
		st, err := s.cfg.Store.GetSettings(ctx)
		if err != nil || !st.BackupEnabled {
			return
		}
		if _, err := s.Create(ctx); err != nil {
			s.log.Warn("daily backup", "err", err)
		}
	})
}

// Create makes a backup, removes the backups over backup_keep, and uploads
// the new one to S3 if a bucket is set. A failed upload fails the backup
// but keeps its file. Each failure sends a backup.failed notification.
func (s *Service) Create(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.cfg.Store.GetSettings(ctx)
	if err != nil {
		return "", err
	}
	row := store.Backup{ID: ids.New(), CreatedAt: time.Now(), Status: store.BackupRunning}
	if err := s.cfg.Store.CreateBackup(ctx, row); err != nil {
		return "", err
	}
	s.publish(row)
	res, err := write(ctx, s.cfg.Store.DB(), s.cfg.Dir, st.BackupKeep)
	if err != nil {
		row.Status, row.Error = store.BackupFailed, err.Error()
		s.save(ctx, row)
		s.failed(st, err)
		return "", fmt.Errorf("back up the database: %w", err)
	}
	row.Path, row.SizeBytes, row.Status = res.path, res.size, store.BackupDone
	s.save(ctx, row)
	s.dropRows(ctx, res.removed)
	if st.BackupS3.Bucket == "" {
		return row.ID, nil
	}
	row.Status = store.BackupUploading
	s.save(ctx, row)
	row.S3Key, err = s.upload(ctx, st.BackupS3, res.path)
	if err != nil {
		row.Status, row.Error = store.BackupFailed, err.Error()
		s.failed(st, err)
	} else {
		row.Status = store.BackupUploaded
	}
	s.save(ctx, row)
	return row.ID, nil
}

func (s *Service) upload(ctx context.Context, b store.BackupS3, path string) (string, error) {
	key, err := s.cfg.Box.Open(b.SecretKeyEnc)
	if err != nil {
		return "", fmt.Errorf("open the S3 secret key: %w", err)
	}
	bucket := S3{Endpoint: b.Endpoint, Region: b.Region, Bucket: b.Bucket, Prefix: b.Prefix, AccessKey: b.AccessKey, SecretKey: string(key)}
	k, err := bucket.Upload(ctx, path)
	if err != nil {
		return "", fmt.Errorf("upload to S3: %w", err)
	}
	return k, nil
}

func (s *Service) save(ctx context.Context, row store.Backup) {
	if err := s.cfg.Store.UpdateBackup(context.WithoutCancel(ctx), row); err != nil {
		s.log.Warn("record backup", "backup", row.ID, "err", err)
		return
	}
	s.publish(row)
}

func (s *Service) publish(row store.Backup) {
	s.cfg.Bus.Publish(events.BackupState(row))
}

func (s *Service) dropRows(ctx context.Context, removed []string) {
	if len(removed) == 0 {
		return
	}
	list, err := s.cfg.Store.ListBackups(ctx)
	if err != nil {
		s.log.Warn("list backups", "err", err)
		return
	}
	for _, row := range list {
		if !slices.Contains(removed, row.Path) {
			continue
		}
		if err := s.cfg.Store.DeleteBackup(ctx, row.ID); err != nil {
			s.log.Warn("delete backup row", "backup", row.ID, "err", err)
		}
	}
}

func (s *Service) failed(st store.Settings, err error) {
	s.log.Error("backup failed", "err", err)
	s.cfg.Notify(notify.BackupFailedEvent(st.DashboardURL(), err))
}
