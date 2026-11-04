package api

import (
	"context"
	"testing"

	"github.com/runplane/runplane/internal/auth"
)

// fakeCaches is a cache server whose limit is a quarter of a small disk.
type fakeCaches struct {
	Caches
	limit int64
}

func (f fakeCaches) MaxBytes() int64 { return f.limit }

func (f fakeCaches) MirrorStats(context.Context) (MirrorStats, error) { return MirrorStats{}, nil }

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
