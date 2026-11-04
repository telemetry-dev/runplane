//go:build devseed

package devseed

import (
	"context"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/store"
)

// missShare is the share of cache lookups that miss: the designs show a hit
// rate of 92%.
const missShare = 0.08

func (s *seeder) cacheEntries(ctx context.Context) error {
	for i, c := range cacheSpecs {
		created := s.now.Add(-c.age)
		id := s.id(created)
		err := s.st.CreateCacheEntry(ctx, store.CacheEntry{
			ID: id, Repository: c.repo, Ref: c.ref, Key: c.prefix + cacheHash(c.repo, c.ref),
			Version: lockHash("version", c.prefix), SizeBytes: c.mb<<20 + s.rng.Int64N(1<<20),
			Path: "blobs/" + id, Finalized: true, CreatedAt: created,
			LastUsedAt: later(created, s.now.Add(-time.Duration(4+7*i)*time.Minute)),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// cacheHash is the lockfile hash in the cache keys of a repository. The logs
// show the hash of the default branch.
func cacheHash(repo, ref string) string {
	if ref == "refs/heads/main" {
		return lockHash(repo)
	}
	return lockHash(repo, ref)
}

// cacheStats counts one cache lookup for each cache step of each job that
// started, in the hour of its start. Every 12th or 13th lookup misses, so
// each range of the overview shows a hit rate close to 92%.
func (s *seeder) cacheStats(ctx context.Context, jobs []*job) error {
	type hourRepo struct {
		hour time.Time
		repo string
	}
	size := map[string]int64{}
	for _, c := range cacheSpecs {
		if strings.HasSuffix(c.ref, "/main") && size[c.repo] == 0 {
			size[c.repo] = c.mb << 20
		}
	}
	counts := map[hourRepo]store.CacheCounts{}
	miss := 0.0
	for _, j := range jobs {
		if j.started.IsZero() {
			continue
		}
		for _, st := range j.kind.steps {
			if !st.cache {
				continue
			}
			key := hourRepo{j.started.Truncate(time.Hour), j.run.repo.name}
			c := counts[key]
			if miss += missShare; miss >= 1 {
				miss--
				c.Misses++
				c.BytesWritten += size[key.repo]
			} else {
				c.Hits++
				c.BytesRead += size[key.repo]
			}
			counts[key] = c
		}
	}
	for k, c := range counts {
		if err := s.st.AddCacheStats(ctx, k.hour, k.repo, c); err != nil {
			return err
		}
	}
	return nil
}
