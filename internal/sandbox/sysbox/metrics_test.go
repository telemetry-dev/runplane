package sysbox

import (
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/runplane/runplane/internal/sandbox/lifecycle"
)

func TestUsage(t *testing.T) {
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		stats map[string]uint64
		cache int64
	}{
		{"cgroup v2", map[string]uint64{"inactive_file": 100 << 20}, 100 << 20},
		{"cgroup v1", map[string]uint64{"total_inactive_file": 200 << 20}, 200 << 20},
		{"no stats", nil, 0},
	}
	for _, c := range cases {
		var st container.StatsResponse
		st.Read = at
		st.CPUStats.CPUUsage.TotalUsage = 6_000_000_000
		st.MemoryStats = container.MemoryStats{Usage: 600 << 20, Stats: c.stats}
		want := lifecycle.Usage{At: at, CPU: 6 * time.Second, Memory: 600 << 20, Cache: c.cache}
		if got := usage(st); got != want {
			t.Errorf("%s: usage %+v, want %+v", c.name, got, want)
		}
	}
}
