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

	refs := []string{"refs/pull/1/merge", "refs/heads/main"}
	tests := []struct {
		name   string
		key    string
		prefix bool
		refs   []string
		want   string
	}{
		{"first ref wins over newer entries in later refs", "npm-linux-", true, refs, "pr"},
		{"newest entry in the ref", "npm-linux-", true, []string{"refs/heads/main"}, "main-new"},
		{"exact key falls back to the next ref", "npm-linux-aaa", false, refs, "main-old"},
		{"exact key does not match a prefix", "npm-linux", false, refs, ""},
		{"prefix is case-sensitive", "NPM-", true, refs, "upper"},
		{"unfinished upload does not match", "npm-linux-ddd", false, refs, ""},
		{"underscore is not a wildcard", "npm_linux", true, refs, ""},
	}
	for _, tt := range tests {
		e, err := s.MatchCacheEntry(ctx, "o/r", "v1", tt.refs, tt.key, tt.prefix)
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
