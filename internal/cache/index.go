package cache

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound means that no entry matches.
	ErrNotFound = errors.New("cache entry not found")
	// ErrExists means that an entry with the same ID, or the same repository,
	// ref, key, and version, already exists.
	ErrExists = errors.New("cache entry exists")
)

// Entry is one cache archive. It maps to one row of the cache_entries table.
type Entry struct {
	ID         string // ULID
	Repository string // owner/name
	Ref        string // scope that wrote the entry, for example refs/heads/main
	Key        string
	Version    string
	SizeBytes  int64  // 0 until finalized
	Path       string // blob path, relative to the cache folder
	Finalized  bool
	CreatedAt  time.Time
	LastUsedAt time.Time // last restore; for an unfinished entry, last upload activity
}

// Query selects finalized entries for a restore.
type Query struct {
	Repository string
	Ref        string
	Version    string
	Key        string
	Prefix     bool // match keys that start with Key, not only Key itself
}

// Usage is the disk use of the finalized entries of one repository.
type Usage struct {
	Repository string
	Entries    int
	Bytes      int64
}

// Index keeps the cache entry metadata. The blobs stay on disk.
// Repository, Ref, Key, and Version together are unique.
type Index interface {
	// Get returns the entry with this ID, finalized or not.
	Get(ctx context.Context, id string) (Entry, error)
	// Find returns the entry, finalized or not, with this repository, ref, key, and version.
	Find(ctx context.Context, repository, ref, key, version string) (Entry, error)
	// Lookup returns the newest finalized entry that matches q.
	Lookup(ctx context.Context, q Query) (Entry, error)
	// Create adds an unfinished entry. It returns ErrExists for a duplicate.
	Create(ctx context.Context, e Entry) error
	// Finalize marks the entry finalized with its size and sets LastUsedAt.
	Finalize(ctx context.Context, id string, size int64, at time.Time) error
	// Touch sets LastUsedAt.
	Touch(ctx context.Context, id string, at time.Time) error
	Delete(ctx context.Context, id string) error
	// List returns all entries of a repository, newest first.
	List(ctx context.Context, repository string) ([]Entry, error)
	// Usage returns the use of each repository that has finalized entries.
	Usage(ctx context.Context) ([]Usage, error)
	// Oldest returns up to limit finalized entries, least recently used first.
	Oldest(ctx context.Context, limit int) ([]Entry, error)
	// Unfinished returns the unfinished entries created before a time.
	Unfinished(ctx context.Context, before time.Time) ([]Entry, error)
}

// Counts is a change to the cache statistics of one repository.
type Counts struct {
	Hits, Misses, BytesRead, BytesWritten int64
}

// Stats records cache use for each repository (the cache_stats table).
// The server calls Add on request paths, so Add must not block for long.
type Stats interface {
	Add(repository string, c Counts)
}

type noStats struct{}

func (noStats) Add(string, Counts) {}
