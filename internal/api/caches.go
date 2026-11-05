package api

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/store"
)

type CacheOverview struct {
	Enabled       bool              `json:"enabled" doc:"The actions_cache setting."`
	SizeBytes     int64             `json:"size_bytes" doc:"Size of all entries, also the unfinished uploads."`
	MaxBytes      int64             `json:"max_bytes" doc:"The limit of the cache: the cache_max_gb setting in bytes, at most a quarter of the disk of the cache. runplane removes the least recently used entries above it."`
	HitRate       float64           `json:"hit_rate" doc:"Hits / (hits + misses) of the last 7 days, 0 to 1."`
	Repositories  []CacheRepository `json:"repositories" doc:"Largest first."`
	MirrorEnabled bool              `json:"mirror_enabled" doc:"The registry_mirror setting."`
	Mirror        *MirrorStats      `json:"mirror,omitempty" doc:"Absent when the mirror stats are not available."`
}

type CacheRepository struct {
	Repository string     `json:"repository"`
	Entries    int        `json:"entries"`
	SizeBytes  int64      `json:"size_bytes"`
	HitRate    float64    `json:"hit_rate" doc:"Hits / (hits + misses) of the last 7 days, 0 to 1."`
	Hits       int64      `json:"hits" doc:"Of the last 7 days."`
	Misses     int64      `json:"misses" doc:"Of the last 7 days."`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// MirrorStats are the counts of the registry mirrors of all servers since the
// start of runplane.
type MirrorStats struct {
	Hits      int64     `json:"hits"`
	Misses    int64     `json:"misses"`
	HitBytes  int64     `json:"hit_bytes" doc:"Bytes sent from the mirror."`
	MissBytes int64     `json:"miss_bytes" doc:"Bytes fetched from the upstream registry."`
	SizeBytes int64     `json:"size_bytes" doc:"Bytes in the mirror now."`
	StartedAt time.Time `json:"started_at" doc:"Start of runplane, where the counts start."`
}

const cacheStatsWindow = 7 * 24 * time.Hour

func (h *Handler) getCaches(ctx context.Context, _ *struct{}) (*body[CacheOverview], error) {
	st := h.d.Store
	settings, err := st.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	usage, err := st.CacheUsage(ctx)
	if err != nil {
		return nil, err
	}
	stats, err := st.CacheStatsByRepository(ctx, time.Now().Add(-cacheStatsWindow))
	if err != nil {
		return nil, err
	}
	size, err := st.CacheSize(ctx)
	if err != nil {
		return nil, err
	}
	out := CacheOverview{
		Enabled: settings.ActionsCache, SizeBytes: size, MaxBytes: int64(settings.CacheMaxGB) << 30,
		MirrorEnabled: settings.RegistryMirror, Repositories: []CacheRepository{},
	}
	// A repository with hits in the window but no entries now also shows.
	repos := map[string]*CacheRepository{}
	for _, u := range usage {
		repos[u.Repository] = &CacheRepository{Repository: u.Repository, Entries: u.Entries, SizeBytes: u.SizeBytes, LastUsedAt: u.LastUsedAt}
	}
	var hits, misses int64
	for repo, c := range stats {
		r := repos[repo]
		if r == nil {
			r = &CacheRepository{Repository: repo}
			repos[repo] = r
		}
		r.Hits, r.Misses, r.HitRate = c.Hits, c.Misses, rate(c.Hits, c.Misses)
		hits, misses = hits+c.Hits, misses+c.Misses
	}
	out.HitRate = rate(hits, misses)
	for _, r := range repos {
		out.Repositories = append(out.Repositories, *r)
	}
	slices.SortFunc(out.Repositories, func(a, b CacheRepository) int {
		return cmp.Or(cmp.Compare(b.SizeBytes, a.SizeBytes), strings.Compare(a.Repository, b.Repository))
	})
	if h.d.Caches != nil {
		out.MaxBytes = h.d.Caches.MaxBytes()
		m, err := h.d.Caches.MirrorStats(ctx)
		if err != nil {
			h.d.Log.Warn("read mirror stats", "err", err)
		} else {
			m.StartedAt = h.started
			out.Mirror = &m
		}
	}
	return &body[CacheOverview]{out}, nil
}

func rate(hits, misses int64) float64 {
	if hits+misses == 0 {
		return 0
	}
	return float64(hits) / float64(hits+misses)
}

type CacheEntry struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"`
	Ref        string    `json:"ref" doc:"For example refs/heads/main."`
	Key        string    `json:"key"`
	Version    string    `json:"version"`
	SizeBytes  int64     `json:"size_bytes"`
	Finalized  bool      `json:"finalized" doc:"False while the upload runs."`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

type cacheEntriesInput struct {
	Repository string `query:"repository" maxLength:"200" doc:"owner/name. All repositories if absent."`
}

const maxCacheEntries = 1000

func (h *Handler) listCacheEntries(ctx context.Context, in *cacheEntriesInput) (*body[[]CacheEntry], error) {
	list, err := h.d.Store.ListCacheEntries(ctx, in.Repository)
	if err != nil {
		return nil, err
	}
	list = list[:min(len(list), maxCacheEntries)]
	out := make([]CacheEntry, len(list))
	for i, e := range list {
		out[i] = CacheEntry{
			ID: e.ID, Repository: e.Repository, Ref: e.Ref, Key: e.Key, Version: e.Version, SizeBytes: e.SizeBytes,
			Finalized: e.Finalized, CreatedAt: e.CreatedAt, LastUsedAt: e.LastUsedAt,
		}
	}
	return &body[[]CacheEntry]{out}, nil
}

type deleteCachesInput struct {
	Repository string `query:"repository" maxLength:"200" doc:"Remove all entries of this repository."`
	ID         string `query:"id" maxLength:"100" doc:"Remove this entry."`
}

type DeletedCaches struct {
	Deleted int `json:"deleted"`
}

func (h *Handler) deleteCacheEntries(ctx context.Context, in *deleteCachesInput) (*body[DeletedCaches], error) {
	if in.Repository == "" && in.ID == "" {
		return nil, invalid("query.repository", "Give a repository or an entry ID.", nil)
	}
	if h.d.Caches == nil {
		return nil, unavailable("The cache server")
	}
	n, err := h.d.Caches.Delete(ctx, in.Repository, in.ID)
	if err != nil {
		return nil, h.serviceErr(err)
	}
	h.audit(ctx, "cache.delete", cmp.Or(in.ID, in.Repository), map[string]any{"repository": in.Repository, "id": in.ID, "deleted": n})
	return &body[DeletedCaches]{DeletedCaches{Deleted: n}}, nil
}

type RunnerVersion struct {
	Version    string     `json:"version" doc:"For example 2.330.0."`
	Arch       string     `json:"arch" enum:"x64,arm64"`
	Status     string     `json:"status" enum:"pending,ready,failed"`
	Patched    bool       `json:"patched" doc:"The runner has the patch for the local Actions cache."`
	SHA256     string     `json:"sha256"`
	Error      string     `json:"error"`
	EndDate    *time.Time `json:"end_date" doc:"GitHub stops sending jobs to this version after this time."`
	EndsSoon   bool       `json:"ends_soon" doc:"The end date is in less than 14 days."`
	ReleasedAt *time.Time `json:"released_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type RunnerVersions struct {
	Latest  string          `json:"latest" doc:"Highest known version. Empty before the first check."`
	Current RunnerCurrent   `json:"current" doc:"The version that new sandboxes get on each arch: the highest ready version."`
	Items   []RunnerVersion `json:"items" doc:"Newest version first."`
}

// RunnerCurrent has a version for each arch. Empty: no version is ready.
type RunnerCurrent struct {
	X64   string `json:"x64"`
	ARM64 string `json:"arm64"`
}

const endsSoon = 14 * 24 * time.Hour

func runnerVersion(b store.RunnerBundle, now time.Time) RunnerVersion {
	return RunnerVersion{
		Version: b.Version, Arch: b.Arch, Status: b.Status, Patched: b.Patched, SHA256: b.SHA256, Error: b.Error,
		EndDate: b.EndDate, EndsSoon: b.EndDate != nil && b.EndDate.Sub(now) < endsSoon,
		ReleasedAt: b.ReleasedAt, CreatedAt: b.CreatedAt,
	}
}

func (h *Handler) listRunnerVersions(ctx context.Context, _ *struct{}) (*body[RunnerVersions], error) {
	list, err := h.d.Store.ListRunnerBundles(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := RunnerVersions{Items: make([]RunnerVersion, len(list))}
	for i, b := range list {
		out.Items[i] = runnerVersion(b, now)
		if b.Status != store.BundleReady {
			continue
		}
		// The list is newest first, so the first ready bundle of an arch is current.
		switch {
		case b.Arch == "x64" && out.Current.X64 == "":
			out.Current.X64 = b.Version
		case b.Arch == "arm64" && out.Current.ARM64 == "":
			out.Current.ARM64 = b.Version
		}
	}
	if len(list) > 0 {
		out.Latest = list[0].Version
	}
	return &body[RunnerVersions]{out}, nil
}
