package store

import (
	"context"
	"time"
)

// CacheEntry is one Actions cache entry. A row exists from the start of the
// upload; Finalized is set when the upload is complete. The cache server
// keeps the blob of the entry in a file named by its ID.
type CacheEntry struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
	Key        string `json:"key"`
	Version    string `json:"version"`
	// SizeBytes is the size of the blob, and of an unfinished upload the
	// bytes that it has on disk now.
	SizeBytes  int64     `json:"size_bytes"`
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

// CacheBytes are the bytes of the Actions cache on disk.
type CacheBytes struct {
	Entries int64 // of the finalized entries
	Uploads int64 // of the unfinished uploads
}

const cacheCols = "id, repository, ref, key, version, size_bytes, finalized, created_at, last_used_at"

func (e *CacheEntry) cols() []any {
	return []any{&e.ID, &e.Repository, &e.Ref, &e.Key, &e.Version, &e.SizeBytes, &e.Finalized,
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

// MatchCacheEntry finds the newest finalized entry of the repository and
// the ref with this version. With prefix, the key of the entry must start
// with key; without, it must be equal. It returns ErrNotFound if no entry
// matches.
func (s *Store) MatchCacheEntry(ctx context.Context, repository, version, ref, key string, prefix bool) (CacheEntry, error) {
	keyCond := "key = ?"
	if prefix {
		keyCond = "instr(key, ?) = 1" // LIKE would ignore case
	}
	return queryOne(ctx, s.r, "match cache entry", scanRow[CacheEntry], "SELECT "+cacheCols+` FROM cache_entries
		WHERE repository = ? AND version = ? AND ref = ? AND finalized = 1 AND `+keyCond+`
		ORDER BY created_at DESC LIMIT 1`, repository, version, ref, key)
}

func (s *Store) TouchCacheEntry(ctx context.Context, id string, used time.Time) error {
	return exec1(ctx, s.w, "touch cache entry", "UPDATE cache_entries SET last_used_at = ? WHERE id = ?", ms(used), id)
}

// SetCacheUpload records the bytes that an unfinished upload has on disk
// now, and its last use.
func (s *Store) SetCacheUpload(ctx context.Context, id string, size int64, used time.Time) error {
	return exec1(ctx, s.w, "set cache upload",
		"UPDATE cache_entries SET size_bytes = ?, last_used_at = ? WHERE id = ? AND finalized = 0", size, ms(used), id)
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

// DeleteCacheEntry deletes the entry id. An entry that is gone is not an
// error.
func (s *Store) DeleteCacheEntry(ctx context.Context, id string) error {
	return exec(ctx, s.w, "delete cache entry", "DELETE FROM cache_entries WHERE id = ?", id)
}

// CacheUsage returns the use of each repository, largest first.
func (s *Store) CacheUsage(ctx context.Context) ([]CacheUsage, error) {
	return queryAll(ctx, s.r, "get cache usage", func(sc scanner) (CacheUsage, error) {
		var u CacheUsage
		err := sc.Scan(&u.Repository, &u.Entries, &u.SizeBytes, atPtr(&u.LastUsedAt))
		return u, err
	}, `SELECT repository, count(*), sum(size_bytes), max(last_used_at)
		FROM cache_entries WHERE finalized = 1 GROUP BY repository ORDER BY 3 DESC, repository`)
}

func (s *Store) CacheSize(ctx context.Context) (CacheBytes, error) {
	return queryOne(ctx, s.r, "get cache size", func(sc scanner) (CacheBytes, error) {
		var b CacheBytes
		err := sc.Scan(&b.Entries, &b.Uploads)
		return b, err
	}, `SELECT coalesce(sum(size_bytes) FILTER (WHERE finalized = 1), 0), coalesce(sum(size_bytes) FILTER (WHERE finalized = 0), 0)
		FROM cache_entries`)
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
