package store

import (
	"errors"
	"testing"
)

func TestMatchCacheEntry(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	add := func(id, ref, key string, created int, finalized bool) {
		t.Helper()
		err := s.CreateCacheEntry(ctx, CacheEntry{ID: id, Repository: "o/r", Ref: ref, Key: key, Version: "v1",
			Finalized: finalized, CreatedAt: sec(created), LastUsedAt: t0})
		must(t, err)
	}
	add("main-old", "refs/heads/main", "npm-linux-aaa", 1, true)
	add("main-new", "refs/heads/main", "npm-linux-bbb", 2, true)
	add("pr", "refs/pull/1/merge", "npm-linux-ccc", 0, true)
	add("pr-upload", "refs/pull/1/merge", "npm-linux-ddd", 9, false)
	add("upper", "refs/pull/1/merge", "NPM-linux-eee", 8, true)

	const pr, main = "refs/pull/1/merge", "refs/heads/main"
	tests := []struct {
		name   string
		key    string
		prefix bool
		ref    string
		want   string
	}{
		{"newest entry in the ref", "npm-linux-", true, main, "main-new"},
		{"an entry of another ref does not match", "npm-linux-aaa", false, pr, ""},
		{"exact key", "npm-linux-aaa", false, main, "main-old"},
		{"exact key does not match a prefix", "npm-linux", false, main, ""},
		{"prefix is case-sensitive", "NPM-", true, pr, "upper"},
		{"unfinished upload does not match", "npm-linux-ddd", false, pr, ""},
		{"underscore is not a wildcard", "npm_linux", true, pr, ""},
	}
	for _, tt := range tests {
		e, err := s.MatchCacheEntry(ctx, "o/r", "v1", tt.ref, tt.key, tt.prefix)
		if tt.want == "" {
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("%s: got %q, %v; want ErrNotFound", tt.name, e.ID, err)
			}
			continue
		}
		if err != nil || e.ID != tt.want {
			t.Errorf("%s: got %q, %v; want %q", tt.name, e.ID, err, tt.want)
		}
	}

	err := s.CreateCacheEntry(ctx, CacheEntry{ID: "dup", Repository: "o/r", Ref: "refs/heads/main",
		Key: "npm-linux-aaa", Version: "v1", CreatedAt: t0, LastUsedAt: t0})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate key: err = %v, want ErrConflict", err)
	}
}

// The cache server keeps the entries and the unfinished uploads under one
// limit, and the dashboard shows the same two sizes.
func TestCacheSize(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	for _, id := range []string{"done", "upload"} {
		must(t, s.CreateCacheEntry(ctx, CacheEntry{ID: id, Repository: "o/r", Ref: "refs/heads/main", Key: id, Version: "v1",
			CreatedAt: t0, LastUsedAt: t0}))
	}
	must(t, s.FinishCacheEntry(ctx, "done", 10, t0))
	must(t, s.SetCacheUpload(ctx, "upload", 5, t0))
	if err := s.SetCacheUpload(ctx, "done", 99, t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetCacheUpload of a finalized entry: err = %v, want ErrNotFound", err)
	}
	got, err := s.CacheSize(ctx)
	must(t, err)
	if got != (CacheBytes{Entries: 10, Uploads: 5}) {
		t.Errorf("CacheSize = %+v, want 10 bytes of entries and 5 of uploads", got)
	}
}
