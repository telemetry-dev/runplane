package sysbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox"
)

// cpuSample is the CPU time of a container at one moment.
type cpuSample struct {
	at    time.Time
	usage uint64 // nanoseconds of CPU time
}

// sampleMetrics sends the CPU and memory use of each busy sandbox. The CPU
// use is the CPU time since the last sample, so the first sample of a
// sandbox sends nothing.
func (d *Driver) sampleMetrics(ctx context.Context) {
	d.mu.Lock()
	var busy []*box
	for _, b := range d.boxes {
		if b.state == sandbox.StateBusy && b.containerID != "" {
			busy = append(busy, b)
		}
	}
	d.mu.Unlock()
	for _, b := range busy {
		sample, memMB, err := d.stats(ctx, d.containerID(b))
		if err != nil {
			if ctx.Err() == nil {
				d.log.Debug("read container stats", "sandbox", b.id, "err", err)
			}
			continue
		}
		d.mu.Lock()
		prev := b.cpu
		b.cpu = sample
		d.mu.Unlock()
		if prev.at.IsZero() {
			continue
		}
		d.emit(sandbox.Event{
			SandboxID: b.id,
			Type:      sandbox.EventMetrics,
			Time:      sample.at.UTC(),
			Metrics:   &sandbox.Metrics{CPUPercent: cpuPercent(prev, sample, b.spec.VCPU), MemoryMB: memMB},
		})
	}
}

// stats reads one sample of Docker stats. It reads cgroup files on the host
// and does not run anything in the container.
func (d *Driver) stats(ctx context.Context, id string) (cpuSample, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := d.docker.ContainerStats(ctx, id, client.ContainerStatsOptions{})
	if err != nil {
		return cpuSample{}, 0, err
	}
	defer res.Body.Close()
	var st container.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		return cpuSample{}, 0, fmt.Errorf("read stats: %w", err)
	}
	return cpuSample{at: st.Read, usage: st.CPUStats.CPUUsage.TotalUsage}, memoryMB(st.MemoryStats), nil
}

// cpuPercent is the CPU use between two samples as a part of the vCPU
// allocation, 0 to 100.
func cpuPercent(prev, cur cpuSample, vcpu int) float64 {
	wall := cur.at.Sub(prev.at)
	if wall <= 0 || vcpu <= 0 || cur.usage < prev.usage {
		return 0
	}
	p := float64(cur.usage-prev.usage) / float64(wall.Nanoseconds()) / float64(vcpu) * 100
	return min(p, 100)
}

// memoryMB is the memory use without the inactive page cache, as docker
// stats shows it.
func memoryMB(m container.MemoryStats) int {
	used := m.Usage
	cache, ok := m.Stats["inactive_file"] // cgroup v2
	if !ok {
		cache = m.Stats["total_inactive_file"] // cgroup v1
	}
	if cache < used {
		used -= cache
	}
	return int(used >> 20)
}
