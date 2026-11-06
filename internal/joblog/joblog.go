// Package joblog keeps the logs of the jobs. While a job runs, its sandbox
// streams the log (log state live). After the job, the finish step of the
// scheduler puts the GitHub log in a file in the logs folder (stored), or
// GitHub has none (none). The retention removes old files (expired).
package joblog

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/runplane/runplane/internal/atomicfile"
	"github.com/runplane/runplane/internal/store"
)

type Config struct {
	Dir   string // the logs folder
	Store *store.Store
	// Driver returns the driver of a connected server, or nil. The live log
	// of a job comes from the driver of its sandbox.
	Driver func(serverID string) Driver
	// Waits are the waits before the download tries. GitHub can need some
	// time after a job before its log is ready. nil: 0, 10 s, 30 s, 2 min.
	Waits []time.Duration
	Log   *slog.Logger // nil: no log
}

// Driver is the part of a sandbox driver that the logs use.
type Driver interface {
	Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
}

type Logs struct {
	cfg Config
	log *slog.Logger
}

func New(cfg Config) *Logs {
	if cfg.Waits == nil {
		cfg.Waits = []time.Duration{0, 10 * time.Second, 30 * time.Second, 2 * time.Minute}
	}
	return &Logs{cfg: cfg, log: cmp.Or(cfg.Log, slog.New(slog.DiscardHandler))}
}

// MarkLive records that the sandbox of a running job streams its log,
// unless the job has a log state already. A start message that comes again
// after the completion does not change the job.
func (l *Logs) MarkLive(ctx context.Context, job *store.Job) error {
	if job.State != store.JobRunning || job.LogState != store.LogNone {
		return nil
	}
	if err := l.cfg.Store.SetJobLogState(ctx, job.ID, store.LogLive); err != nil {
		return err
	}
	job.LogState = store.LogLive
	return nil
}

// Fetch downloads the GitHub log of a finished job with download, and
// records the log state. It writes the file in one step, so that the file
// is never a partial log. When ctx ends first, it records nothing, and the
// log state stays.
func (l *Logs) Fetch(ctx context.Context, jobID string, download func(context.Context) (io.ReadCloser, error)) {
	var err error
	for _, wait := range l.cfg.Waits {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err = l.write(ctx, jobID, download); err == nil {
			break
		}
	}
	if ctx.Err() != nil {
		return
	}
	state := store.LogStored
	if err != nil {
		l.log.Warn("download job log", "job", jobID, "err", err)
		state = store.LogNone
	}
	if err := l.cfg.Store.SetJobLogState(ctx, jobID, state); err != nil {
		l.log.Warn("save job log state", "job", jobID, "err", err)
	}
}

func (l *Logs) write(ctx context.Context, jobID string, download func(context.Context) (io.ReadCloser, error)) error {
	log, err := download(ctx)
	if err != nil {
		return err
	}
	defer log.Close()
	return atomicfile.Write(l.path(jobID), log, 0o600)
}

// Expire marks the stored logs of the jobs that finished before before as
// expired, and removes their files.
func (l *Logs) Expire(ctx context.Context, before time.Time) error {
	ids, err := l.cfg.Store.ExpireJobLogs(ctx, before)
	l.Remove(ids)
	return err
}

// Remove removes the log files of jobs, for example after the jobs are
// deleted.
func (l *Logs) Remove(jobIDs []string) {
	for _, id := range jobIDs {
		if err := os.Remove(l.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			l.log.Warn("remove job log", "job", id, "err", err)
		}
	}
}

func (l *Logs) path(jobID string) string { return filepath.Join(l.cfg.Dir, jobID+".log") }
