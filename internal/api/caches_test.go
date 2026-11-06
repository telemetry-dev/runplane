package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/store"
)

// fakeCaches is a cache server whose limit is a quarter of a small disk.
type fakeCaches struct {
	Caches
	limit int64
}

func (f fakeCaches) MaxBytes() int64 { return f.limit }

type fakeMirrors mirror.Stats

func (f fakeMirrors) MirrorStats(context.Context) mirror.Stats { return mirror.Stats(f) }

func TestCachesReportTheMirrorsOfAllServers(t *testing.T) {
	e := newEnv(t, func(d *Deps) {
		d.Mirrors = fakeMirrors{Hits: 6, Misses: 5, HitBytes: 600, MissBytes: 500, SizeBytes: 1300}
	})
	var out CacheOverview
	e.login("ada@example.com", auth.RoleMember).do("GET", "/api/caches", nil, &out).want(t, 200)
	want := MirrorStats{Hits: 6, Misses: 5, HitBytes: 600, MissBytes: 500, SizeBytes: 1300}
	if m := out.Mirror; m == nil || m.StartedAt.IsZero() {
		t.Fatalf("mirror %+v, want the counts since the start", m)
	}
	if r := out.Mirror.HitRate; r == nil || *r != 6.0/11 {
		t.Errorf("mirror hit rate %v, want 6/11", r)
	}
	want.StartedAt, want.HitRate = out.Mirror.StartedAt, out.Mirror.HitRate
	if *out.Mirror != want {
		t.Errorf("mirror %+v, want %+v", *out.Mirror, want)
	}
}

func TestCachesReportTheLimitOfTheCacheServer(t *testing.T) {
	// cache_max_gb is 50 by default, but a 40 GB disk allows 10 GB.
	e := newEnv(t, func(d *Deps) { d.Caches = fakeCaches{limit: 10 << 30} })
	b := e.login("ada@example.com", auth.RoleMember)
	var out CacheOverview
	b.do("GET", "/api/caches", nil, &out).want(t, 200)
	if out.MaxBytes != 10<<30 {
		t.Fatalf("max_bytes = %d, want %d, the limit of the cache server", out.MaxBytes, int64(10<<30))
	}
}

// deletingCaches is a cache server with the entries of one repository.
type deletingCaches struct {
	Caches
	ids  map[string]bool
	repo map[string]int
}

func (f deletingCaches) Delete(_ context.Context, id string) error {
	if !f.ids[id] {
		return fmt.Errorf("get entry: %w", store.ErrNotFound)
	}
	return nil
}

func (f deletingCaches) DeleteRepository(_ context.Context, repository string) (int, error) {
	return f.repo[repository], nil
}

func TestDeleteCaches(t *testing.T) {
	caches := deletingCaches{ids: map[string]bool{"e1": true}, repo: map[string]int{"acme/app": 3}}
	e := newEnv(t, func(d *Deps) { d.Caches = caches })
	b := e.login("ada@example.com", auth.RoleAdmin)
	tests := []struct {
		query  string
		status int
		want   int
	}{
		{"?id=e1", 200, 1},
		{"?id=e1&repository=other/lib", 200, 1}, // the ID wins
		{"?id=gone", 404, 0},
		{"?repository=acme/app", 200, 3},
	}
	for _, tt := range tests {
		var out DeletedCaches
		b.do("DELETE", "/api/caches"+tt.query, nil, &out).want(t, tt.status)
		if out.Deleted != tt.want {
			t.Errorf("DELETE %s: deleted %d, want %d", tt.query, out.Deleted, tt.want)
		}
	}
	b.do("DELETE", "/api/caches", nil, nil).wantField(t, 422, "query.repository")
}

func TestRunnerVersionsCurrentIsTheNewestReady(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, b := range []store.RunnerBundle{
		{Version: "2.331.0", Arch: "x64", Status: store.BundleFailed},
		{Version: "2.330.0", Arch: "x64", Status: store.BundleReady},
		{Version: "2.99.0", Arch: "x64", Status: store.BundleReady},
		{Version: "2.331.0", Arch: "arm64", Status: store.BundlePending},
	} {
		must(t, e.st.SetRunnerBundleBuild(ctx, b, time.Now()))
	}
	var got RunnerVersions
	e.login("ada@example.com", auth.RoleMember).do("GET", "/api/runner-versions", nil, &got).want(t, 200)
	if got.Latest != "2.331.0" || got.Current != (RunnerCurrent{X64: "2.330.0"}) {
		t.Fatalf("latest %q, current %+v; want 2.331.0 and x64 2.330.0 only", got.Latest, got.Current)
	}
}
