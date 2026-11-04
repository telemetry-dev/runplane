package sysbox

import (
	"math"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

func TestCPUPercent(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	prev := cpuSample{at: t0, usage: 1_000_000_000}
	cases := []struct {
		name string
		cur  cpuSample
		vcpu int
		want float64
	}{
		{"one of two CPUs busy", cpuSample{at: t0.Add(5 * time.Second), usage: 6_000_000_000}, 2, 50},
		{"more than the allocation", cpuSample{at: t0.Add(5 * time.Second), usage: 21_000_000_000}, 2, 100},
		{"counter reset", cpuSample{at: t0.Add(5 * time.Second), usage: 10}, 2, 0},
		{"same time", cpuSample{at: t0, usage: 2_000_000_000}, 2, 0},
	}
	for _, c := range cases {
		if got := cpuPercent(prev, c.cur, c.vcpu); math.Abs(got-c.want) > 0.001 {
			t.Errorf("%s: cpuPercent = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMemoryMB(t *testing.T) {
	cases := []struct {
		name string
		m    container.MemoryStats
		want int
	}{
		{"cgroup v2", container.MemoryStats{Usage: 600 << 20, Stats: map[string]uint64{"inactive_file": 100 << 20}}, 500},
		{"cgroup v1", container.MemoryStats{Usage: 600 << 20, Stats: map[string]uint64{"total_inactive_file": 200 << 20}}, 400},
		{"no stats", container.MemoryStats{Usage: 300 << 20}, 300},
	}
	for _, c := range cases {
		if got := memoryMB(c.m); got != c.want {
			t.Errorf("%s: memoryMB = %d, want %d", c.name, got, c.want)
		}
	}
}
