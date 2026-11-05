package sysbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox/lifecycle"
)

// Usage reads one sample of Docker stats. Docker reads cgroup files on the
// host and runs nothing in the container.
func (d backend) Usage(ctx context.Context, b *box) (lifecycle.Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := d.docker.ContainerStats(ctx, containerName(b.ID), client.ContainerStatsOptions{})
	if err != nil {
		return lifecycle.Usage{}, err
	}
	defer res.Body.Close()
	var st container.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		return lifecycle.Usage{}, fmt.Errorf("read stats: %w", err)
	}
	return usage(st), nil
}

func usage(st container.StatsResponse) lifecycle.Usage {
	cache, ok := st.MemoryStats.Stats["inactive_file"] // cgroup v2
	if !ok {
		cache = st.MemoryStats.Stats["total_inactive_file"] // cgroup v1
	}
	return lifecycle.Usage{
		At:     st.Read,
		CPU:    time.Duration(st.CPUStats.CPUUsage.TotalUsage),
		Memory: int64(st.MemoryStats.Usage),
		Cache:  int64(cache),
	}
}
