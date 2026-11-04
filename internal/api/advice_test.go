package api

import (
	"testing"

	"github.com/runplane/runplane/internal/store"
)

func TestSizeAdvice(t *testing.T) {
	pool := func(label, arch string, vcpu, memoryMB int, enabled bool) store.Pool {
		return store.Pool{ID: label, Label: label, Arch: arch, VCPU: vcpu, MemoryMB: memoryMB, Enabled: enabled}
	}
	pools := []store.Pool{
		pool("small", "x64", 2, 4096, true),
		pool("medium", "x64", 4, 8192, true),
		pool("large", "x64", 8, 16384, true),
		pool("highmem", "x64", 2, 8192, true),
		pool("arm-tiny", "arm64", 1, 2048, true),
		pool("off", "x64", 1, 2048, false),
	}
	byLabel := func(label string) *store.Pool {
		for i := range pools {
			if pools[i].Label == label {
				return &pools[i]
			}
		}
		return nil
	}
	// runs makes one run for each value of vary on a sandbox of vcpu and
	// memoryMB; vary sets the field that changes.
	runs := func(vcpu, memoryMB int, base store.RunUsage, vary func(*store.RunUsage, float64), values ...float64) []store.RunUsage {
		var out []store.RunUsage
		for _, v := range values {
			r := base
			r.VCPU, r.MemoryMB = vcpu, memoryMB
			vary(&r, v)
			out = append(out, r)
		}
		return out
	}
	mem := func(r *store.RunUsage, v float64) { r.MemPeakMB = int(v) }
	avg := func(r *store.RunUsage, v float64) { r.CPUAvg = v }
	peak := func(r *store.RunUsage, v float64) { r.CPUPeak = v }
	of10 := " (90th percentile of the last 10 runs)."

	tests := []struct {
		name string
		pool string
		runs []store.RunUsage
		want SizeAdvice
	}{
		{
			// p90 of the memory peaks is 3800 MB, more than 90% of 4096 MB.
			// 3800 MB with 30% headroom needs 5429 MB: highmem is the smallest.
			name: "memory near the limit", pool: "small",
			runs: runs(2, 4096, store.RunUsage{CPUPeak: 100, CPUAvg: 50}, mem, 3000, 3100, 3200, 3300, 3400, 3500, 3600, 3700, 3800, 4000),
			want: SizeAdvice{"larger", "highmem", "Memory peaked at 3.7 GB of 4.0 GB" + of10},
		},
		{
			// p90 of the CPU averages is 96% of 2 vCPUs = 1.92 vCPUs, more
			// than 85%. 1.92 vCPUs with 30% headroom needs 2.75 vCPUs: medium.
			name: "CPU bound", pool: "small",
			runs: runs(2, 4096, store.RunUsage{CPUPeak: 100, MemPeakMB: 2000}, avg, 80, 82, 84, 86, 88, 90, 92, 94, 96, 98),
			want: SizeAdvice{"larger", "medium", "CPU use averaged 96% of 2 vCPUs" + of10},
		},
		{
			// Runs on a large sandbox, before the job moved to small: 25% of
			// 8 vCPUs is 2 vCPUs, all of small.
			name: "runs on another size count with their size", pool: "small",
			runs: runs(8, 16384, store.RunUsage{CPUPeak: 50, MemPeakMB: 2000}, avg, 25, 25, 25),
			want: SizeAdvice{"larger", "medium", "CPU use averaged 100% of 2 vCPUs (90th percentile of the last 3 runs)."},
		},
		{
			// p90: CPU peak 28% of 8 vCPUs = 2.24 vCPUs, memory 3600 MB, both
			// below 40%. With 30% headroom: 3.2 vCPUs and 5143 MB, so not
			// small or highmem (2 vCPUs), but medium.
			name: "oversized", pool: "large",
			runs: func() []store.RunUsage {
				list := runs(8, 16384, store.RunUsage{CPUAvg: 10}, peak, 12, 14, 16, 18, 20, 22, 24, 26, 28, 30)
				for i := range list {
					list[i].MemPeakMB = 2000 + 200*i
				}
				return list
			}(),
			want: SizeAdvice{"smaller", "medium", "CPU use peaked at 28% of 8 vCPUs and memory at 3.5 GB of 16.0 GB" + of10},
		},
		{
			// 0.4 vCPUs and 500 MB fit off and arm-tiny, but one is disabled
			// and the other has another arch.
			name: "no smaller pool", pool: "small",
			runs: runs(2, 4096, store.RunUsage{CPUPeak: 20, CPUAvg: 5}, mem, 500, 500, 500),
			want: SizeAdvice{"smaller", "", "CPU use peaked at 20% of 2 vCPUs and memory at 0.5 GB of 4.0 GB (90th percentile of the last 3 runs). No enabled x64 pool fits with 30% headroom."},
		},
		{
			name: "no larger pool", pool: "large",
			runs: runs(8, 16384, store.RunUsage{CPUPeak: 60, CPUAvg: 40}, mem, 16000, 16000, 16000),
			want: SizeAdvice{"larger", "", "Memory peaked at 15.6 GB of 16.0 GB (90th percentile of the last 3 runs). No enabled x64 pool fits with 30% headroom."},
		},
		{
			// One run of ten near the limit is not the 90th percentile.
			name: "one outlier", pool: "small",
			runs: runs(2, 4096, store.RunUsage{CPUPeak: 70, CPUAvg: 50}, mem, 2000, 2000, 2000, 2000, 2000, 2000, 2000, 2000, 2000, 4090),
			want: SizeAdvice{"none", "", "The CPU and memory use fit the pool" + of10},
		},
		{
			// CPU is below 40%, but memory is not.
			name: "memory keeps the size", pool: "medium",
			runs: runs(4, 8192, store.RunUsage{CPUPeak: 20, CPUAvg: 10}, mem, 4000, 4000, 4000),
			want: SizeAdvice{"none", "", "The CPU and memory use fit the pool (90th percentile of the last 3 runs)."},
		},
		{
			name: "too few runs", pool: "small",
			runs: runs(2, 4096, store.RunUsage{CPUPeak: 100, CPUAvg: 95}, mem, 4000, 4000),
			want: SizeAdvice{"none", "", "Size advice needs 3 completed runs with CPU and memory samples. This job has 2."},
		},
		{
			name: "pool deleted", pool: "gone",
			runs: runs(2, 4096, store.RunUsage{}, mem, 1000, 1000, 1000),
			want: SizeAdvice{"none", "", "The pool of this job no longer exists."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sizeAdvice(tt.runs, byLabel(tt.pool), pools); got != tt.want {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}
