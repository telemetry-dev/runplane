// Package backup copies the SQLite database to gzip files, keeps the newest
// ones, and uploads them to S3-compatible storage.
package backup

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Result describes one backup.
type Result struct {
	Path    string   // the new backup file
	Size    int64    // its size in bytes
	Removed []string // older backup files that Run removed
}

const (
	timeFormat  = "20060102T150405.000Z"
	filePattern = "runplane-????????T??????.???Z.db.gz"
)

// Run copies the database with VACUUM INTO, writes it to
// dir/runplane-<UTC time>.db.gz, and removes the oldest backups so that
// keep files stay. If keep is 0 or less, Run removes no backups.
func Run(ctx context.Context, db *sql.DB, dir string, keep int) (Result, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("make backup dir: %w", err)
	}
	name, path := freeName(dir, time.Now().UTC())

	// VACUUM INTO writes a consistent copy that includes the WAL, and it
	// does not block writers. It fails if the target file exists.
	snapshot := filepath.Join(dir, "."+name+".db")
	os.Remove(snapshot)
	defer os.Remove(snapshot)
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", snapshot); err != nil {
		return Result{}, fmt.Errorf("vacuum into snapshot: %w", err)
	}

	size, err := gzipFile(snapshot, path)
	if err != nil {
		return Result{}, err
	}
	res := Result{Path: path, Size: size}
	if keep > 0 {
		if res.Removed, err = prune(dir, keep); err != nil {
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

// gzipFile compresses src to dst through a temporary file and returns the
// size of dst.
func gzipFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, fmt.Errorf("open snapshot: %w", err)
	}
	defer in.Close()

	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create backup: %w", err)
	}
	defer os.Remove(tmp)

	zw := gzip.NewWriter(out)
	zw.Name = strings.TrimSuffix(filepath.Base(dst), ".gz")
	_, err = io.Copy(zw, in)
	if err == nil {
		err = zw.Close()
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, fmt.Errorf("write backup: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return 0, fmt.Errorf("write backup: %w", err)
	}
	info, err := os.Stat(dst)
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
	sort.Strings(files)
	var removed []string
	for _, f := range files[:len(files)-keep] {
		if err := os.Remove(f); err != nil {
			return removed, fmt.Errorf("remove old backup: %w", err)
		}
		removed = append(removed, f)
	}
	return removed, nil
}
