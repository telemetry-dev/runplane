package host

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

// Stats is one sample of host use. The percentages are 0 to 100.
type Stats struct {
	At            time.Time `json:"at"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryPercent float64   `json:"memory_percent"`
	DiskPercent   float64   `json:"disk_percent"`
	Resources
}

// Sampler measures the CPU, memory, and data disk use of the host.
type Sampler struct {
	DataDir  string
	Interval time.Duration // 0 means 10 seconds
}

// Run calls fn with one sample each interval until ctx ends. The first
// sample comes after one interval, because CPU use is the difference of two
// readings. Run returns nil when ctx ends and an error if a reading fails.
func (s Sampler) Run(ctx context.Context, fn func(Stats)) error {
	interval := s.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	prev, err := readCPU()
	if err != nil {
		return err
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		cur, err := readCPU()
		if err != nil {
			return err
		}
		res, mem, err := readResources(s.DataDir)
		if err != nil {
			return err
		}
		fn(Stats{
			At:            time.Now().UTC(),
			CPUPercent:    cpuPercent(prev, cur),
			MemoryPercent: percent(mem.totalKB-mem.availableKB, mem.totalKB),
			DiskPercent:   percent(uint64(res.DiskBytes-res.FreeDiskBytes), uint64(res.DiskBytes)),
			Resources:     res,
		})
		prev = cur
	}
}

// cpuTimes are the summed jiffies of all CPUs from the "cpu" line of /proc/stat.
type cpuTimes struct {
	busy, total uint64
}

// parseStat reads the first line of /proc/stat:
// cpu user nice system idle iowait irq softirq steal guest guest_nice.
// Idle and iowait count as idle. Guest time is already part of user time.
func parseStat(r io.Reader) (cpuTimes, error) {
	s := bufio.NewScanner(r)
	if !s.Scan() {
		return cpuTimes{}, errors.New("read cpu times: empty /proc/stat")
	}
	f := strings.Fields(s.Text())
	if len(f) < 5 || f[0] != "cpu" {
		return cpuTimes{}, errors.New("read cpu times: no cpu line in /proc/stat")
	}
	var t cpuTimes
	for i, field := range f[1:min(len(f), 9)] {
		v, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuTimes{}, errors.New("read cpu times: bad number in /proc/stat")
		}
		t.total += v
		if i != 3 && i != 4 { // idle, iowait
			t.busy += v
		}
	}
	return t, nil
}

// cpuPercent is the busy part of the CPU time between two readings.
func cpuPercent(prev, cur cpuTimes) float64 {
	if cur.total <= prev.total || cur.busy < prev.busy {
		return 0
	}
	return percent(cur.busy-prev.busy, cur.total-prev.total)
}

func percent(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}
