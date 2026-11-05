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

func (e *CacheEntry) cols() []any {
	return []any{&e.ID, &e.Repository, &e.Ref, &e.Key, &e.Version, &e.SizeBytes, &e.Path, &e.Finalized,
		at(&e.CreatedAt), at(&e.LastUsedAt)}
}

// CreateCacheEntry returns ErrConflict if the repository and ref have an
// entry with the same key and version.
func (s *Store) CreateCacheEntry(ctx context.Context, e CacheEntry) error {
	return exec(ctx, s.w, "create cache entry", insert("cache_entries", cacheCols), e.cols()...)
}

func (s *Store) GetCacheEntry(ctx context.Context, id string) (CacheEntry, error) {
	return queryOne(ctx, s.r, "get cache entry", scanRow[CacheEntry], "SELECT "+cacheCols+" FROM cache_entries WHERE id = ?", id)
}

func (s *Store) GetCacheEntryByKey(ctx context.Context, repository, ref, key, version string) (CacheEntry, error) {
	return queryOne(ctx, s.r, "get cache entry", scanRow[CacheEntry], "SELECT "+cacheCols+
		" FROM cache_entries WHERE repository = ? AND ref = ? AND key = ? AND version = ?", repository, ref, key, version)
}

// MatchCacheEntry finds a finalized entry of the repository with this
// version. It looks in refs in order and takes the newest entry of the first
// ref that has one. With prefix, the key of the entry must start with key;
// without, it must be equal. It returns ErrNotFound if no entry matches.
func (s *Store) MatchCacheEntry(ctx context.Context, repository, version string, refs []string, key string, prefix bool) (CacheEntry, error) {
	keyCond := "key = ?"
	if prefix {
		keyCond = "instr(key, ?) = 1" // LIKE would ignore case
	}
	for _, ref := range refs {
		e, err := queryOne(ctx, s.r, "match cache entry", scanRow[CacheEntry], "SELECT "+cacheCols+` FROM cache_entries
			WHERE repository = ? AND version = ? AND ref = ? AND finalized = 1 AND `+keyCond+`
			ORDER BY created_at DESC LIMIT 1`, repository, version, ref, key)
		if !errors.Is(err, ErrNotFound) {
			return e, err
		}
	}
	return CacheEntry{}, fmt.Errorf("match cache entry: %w", ErrNotFound)
}

func (s *Store) TouchCacheEntry(ctx context.Context, id string, used time.Time) error {
	return exec1(ctx, s.w, "touch cache entry", "UPDATE cache_entries SET last_used_at = ? WHERE id = ?", ms(used), id)
}

// FinishCacheEntry marks the upload complete with its size, and counts the
// finish as the last use.
func (s *Store) FinishCacheEntry(ctx context.Context, id string, size int64, at time.Time) error {
	return exec1(ctx, s.w, "finish cache entry",
		"UPDATE cache_entries SET finalized = 1, size_bytes = ?, last_used_at = ? WHERE id = ?", size, ms(at), id)
}

// ListCacheEntries returns the entries of a repository ("" = all), last used first.
func (s *Store) ListCacheEntries(ctx context.Context, repository string) ([]CacheEntry, error) {
	return queryAll(ctx, s.r, "list cache entries", scanRow[CacheEntry], "SELECT "+cacheCols+
		" FROM cache_entries WHERE ? = '' OR repository = ? ORDER BY last_used_at DESC, id DESC", repository, repository)
}

// LeastRecentCacheEntries returns the finalized entries that were used least
// recently, first the oldest.
func (s *Store) LeastRecentCacheEntries(ctx context.Context, limit int) ([]CacheEntry, error) {
	return queryAll(ctx, s.r, "list cache entries", scanRow[CacheEntry], "SELECT "+cacheCols+
		" FROM cache_entries WHERE finalized = 1 ORDER BY last_used_at, id LIMIT ?", limit)
}

// ListUnfinishedCacheEntries returns the entries with an upload that started before before.
func (s *Store) ListUnfinishedCacheEntries(ctx context.Context, before time.Time) ([]CacheEntry, error) {
	return queryAll(ctx, s.r, "list cache entries", scanRow[CacheEntry], "SELECT "+cacheCols+
		" FROM cache_entries WHERE finalized = 0 AND created_at < ? ORDER BY created_at", ms(before))
}

// DeleteCacheEntries deletes the entry id, or all entries of repository if
// id is empty, and returns the deleted entries so the caller can remove their files.
func (s *Store) DeleteCacheEntries(ctx context.Context, repository, id string) ([]CacheEntry, error) {
	if repository == "" && id == "" {
		return nil, errors.New("delete cache entries: give a repository or an ID")
	}
	return queryAll(ctx, s.w, "delete cache entries", scanRow[CacheEntry], `DELETE FROM cache_entries
		WHERE (? = '' OR repository = ?) AND (? = '' OR id = ?) RETURNING `+cacheCols, repository, repository, id, id)
}

// CacheUsage returns the use of each repository, largest first.
func (s *Store) CacheUsage(ctx context.Context) ([]CacheUsage, error) {
	return queryAll(ctx, s.r, "get cache usage", func(sc scanner) (CacheUsage, error) {
		var u CacheUsage
		return u, sc.Scan(&u.Repository, &u.Entries, &u.SizeBytes, atPtr(&u.LastUsedAt))
	}, `SELECT repository, count(*), sum(size_bytes), max(last_used_at)
		FROM cache_entries WHERE finalized = 1 GROUP BY repository ORDER BY 3 DESC, repository`)
}

// CacheSize is the size of all entries, also the unfinished uploads.
func (s *Store) CacheSize(ctx context.Context) (int64, error) {
	return queryOne(ctx, s.r, "get cache size", scanValue[int64], "SELECT coalesce(sum(size_bytes), 0) FROM cache_entries")
}

// AddCacheStats adds c to the counts of the repository for the hour of t.
func (s *Store) AddCacheStats(ctx context.Context, t time.Time, repository string, c CacheCounts) error {
	return exec(ctx, s.w, "add cache stats", `INSERT INTO cache_stats (hour, repository, hits, misses, bytes_read, bytes_written)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (hour, repository) DO UPDATE SET hits = hits + excluded.hits, misses = misses + excluded.misses,
			bytes_read = bytes_read + excluded.bytes_read, bytes_written = bytes_written + excluded.bytes_written`,
		t.Unix()/3600, repository, c.Hits, c.Misses, c.BytesRead, c.BytesWritten)
}

// CacheStatsByRepository sums the counts of each repository from the hour of since.
func (s *Store) CacheStatsByRepository(ctx context.Context, since time.Time) (map[string]CacheCounts, error) {
	return queryMap(ctx, s.r, "get cache stats", func(sc scanner) (string, CacheCounts, error) {
		var repo string
		var c CacheCounts
		err := sc.Scan(&repo, &c.Hits, &c.Misses, &c.BytesRead, &c.BytesWritten)
		return repo, c, err
	}, `SELECT repository, sum(hits), sum(misses), sum(bytes_read), sum(bytes_written)
		FROM cache_stats WHERE hour >= ? GROUP BY repository`, since.Unix()/3600)
}

func (s *Store) PruneCacheStats(ctx context.Context, before time.Time) error {
	return exec(ctx, s.w, "prune cache stats", "DELETE FROM cache_stats WHERE hour < ?", before.Unix()/3600)
}
