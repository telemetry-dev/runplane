package host

import (
	"context"
	"math"
	"runtime"
	"strings"
	"testing"
	"time"
)

func readStat(t *testing.T, file string) cpuTimes {
	t.Helper()
	c, err := parseStat(open(t, "testdata/proc/"+file))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCPUPercent(t *testing.T) {
	before, after := readStat(t, "stat-before"), readStat(t, "stat-after")
	// Deltas: user 600, system 300, idle 800, iowait 200, softirq 50,
	// steal 50, guest 100 (already in user). Busy 1000 of 2000.
	if got := cpuPercent(before, after); got != 50 {
		t.Errorf("got %v, want 50", got)
	}
	// Counters go back after a reboot or a wrong order; no huge number.
	if got := cpuPercent(after, before); got != 0 {
		t.Errorf("backwards: got %v, want 0", got)
	}
	if got := cpuPercent(after, after); got != 0 {
		t.Errorf("no time passed: got %v, want 0", got)
	}
}

func TestParseStatRejectsOtherFirstLine(t *testing.T) {
	if _, err := parseStat(strings.NewReader("intr 1 2 3\n")); err == nil {
		t.Error("want an error")
	}
}

func TestMemoryPercent(t *testing.T) {
	m, err := parseMeminfo(open(t, "testdata/proc/meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	// (16422380 - 13449000) / 16422380 = 18.106 %
	if got := percent(m.totalKB-m.availableKB, m.totalKB); math.Abs(got-18.106) > 0.001 {
		t.Errorf("got %v, want 18.106", got)
	}
}

func TestSamplerRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got []Stats
	err := Sampler{DataDir: t.TempDir(), Interval: 20 * time.Millisecond}.Run(ctx, func(s Stats) {
		got = append(got, s)
		if len(got) == 2 {
			cancel()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2", len(got))
	}
	s := got[1]
	if s.VCPU != runtime.NumCPU() || s.MemoryMB <= 0 || s.DiskBytes <= 0 || s.FreeDiskBytes > s.DiskBytes {
		t.Errorf("bad totals: %+v", s.Resources)
	}
	for name, p := range map[string]float64{"cpu": s.CPUPercent, "memory": s.MemoryPercent, "disk": s.DiskPercent} {
		if p < 0 || p > 100 {
			t.Errorf("%s percent %v is outside 0 to 100", name, p)
		}
	}
	if runtime.GOOS == "linux" && (s.MemoryPercent == 0 || s.DiskPercent == 0) {
		t.Errorf("Linux reports no use: %+v", s)
	}
}
