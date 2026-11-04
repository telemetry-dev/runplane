//go:build devseed

package devseed

import (
	"context"
	"math"
	"time"

	"github.com/runplane/runplane/internal/store"
)

// designMetrics is the CPU and memory summary that a design shows for a job.
type designMetrics struct {
	cpuPeak, cpuAvg float64
	memMB           int
}

// recentJobs is the count of the newest jobs that get metrics samples and logs.
const recentJobs = 50

// telemetry writes the metrics of the newest jobs that started, and the logs
// of the completed ones. jobs is in queued order.
func (s *seeder) telemetry(ctx context.Context, jobs []*job) error {
	n := 0
	for i := len(jobs) - 1; i >= 0 && n < recentJobs; i-- {
		j := jobs[i]
		if j.started.IsZero() {
			continue
		}
		n++
		if err := s.st.AppendJobMetrics(ctx, j.id, s.samples(j)); err != nil {
			return err
		}
		if j.result == "" {
			if err := s.st.SetJobLogState(ctx, j.id, store.LogLive); err != nil {
				return err
			}
			continue
		}
		sum, err := s.st.JobMetricsSummary(ctx, j.id)
		if err != nil {
			return err
		}
		if err := s.st.SetJobMetricsSummary(ctx, j.id, sum); err != nil {
			return err
		}
		if err := s.writeLog(j); err != nil {
			return err
		}
		if err := s.st.SetJobLogState(ctx, j.id, store.LogStored); err != nil {
			return err
		}
	}
	return nil
}

// samples returns a sample each 2 seconds from the start of j to its end (or
// now): light load in the setup steps, the load of the job in the main step
// with memory that grows to its peak, and idle after it.
func (s *seeder) samples(j *job) []store.JobMetric {
	end := j.finished
	if end.IsZero() {
		end = s.now
	}
	base := 420 + 260*s.rng.Float64()
	peak := float64(j.kind.memMB) * (0.85 + 0.2*s.rng.Float64())
	if j.design != nil {
		peak = float64(j.design.memMB)
	}
	limit := float64(s.pools[j.kind.pool].MemoryMB)
	from, to := j.main[0], j.main[1]
	var list []store.JobMetric
	for t := j.started; !t.After(end); t = t.Add(2 * time.Second) {
		var cpu, mem float64
		switch {
		case t.Before(from):
			cpu = 15 + 35*s.rng.Float64()
			mem = base + 250*share(t, j.started, from)
		case t.Before(to):
			cpu = j.kind.cpu + 6*s.rng.NormFloat64()
			if s.rng.Float64() < 0.05 {
				cpu += 10 + 15*s.rng.Float64()
			}
			mem = base + 250 + (peak-base-250)*math.Pow(share(t, from, to), 0.7) + 40*s.rng.NormFloat64()
		default:
			cpu = 3 + 5*s.rng.Float64()
			mem = base + 120
		}
		list = append(list, store.JobMetric{
			At: t, CPUPercent: round1(clamp(cpu, 0.5, 100)), MemoryMB: int(clamp(mem, base, limit)),
		})
	}
	if j.design != nil {
		fit(list, from, to, *j.design)
	}
	return list
}

// fit changes the samples of the main step [from, to) so that the CPU peak,
// the CPU average of all samples, and the memory peak are those of the design.
func fit(list []store.JobMetric, from, to time.Time, design designMetrics) {
	var main []int
	top, memTop := -1, -1
	sum := 0.0
	for i, m := range list {
		sum += m.CPUPercent
		if m.At.Before(from) || !m.At.Before(to) {
			continue
		}
		main = append(main, i)
		if top < 0 || m.CPUPercent > list[top].CPUPercent {
			top = i
		}
		if memTop < 0 || m.MemoryMB > list[memTop].MemoryMB {
			memTop = i
		}
	}
	// Move the other main samples so that the average comes out right.
	delta := (design.cpuAvg*float64(len(list)) - sum + list[top].CPUPercent - design.cpuPeak) / float64(len(main)-1)
	for _, i := range main {
		m := &list[i]
		if i != top {
			m.CPUPercent = round1(clamp(m.CPUPercent+delta, 0.5, design.cpuPeak-1))
		}
		m.MemoryMB = min(m.MemoryMB, design.memMB)
	}
	list[top].CPUPercent = design.cpuPeak
	list[memTop].MemoryMB = design.memMB
}

// serverStats writes a host sample each minute for the 24 hours up to the
// last heartbeat of v. The load follows the daily rhythm of the jobs, and
// the last sample is the one that the "Servers" design shows.
func (s *seeder) serverStats(ctx context.Context, v *server) error {
	if v.seen == 0 {
		return nil
	}
	last := s.now.Add(-v.seen)
	var list []store.ServerStat
	for t := last.Add(-24 * time.Hour); t.Before(last); t = t.Add(time.Minute) {
		w := hourWeights[t.Hour()]
		list = append(list, store.ServerStat{
			ServerID:      v.id,
			At:            t,
			CPUPercent:    round1(clamp(4+(v.cpu-4)*w+3*s.rng.NormFloat64(), 0.5, 99)),
			MemoryPercent: round1(clamp(12+(v.mem-12)*w+2*s.rng.NormFloat64(), 5, 99)),
			DiskPercent:   round1(v.disk - 0.6*last.Sub(t).Hours()/24),
		})
	}
	list = append(list, store.ServerStat{ServerID: v.id, At: last, CPUPercent: v.cpu, MemoryPercent: v.mem, DiskPercent: v.disk})
	return s.st.AppendServerStats(ctx, list)
}

// share is the part of [from, to) before t, from 0 to 1.
func share(t, from, to time.Time) float64 {
	if !to.After(from) {
		return 1
	}
	return clamp(float64(t.Sub(from))/float64(to.Sub(from)), 0, 1)
}

func clamp(v, lo, hi float64) float64 { return min(max(v, lo), hi) }

func round1(v float64) float64 { return math.Round(v*10) / 10 }
