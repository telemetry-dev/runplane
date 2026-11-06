// Package backup makes the backups of the database: it copies the SQLite
// database to gzip files, keeps the newest ones, and uploads them to
// S3-compatible storage.
package backup

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/atomicfile"
)

type result struct {
	path    string   // the new backup file
	size    int64    // its size in bytes
	removed []string // older backup files that write removed
}

const (
	timeFormat  = "20060102T150405.000Z"
	filePattern = "runplane-????????T??????.???Z.db.gz"
)

// write copies the database with vacuumInto to dir/runplane-<UTC time>.db.gz
// and removes the oldest backups so that keep files stay. If keep is 0 or
// less, write removes no backups.
func write(ctx context.Context, vacuumInto func(ctx context.Context, path string) error, dir string, keep int) (result, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return result{}, fmt.Errorf("make backup dir: %w", err)
	}
	name, path := freeName(dir, time.Now().UTC())

	// VACUUM INTO fails if the target file exists.
	snapshot := filepath.Join(dir, "."+name+".db")
	os.Remove(snapshot)
	defer os.Remove(snapshot)
	if err := vacuumInto(ctx, snapshot); err != nil {
		return result{}, err
	}

	size, err := gzipFile(snapshot, path)
	if err != nil {
		return result{}, err
	}
	res := result{path: path, size: size}
	if keep > 0 {
		if res.removed, err = prune(dir, keep); err != nil {
			return res, err
		}
	}
	return res, nil
}

// freeName returns the name and path of a new backup at t, or a few
// milliseconds later if a backup has that name.
func freeName(dir string, t time.Time) (string, string) {
	for {
		name := "runplane-" + t.Format(timeFormat) + ".db.gz"
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return name, path
		}
		t = t.Add(time.Millisecond)
	}
}

// gzipFile compresses src to dst and returns the size of dst.
func gzipFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, fmt.Errorf("open snapshot: %w", err)
	}
	defer in.Close()

	out, err := atomicfile.Create(dst, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create backup: %w", err)
	}
	defer out.Close()
	zw := gzip.NewWriter(out)
	zw.Name = strings.TrimSuffix(filepath.Base(dst), ".gz")
	_, err = io.Copy(zw, in)
	if err == nil {
		err = zw.Close()
	}
	if err == nil {
		err = out.Commit()
	}
	if err != nil {
		return 0, fmt.Errorf("write backup: %w", err)
	}
	info, err := out.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat backup: %w", err)
	}
	return info.Size(), nil
}

// prune removes all but the newest keep backups. The UTC time in the file
// names sorts in time order.
func prune(dir string, keep int) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, filePattern))
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	if len(files) <= keep {
		return nil, nil
	}
	slices.Sort(files)
	var removed []string
	for _, f := range files[:len(files)-keep] {
		if err := os.Remove(f); err != nil {
			return removed, fmt.Errorf("remove old backup: %w", err)
		}
		removed = append(removed, f)
	}
	return removed, nil
}

// DailyAt is the time of day in UTC of the daily backup.
const DailyAt = 3 * time.Hour

// daily calls job each day at DailyAt until ctx ends.
func daily(ctx context.Context, job func(context.Context)) {
	for {
		t := time.NewTimer(time.Until(nextRun(time.Now())))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			job(ctx)
		}
	}
}

// nextRun returns the first DailyAt after now.
func nextRun(now time.Time) time.Time {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(DailyAt)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
