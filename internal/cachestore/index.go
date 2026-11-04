// Package cachestore keeps the data of the Actions cache server in the
// runplane database: the entries (cache.Index), the use counts
// (cache.Stats), and the sandbox behind each request (cache.Resolver).
package cachestore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/cache"
	"github.com/runplane/runplane/internal/store"
)

// Index is the cache_entries table as a cache.Index.
type Index struct {
	st *store.Store
}

var _ cache.Index = Index{}

func NewIndex(st *store.Store) Index { return Index{st: st} }

func (x Index) Get(ctx context.Context, id string) (cache.Entry, error) {
	e, err := x.st.GetCacheEntry(ctx, id)
	return entry(e), mapErr(err)
}

func (x Index) Find(ctx context.Context, repository, ref, key, version string) (cache.Entry, error) {
	e, err := x.st.GetCacheEntryByKey(ctx, repository, ref, key, version)
	return entry(e), mapErr(err)
}

func (x Index) Lookup(ctx context.Context, q cache.Query) (cache.Entry, error) {
	e, err := x.st.MatchCacheEntry(ctx, q.Repository, q.Version, []string{q.Ref}, q.Key, q.Prefix)
	return entry(e), mapErr(err)
}

func (x Index) Create(ctx context.Context, e cache.Entry) error {
	return mapErr(x.st.CreateCacheEntry(ctx, store.CacheEntry{
		ID: e.ID, Repository: e.Repository, Ref: e.Ref, Key: e.Key, Version: e.Version, SizeBytes: e.SizeBytes,
		Path: e.Path, Finalized: e.Finalized, CreatedAt: e.CreatedAt, LastUsedAt: e.LastUsedAt,
	}))
}

func (x Index) Finalize(ctx context.Context, id string, size int64, at time.Time) error {
	return mapErr(x.st.FinishCacheEntry(ctx, id, size, at))
}

func (x Index) Touch(ctx context.Context, id string, at time.Time) error {
	return mapErr(x.st.TouchCacheEntry(ctx, id, at))
}

func (x Index) Delete(ctx context.Context, id string) error {
	list, err := x.st.DeleteCacheEntries(ctx, "", id)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return cache.ErrNotFound
	}
	return nil
}

func (x Index) List(ctx context.Context, repository string) ([]cache.Entry, error) {
	// The store reads all repositories for "", and the cache server deletes
	// what List returns.
	if repository == "" {
		return nil, nil
	}
	list, err := x.st.ListCacheEntries(ctx, repository)
	if err != nil {
		return nil, err
	}
	out := entries(list)
	slices.SortFunc(out, func(a, b cache.Entry) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), strings.Compare(b.ID, a.ID))
	})
	return out, nil
}

func (x Index) Usage(ctx context.Context) ([]cache.Usage, error) {
	list, err := x.st.CacheUsage(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]cache.Usage, len(list))
	for i, u := range list {
		out[i] = cache.Usage{Repository: u.Repository, Entries: u.Entries, Bytes: u.SizeBytes}
	}
	return out, nil
}

func (x Index) Oldest(ctx context.Context, limit int) ([]cache.Entry, error) {
	list, err := x.st.LeastRecentCacheEntries(ctx, limit)
	return entries(list), err
}

func (x Index) Unfinished(ctx context.Context, before time.Time) ([]cache.Entry, error) {
	list, err := x.st.ListUnfinishedCacheEntries(ctx, before)
	return entries(list), err
}

func entry(e store.CacheEntry) cache.Entry {
	return cache.Entry{
		ID: e.ID, Repository: e.Repository, Ref: e.Ref, Key: e.Key, Version: e.Version, SizeBytes: e.SizeBytes,
		Path: e.Path, Finalized: e.Finalized, CreatedAt: e.CreatedAt, LastUsedAt: e.LastUsedAt,
	}
}

func entries(list []store.CacheEntry) []cache.Entry {
	out := make([]cache.Entry, len(list))
	for i, e := range list {
		out[i] = entry(e)
	}
	return out
}

// mapErr turns the store errors into the errors that the cache server checks.
func mapErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("%w: %w", cache.ErrNotFound, err)
	case errors.Is(err, store.ErrConflict):
		return fmt.Errorf("%w: %w", cache.ErrExists, err)
	}
	return err
}
