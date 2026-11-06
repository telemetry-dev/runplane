package api

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/runplane/runplane/internal/store"
)

// SizeAdvice tells whether the pool of a job fits its CPU and memory use.
type SizeAdvice struct {
	Kind   string `json:"kind" enum:"larger,smaller,none" doc:"larger: the job uses almost all of its memory or CPU. smaller: the job uses less than 40% of both."`
	Label  string `json:"label" doc:"The smallest enabled pool with the same arch that fits the job with 30% headroom. Empty for kind none, or when no pool fits."`
	Reason string `json:"reason"`
}

const (
	adviceLarger  = "larger"
	adviceSmaller = "smaller"
	adviceNone    = "none"
)

const (
	adviceRuns    = 10   // the last completed runs that count
	adviceMinRuns = 3    // fewer runs give no advice
	largerMemory  = 0.90 // of the memory, at the peak
	largerCPU     = 0.85 // of the vCPUs, on average over the job
	smallerShare  = 0.40 // of the memory and the vCPUs, at the peak
	headroom      = 0.70 // a pool that fits uses at most this share
)

// sizeAdvice compares the use of the runs of a job with its pool and picks
// a pool from pools that fits better. A share is the 90th percentile of the
// runs (nearest rank), in vCPUs or MB, against the size of pool:
//
//   - larger: memory peak above 90%, or CPU average above 85%. The new pool
//     is not smaller than pool and keeps 30% headroom on each resource that
//     is short.
//   - smaller: CPU peak and memory peak below 40%. The new pool is smaller
//     than pool and keeps 30% headroom on the two peaks.
func sizeAdvice(runs []store.RunUsage, pool *store.Pool, pools []store.Pool) SizeAdvice {
	if pool == nil {
		return SizeAdvice{Kind: adviceNone, Reason: "The pool of this job no longer exists."}
	}
	runs = slices.DeleteFunc(slices.Clone(runs), func(r store.RunUsage) bool { return r.VCPU <= 0 || r.MemoryMB <= 0 })
	if len(runs) < adviceMinRuns {
		return SizeAdvice{Kind: adviceNone, Reason: fmt.Sprintf(
			"Size advice needs %d completed runs with CPU and memory samples. This job has %d.", adviceMinRuns, len(runs))}
	}
	var cpuPeaks, cpuAvgs, mems []float64
	for _, r := range runs {
		cpuPeaks = append(cpuPeaks, r.CPUPeak/100*float64(r.VCPU))
		cpuAvgs = append(cpuAvgs, r.CPUAvg/100*float64(r.VCPU))
		mems = append(mems, float64(r.MemPeakMB))
	}
	cpuPeak, cpuAvg, mem := p90(cpuPeaks), p90(cpuAvgs), p90(mems)
	vcpu, memory := float64(pool.VCPU), float64(pool.MemoryMB)
	of := fmt.Sprintf(" (90th percentile of the last %d runs).", len(runs))

	memShort, cpuShort := mem > largerMemory*memory, cpuAvg > largerCPU*vcpu
	if memShort || cpuShort {
		var why []string
		if memShort {
			why = append(why, fmt.Sprintf("Memory peaked at %s of %s", gb(mem), gb(memory)))
		}
		if cpuShort {
			why = append(why, fmt.Sprintf("CPU use averaged %.0f%% of %d vCPUs", 100*cpuAvg/vcpu, pool.VCPU))
		}
		label := smallest(pools, pool, func(p store.Pool) bool {
			return p.VCPU >= pool.VCPU && p.MemoryMB >= pool.MemoryMB &&
				(!memShort || mem <= headroom*float64(p.MemoryMB)) && (!cpuShort || cpuAvg <= headroom*float64(p.VCPU))
		})
		return SizeAdvice{Kind: adviceLarger, Label: label, Reason: strings.Join(why, " and ") + of + noPool(label, pool)}
	}
	if cpuPeak < smallerShare*vcpu && mem < smallerShare*memory {
		label := smallest(pools, pool, func(p store.Pool) bool {
			return p.VCPU <= pool.VCPU && p.MemoryMB <= pool.MemoryMB && (p.VCPU < pool.VCPU || p.MemoryMB < pool.MemoryMB) &&
				cpuPeak <= headroom*float64(p.VCPU) && mem <= headroom*float64(p.MemoryMB)
		})
		why := fmt.Sprintf("CPU use peaked at %.0f%% of %d vCPUs and memory at %s of %s", 100*cpuPeak/vcpu, pool.VCPU, gb(mem), gb(memory))
		return SizeAdvice{Kind: adviceSmaller, Label: label, Reason: why + of + noPool(label, pool)}
	}
	return SizeAdvice{Kind: adviceNone, Reason: "The CPU and memory use fit the pool" + of}
}

// p90 is the 90th percentile with the nearest-rank method.
func p90(values []float64) float64 {
	sorted := slices.Sorted(slices.Values(values))
	return sorted[int(math.Ceil(0.9*float64(len(sorted))))-1]
}

// smallest returns the label of the smallest enabled pool, other than
// current and with its arch, for which fits is true.
func smallest(pools []store.Pool, current *store.Pool, fits func(store.Pool) bool) string {
	var best *store.Pool
	for i, p := range pools {
		if !p.Enabled || p.ID == current.ID || p.Arch != current.Arch || !fits(p) {
			continue
		}
		if best == nil || cmp.Or(cmp.Compare(p.VCPU, best.VCPU), cmp.Compare(p.MemoryMB, best.MemoryMB), strings.Compare(p.Label, best.Label)) < 0 {
			best = &pools[i]
		}
	}
	if best == nil {
		return ""
	}
	return best.Label
}

func noPool(label string, pool *store.Pool) string {
	if label != "" {
		return ""
	}
	return fmt.Sprintf(" No enabled %s pool fits with 30%% headroom.", pool.Arch)
}

func gb(mb float64) string { return fmt.Sprintf("%.1f GB", mb/1024) }
