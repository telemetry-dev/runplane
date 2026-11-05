package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/backup"
	"github.com/runplane/runplane/internal/cache"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/github"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/tlsutil"
	"github.com/runplane/runplane/internal/update"
	"github.com/runplane/runplane/internal/version"
)

// pools is the api Scheduler: a pool change also changes the scale sets
// on GitHub.
type pools struct {
	*scheduler.Scheduler
	gh *github.Service
}

func (p pools) PoolsChanged(ctx context.Context) {
	p.gh.PoolsChanged(ctx)
	p.Scheduler.PoolsChanged(ctx)
}

// githubRef breaks the construction cycle of the scheduler and the github
// service: the scheduler gets it first, and the service fills it.
type githubRef struct{ *github.Service }

// caches is the api Caches service.
type caches struct {
	srv    *cache.Server
	mirror *mirror.Mirror
	// agents sums the mirror stats of the agents, which have mirrors of
	// their own.
	agents func(context.Context) mirror.Stats
}

func (c caches) Delete(ctx context.Context, repository, id string) (int, error) {
	if id == "" {
		if repository == "" {
			return 0, huma.Error422UnprocessableEntity("Give a repository or an entry ID.")
		}
		return c.srv.DeleteRepository(ctx, repository)
	}
	if err := c.srv.Delete(ctx, id); errors.Is(err, cache.ErrNotFound) {
		return 0, huma.Error404NotFound("No cache entry has this ID.")
	} else if err != nil {
		return 0, err
	}
	return 1, nil
}

func (c caches) MaxBytes() int64 { return c.srv.Limit() }

// MirrorStats sums the registry mirror stats of all servers.
func (c caches) MirrorStats(ctx context.Context) (api.MirrorStats, error) {
	s := c.mirror.Stats()
	s.Add(c.agents(ctx))
	return api.MirrorStats{Hits: s.Hits, Misses: s.Misses, HitBytes: s.HitBytes, MissBytes: s.MissBytes, SizeBytes: s.SizeBytes}, nil
}

// certificate is the api HTTPS service.
type certificate struct{ acme *tlsutil.ACME }

func (c certificate) CertificateStatus(ctx context.Context) api.HTTPSStatus {
	s := c.acme.Status(ctx)
	out := api.HTTPSStatus{State: s.State, Error: s.Error}
	if !s.NotAfter.IsZero() {
		out.NotAfter = &s.NotAfter
	}
	return out
}

// backups makes database backups: the api Backups service and the daily backup.
type backups struct {
	st     *store.Store
	box    *secret.Box
	bus    *events.Bus
	dir    string
	notify func(notify.Event)
	log    *slog.Logger
	mu     sync.Mutex // one backup at a time
}

// Run makes a backup, removes the backups over backup_keep, and uploads
// the new one to S3 if a bucket is set. A failed upload keeps the local
// backup and records the error on it.
func (b *backups) Run(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, err := b.st.GetSettings(ctx)
	if err != nil {
		return "", err
	}
	row := store.Backup{ID: ids.New(), CreatedAt: time.Now(), Status: store.BackupRunning}
	if err := b.st.CreateBackup(ctx, row); err != nil {
		return "", err
	}
	b.publish(row)
	res, err := backup.Run(ctx, b.st.DB(), b.dir, st.BackupKeep)
	if err != nil {
		row.Status, row.Error = store.BackupFailed, err.Error()
		b.save(ctx, row)
		b.failed(st, err)
		return "", fmt.Errorf("back up the database: %w", err)
	}
	row.Path, row.SizeBytes, row.Status = res.Path, res.Size, store.BackupDone
	b.save(ctx, row)
	b.dropRows(ctx, res.Removed)
	if st.BackupS3.Bucket == "" {
		return row.ID, nil
	}
	row.Status = store.BackupUploading
	b.save(ctx, row)
	row.S3Key, err = b.upload(ctx, st.BackupS3, res.Path)
	if err != nil {
		row.Status, row.Error = store.BackupDone, err.Error()
		b.failed(st, err)
	} else {
		row.Status = store.BackupUploaded
	}
	b.save(ctx, row)
	return row.ID, nil
}

func (b *backups) upload(ctx context.Context, s store.BackupS3, path string) (string, error) {
	key, err := b.box.Open(s.SecretKeyEnc)
	if err != nil {
		return "", fmt.Errorf("open the S3 secret key: %w", err)
	}
	bucket := backup.S3{Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, Prefix: s.Prefix, AccessKey: s.AccessKey, SecretKey: string(key)}
	k, err := bucket.Upload(ctx, path)
	if err != nil {
		return "", fmt.Errorf("upload to S3: %w", err)
	}
	return k, nil
}

