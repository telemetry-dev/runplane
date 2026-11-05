package datadisk

import "testing"

func TestLimits(t *testing.T) {
	const gib = 1 << 30
	tests := []struct {
		name          string
		s             Settings
		total, free   int64
		cache, mirror int64
		low           bool
		reclaim       int64
	}{
		{"large disk", Settings{CacheMaxGB: 50, MinFreePercent: 15}, 1000 * gib, 900 * gib, 50 * gib, 50 * gib, false, 0},
		{"a quarter of a small disk", Settings{CacheMaxGB: 50, MinFreePercent: 15}, 40 * gib, 30 * gib, 10 * gib, 4 * gib, false, 0},
		{"the mirror gets 2 GiB at least", Settings{CacheMaxGB: 1}, 10 * gib, 10 * gib, 1 * gib, 2 * gib, false, 0},
		{"no cache_max_gb", Settings{}, 100 * gib, 100 * gib, 25 * gib, 10 * gib, false, 0},
		{"disk size not known", Settings{CacheMaxGB: 50, MinFreePercent: 15}, 0, 0, 50 * gib, 2 * gib, false, 0},
		{"free disk near the minimum", Settings{CacheMaxGB: 50, MinFreePercent: 15}, 100 * gib, 17 * gib, 25 * gib, 10 * gib, false, 3 * gib},
		{"free disk below the minimum", Settings{CacheMaxGB: 50, MinFreePercent: 15}, 100 * gib, 14 * gib, 25 * gib, 10 * gib, true, 6 * gib},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := Limits(tt.s, tt.total, tt.free)
			if sp.Cache != tt.cache || sp.Mirror != tt.mirror || sp.Low() != tt.low || sp.Reclaim() != tt.reclaim {
				t.Errorf("cache %d, mirror %d, low %v, reclaim %d; want %d, %d, %v, %d",
					sp.Cache, sp.Mirror, sp.Low(), sp.Reclaim(), tt.cache, tt.mirror, tt.low, tt.reclaim)
			}
		})
	}
}
