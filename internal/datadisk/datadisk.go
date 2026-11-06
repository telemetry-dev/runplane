// Package datadisk shares the data disk between the Actions cache and the
// registry mirror. The images, the runner bundles, and the sandboxes use the
// same disk, so the two caches also leave min_free_disk_percent free.
package datadisk

import "github.com/runplane/runplane/internal/host"

// evictMargin is how far above min_free_disk_percent a cache frees the disk,
// so that the removal does not run again at each write.
const evictMargin = 5

// Settings are the dashboard settings that limit the caches.
type Settings struct {
	CacheMaxGB     int     // cache_max_gb; 0 for no limit of its own
	MinFreePercent float64 // min_free_disk_percent
}

// Space is the room of the two caches on the data disk at one time.
type Space struct {
	Cache  int64 // the byte limit of the Actions cache; 0 for no limit
	Mirror int64 // the byte limit of the registry mirror; 0 for no limit

	total, free int64
	minFree     float64
}

// Of returns the room of the caches on the disk of dir now. A disk whose
// size runplane cannot read gives the caches their room for an unknown disk.
func Of(dir string, s Settings) Space {
	total, free, err := host.DiskSpace(dir)
	if err != nil {
		return Limits(s, 0, 0)
	}
	return Limits(s, total, free)
}

// Limits returns the room of the caches on a data disk of total bytes with
// free bytes free. The cache gets cache_max_gb, at most a quarter of the
// disk. The mirror gets a tenth of the disk, from 2 to 50 GiB. A total of 0
// means that the disk size is not known.
func Limits(s Settings, total, free int64) Space {
	sp := Space{Cache: int64(s.CacheMaxGB) << 30, Mirror: min(max(total/10, 2<<30), 50<<30)}
	if total <= 0 {
		return sp
	}
	if sp.Cache <= 0 || sp.Cache > total/4 {
		sp.Cache = total / 4
	}
	sp.total, sp.free, sp.minFree = total, free, s.MinFreePercent
	return sp
}

// Low reports if the disk has less than min_free_disk_percent free. The
// caches then take no new data.
func (s Space) Low() bool { return s.short(0) > 0 }

// Reclaim returns the bytes that the caches remove, least recently used
// first: what the disk lacks to have 5 points more than
// min_free_disk_percent free.
func (s Space) Reclaim() int64 { return s.short(evictMargin) }

func (s Space) short(margin float64) int64 {
	if s.total <= 0 {
		return 0
	}
	return max(0, int64(float64(s.total)*(s.minFree+margin)/100)-s.free)
}