func (b *backups) save(ctx context.Context, row store.Backup) {
	if err := b.st.UpdateBackup(context.WithoutCancel(ctx), row); err != nil {
		b.log.Warn("record backup", "backup", row.ID, "err", err)
		return
	}
	b.publish(row)
}

func (b *backups) publish(row store.Backup) {
	b.bus.Publish(events.Event{Type: events.BackupState, Data: row})
}

// dropRows deletes the rows of the backup files that the retention removed.
func (b *backups) dropRows(ctx context.Context, removed []string) {
	if len(removed) == 0 {
		return
	}
	list, err := b.st.ListBackups(ctx)
	if err != nil {
		b.log.Warn("list backups", "err", err)
		return
	}
	for _, row := range list {
		if !slices.Contains(removed, row.Path) {
			continue
		}
		if err := b.st.DeleteBackup(ctx, row.ID); err != nil {
			b.log.Warn("delete backup row", "backup", row.ID, "err", err)
		}
	}
}

func (b *backups) failed(st store.Settings, err error) {
	b.log.Error("backup failed", "err", err)
	b.notify(notify.BackupFailedEvent(st.DashboardURL(), err))
}

// daily makes a backup each day at 03:00 UTC while backup_enabled is on.
func (b *backups) daily(ctx context.Context) {
	backup.Daily(ctx, func(ctx context.Context) {
		st, err := b.st.GetSettings(ctx)
		if err != nil || !st.BackupEnabled {
			return
		}
		if _, err := b.Run(ctx); err != nil {
			b.log.Warn("daily backup", "err", err)
		}
	})
}

// updater is the api Updater service.
type updater struct {
	u *update.Updater
	// restart ends the process after an update, so that systemd starts
	// the new binary.
	restart func()
	log     *slog.Logger

	mu      sync.Mutex
	checked time.Time // of last, which stays valid for checkTTL
	last    update.Status
	state   string // of an auto update, for the dashboard
}

// checkTTL keeps the release reads of the dashboard far below the GitHub
// limit of 60 anonymous requests an hour.
const checkTTL = 10 * time.Minute

// Check reads the latest release, at most once in checkTTL. A repository
// without releases has no update.
func (u *updater) Check(ctx context.Context) (api.UpdateStatus, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if time.Since(u.checked) >= checkTTL {
		s, err := u.u.Check(ctx)
		if errors.Is(err, update.ErrNoRelease) {
			s, err = update.Status{Current: u.u.Current}, nil
		}
		if err != nil {
			return api.UpdateStatus{}, err
		}
		u.last, u.checked = s, time.Now()
	}
	s := u.last
	out := api.UpdateStatus{Current: s.Current, Latest: s.Latest, Available: s.Available, URL: s.URL, Notes: s.Notes, State: u.state}
	if !s.PublishedAt.IsZero() {
		out.PublishedAt = &s.PublishedAt
	}
	return out, nil
}

func (u *updater) Apply(ctx context.Context) error {
	s, err := u.u.Check(ctx)
	if err != nil {
		return err
	}
	if !s.Available {
		return huma.Error409Conflict("runplane " + s.Current + " is the latest version.")
	}
	if err := u.u.Apply(ctx, s.Latest); err != nil {
		return err
	}
	u.log.Info("runplane restarts to run the new version", "version", s.Latest)
	u.restart()
	return nil
}

func (u *updater) setState(state string) {
	u.mu.Lock()
	u.state = state
	u.mu.Unlock()
}

// checkDaily checks for a new release each day. It sends one update.available
// notification for each new version, and with auto_update it installs the
// release and restarts runplane.
func (u *updater) checkDaily(ctx context.Context, st *store.Store, send func(notify.Event)) {
	if !version.IsRelease(version.Version) || update.PublicKey == "" {
		u.log.Debug("no update checks: this binary is not a signed release", "version", version.Version)
		return
	}
	told := ""
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 24 * time.Hour
		s, err := u.u.Check(ctx)
		if err != nil {
			u.log.Warn("check for updates", "err", err)
			continue
		}
		if !s.Available {
			continue
		}
		settings, err := st.GetSettings(ctx)
		if err != nil {
			u.log.Warn("read settings for an update", "err", err)
			continue
		}
		if settings.AutoUpdate {
			u.setState("applying")
			if err := u.u.Apply(ctx, s.Latest); err != nil {
				u.setState("")
				u.log.Error("install update", "version", s.Latest, "err", err)
			} else {
				u.setState("restarting")
				u.log.Info("runplane restarts to run the new version", "version", s.Latest)
				u.restart()
				return
			}
		}
		if told != s.Latest {
			told = s.Latest
			send(notify.UpdateAvailableEvent(settings.DashboardURL(), s.Current, s.Latest, s.URL))
		}
	}
}
