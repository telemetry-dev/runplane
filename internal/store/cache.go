package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CacheEntry is one Actions cache entry. A row exists from the start of the
// upload; Finalized is set when the upload is complete.
type CacheEntry struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"`
	Ref        string    `json:"ref"`
	Key        string    `json:"key"`
	Version    string    `json:"version"`
	SizeBytes  int64     `json:"size_bytes"`
	Path       string    `json:"-"`
	Finalized  bool      `json:"finalized"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// CacheUsage sums the finalized entries of one repository.
type CacheUsage struct {
	Repository string     `json:"repository"`
	Entries    int        `json:"entries"`
	SizeBytes  int64      `json:"size_bytes"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

type CacheCounts struct {
	Hits         int64 `json:"hits"`
	Misses       int64 `json:"misses"`
	BytesRead    int64 `json:"bytes_read"`
	BytesWritten int64 `json:"bytes_written"`
}

const cacheCols = "id, repository, ref, key, version, size_bytes, path, finalized, created_at, last_used_at"

func scanCacheEntry(sc scanner) (CacheEntry, error) {
	var e CacheEntry
	err := sc.Scan(&e.ID, &e.Repository, &e.Ref, &e.Key, &e.Version, &e.SizeBytes, &e.Path, &e.Finalized,
		at(&e.CreatedAt), at(&e.LastUsedAt))
	return e, err
}

// CreateCacheEntry returns ErrConflict if the repository and ref have an
// entry with the same key and version.
func (s *Store) CreateCacheEntry(ctx context.Context, e CacheEntry) error {
	_, err := s.w.ExecContext(ctx, "INSERT INTO cache_entries ("+cacheCols+") VALUES ("+placeholders(10)+")",
		e.ID, e.Repository, e.Ref, e.Key, e.Version, e.SizeBytes, e.Path, e.Finalized, ms(e.CreatedAt), ms(e.LastUsedAt))
	if err != nil {
		return fmt.Errorf("create cache entry: %w", dbErr(err))
	}
	return nil
}

func (s *Store) GetCacheEntry(ctx context.Context, id string) (CacheEntry, error) {
	e, err := scanCacheEntry(s.r.QueryRowContext(ctx, "SELECT "+cacheCols+" FROM cache_entries WHERE id = ?", id))
	if err != nil {
		return CacheEntry{}, fmt.Errorf("get cache entry: %w", dbErr(err))
	}
	return e, nil
}

func (s *Store) GetCacheEntryByKey(ctx context.Context, repository, ref, key, version string) (CacheEntry, error) {
	e, err := scanCacheEntry(s.r.QueryRowContext(ctx, "SELECT "+cacheCols+
		" FROM cache_entries WHERE repository = ? AND ref = ? AND key = ? AND version = ?", repository, ref, key, version))
	if err != nil {
		return CacheEntry{}, fmt.Errorf("get cache entry: %w", dbErr(err))
	}
	return e, nil
}

// FinalizeCacheEntry marks the upload complete with its size.
func (s *Store) FinalizeCacheEntry(ctx context.Context, id string, size int64) error {
	err := exec1(ctx, s.w, "UPDATE cache_entries SET finalized = 1, size_bytes = ? WHERE id = ?", size, id)
	if err != nil {
		return fmt.Errorf("finalize cache entry: %w", err)
	}
	return nil
}

// MatchCacheEntry finds a finalized entry of the repository with this
// version. It looks in refs in order and takes the newest entry of the first
// ref that has one. With prefix, the key of the entry must start with key;
// without, it must be equal. It returns ErrNotFound if no entry matches.
func (s *Store) MatchCacheEntry(ctx context.Context, repository, version string, refs []string, key string, prefix bool) (CacheEntry, error) {
	if len(refs) == 0 {
		return CacheEntry{}, fmt.Errorf("match cache entry: %w", ErrNotFound)
	}
	query := "SELECT " + cacheCols + " FROM cache_entries WHERE repository = ? AND version = ? AND finalized = 1"
	args := []any{repository, version, key}
	if prefix {
		query += " AND instr(key, ?) = 1" // LIKE would ignore case
	} else {
		query += " AND key = ?"
	}
	query += " AND ref IN (" + placeholders(len(refs)) + ") ORDER BY CASE ref"
	for _, ref := range refs {
		args = append(args, ref)
	}
	for i, ref := range refs {
		query += fmt.Sprintf(" WHEN ? THEN %d", i)
		args = append(args, ref)
	}
	query += " END, created_at DESC LIMIT 1"
	e, err := scanCacheEntry(s.r.QueryRowContext(ctx, query, args...))
	if err != nil {
		return CacheEntry{}, fmt.Errorf("match cache entry: %w", dbErr(err))
	}
	return e, nil
}

func (s *Store) TouchCacheEntry(ctx context.Context, id string, used time.Time) error {
	if err := exec1(ctx, s.w, "UPDATE cache_entries SET last_used_at = ? WHERE id = ?", ms(used), id); err != nil {
		return fmt.Errorf("touch cache entry: %w", err)
	}
	return nil
}

// ListCacheEntries returns the entries of a repository ("" = all), last used first.
func (s *Store) ListCacheEntries(ctx context.Context, repository string) ([]CacheEntry, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+cacheCols+
		" FROM cache_entries WHERE ? = '' OR repository = ? ORDER BY last_used_at DESC, id DESC", repository, repository)
	list, err := scanAll(rows, err, scanCacheEntry)
	if err != nil {
		return nil, fmt.Errorf("list cache entries: %w", err)
	}
	return list, nil
}

// LeastRecentCacheEntries returns the finalized entries that were used least
// recently, first the oldest.
func (s *Store) LeastRecentCacheEntries(ctx context.Context, limit int) ([]CacheEntry, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+cacheCols+
		" FROM cache_entries WHERE finalized = 1 ORDER BY last_used_at, id LIMIT ?", limit)
	list, err := scanAll(rows, err, scanCacheEntry)
	if err != nil {
		return nil, fmt.Errorf("list cache entries: %w", err)
	}
	return list, nil
}

// ListUnfinishedCacheEntries returns the entries with an upload that started before before.
func (s *Store) ListUnfinishedCacheEntries(ctx context.Context, before time.Time) ([]CacheEntry, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+cacheCols+
		" FROM cache_entries WHERE finalized = 0 AND created_at < ? ORDER BY created_at", ms(before))
	list, err := scanAll(rows, err, scanCacheEntry)
	if err != nil {
		return nil, fmt.Errorf("list cache entries: %w", err)
	}
	return list, nil
}

// DeleteCacheEntries deletes the entry id, or all entries of repository if
// id is empty, and returns the deleted entries so the caller can remove their files.
func (s *Store) DeleteCacheEntries(ctx context.Context, repository, id string) ([]CacheEntry, error) {
	if repository == "" && id == "" {
		return nil, errors.New("delete cache entries: give a repository or an ID")
	}
	rows, err := s.w.QueryContext(ctx, `DELETE FROM cache_entries
		WHERE (? = '' OR repository = ?) AND (? = '' OR id = ?) RETURNING `+cacheCols, repository, repository, id, id)
	list, err := scanAll(rows, err, scanCacheEntry)
	if err != nil {
		return nil, fmt.Errorf("delete cache entries: %w", err)
	}
	return list, nil
}

// CacheUsage returns the use of each repository, largest first.
func (s *Store) CacheUsage(ctx context.Context) ([]CacheUsage, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT repository, count(*), sum(size_bytes), max(last_used_at)
		FROM cache_entries WHERE finalized = 1 GROUP BY repository ORDER BY 3 DESC, repository`)
	list, err := scanAll(rows, err, func(sc scanner) (CacheUsage, error) {
		var u CacheUsage
		return u, sc.Scan(&u.Repository, &u.Entries, &u.SizeBytes, atPtr(&u.LastUsedAt))
	})
	if err != nil {
		return nil, fmt.Errorf("get cache usage: %w", err)
	}
	return list, nil
}

// CacheSize is the size of all entries, also the unfinished uploads.
func (s *Store) CacheSize(ctx context.Context) (int64, error) {
	var n int64
	if err := s.r.QueryRowContext(ctx, "SELECT coalesce(sum(size_bytes), 0) FROM cache_entries").Scan(&n); err != nil {
		return 0, fmt.Errorf("get cache size: %w", err)
	}
	return n, nil
}

// AddCacheStats adds c to the counts of the repository for the hour of t.
func (s *Store) AddCacheStats(ctx context.Context, t time.Time, repository string, c CacheCounts) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO cache_stats (hour, repository, hits, misses, bytes_read, bytes_written)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (hour, repository) DO UPDATE SET hits = hits + excluded.hits, misses = misses + excluded.misses,
			bytes_read = bytes_read + excluded.bytes_read, bytes_written = bytes_written + excluded.bytes_written`,
		t.Unix()/3600, repository, c.Hits, c.Misses, c.BytesRead, c.BytesWritten)
	if err != nil {
		return fmt.Errorf("add cache stats: %w", err)
	}
	return nil
}

// CacheStatsByRepository sums the counts of each repository from the hour of since.
func (s *Store) CacheStatsByRepository(ctx context.Context, since time.Time) (map[string]CacheCounts, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT repository, sum(hits), sum(misses), sum(bytes_read), sum(bytes_written)
		FROM cache_stats WHERE hour >= ? GROUP BY repository`, since.Unix()/3600)
	if err != nil {
		return nil, fmt.Errorf("get cache stats: %w", err)
	}
	defer rows.Close()
	stats := map[string]CacheCounts{}
	for rows.Next() {
		var repo string
		var c CacheCounts
		if err := rows.Scan(&repo, &c.Hits, &c.Misses, &c.BytesRead, &c.BytesWritten); err != nil {
			return nil, fmt.Errorf("get cache stats: %w", err)
		}
		stats[repo] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get cache stats: %w", err)
	}
	return stats, nil
}

// PruneCacheStats deletes the counts of the hours before before.
func (s *Store) PruneCacheStats(ctx context.Context, before time.Time) error {
	if _, err := s.w.ExecContext(ctx, "DELETE FROM cache_stats WHERE hour < ?", before.Unix()/3600); err != nil {
		return fmt.Errorf("prune cache stats: %w", err)
	}
	return nil
}
