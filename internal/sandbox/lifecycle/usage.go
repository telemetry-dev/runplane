package lifecycle

import (
	"context"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

// Usage is one reading of the cgroup of a sandbox.
type Usage struct {
	At     time.Time
	CPU    time.Duration // CPU time since the start
	Memory int64         // bytes, with the page cache
	// Cache is the inactive file cache in Memory. The metrics leave it out,
	// as docker stats does.
	Cache int64
}

func (c *Core[T]) sampleLoop(ctx context.Context) {
	t := time.NewTicker(metricsEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sample(ctx)
		}
	}
}

func (c *Core[T]) sample(ctx context.Context) {
	for _, b := range c.Boxes() {
		if b.State() != sandbox.StateBusy {
			continue
		}
		u, err := c.backend.Usage(ctx, b)
		if err != nil {
			if ctx.Err() == nil {
				b.log.Debug("read sandbox usage", "err", err)
			}
			continue
		}
		b.mu.Lock()
		if m, ok := b.metrics(u); ok {
			b.emit(sandbox.Event{Type: sandbox.EventMetrics, Metrics: &m})
		}
		b.mu.Unlock()
	}
}

// metrics turns a reading into metrics. The CPU percent needs the previous
// reading, so the first reading gives none. The caller holds b.mu.
func (b *Box[T]) metrics(u Usage) (sandbox.Metrics, bool) {
	prev := b.cpu
	b.cpu = u
	wall := u.At.Sub(prev.At)
	if prev.At.IsZero() || wall <= 0 || u.CPU < prev.CPU {
		return sandbox.Metrics{}, false
	}
	pct := float64(u.CPU-prev.CPU) * 100 / (float64(wall) * float64(max(b.spec.VCPU, 1)))
	mem := u.Memory
	if u.Cache < mem {
		mem -= u.Cache
	}
	return sandbox.Metrics{CPUPercent: min(pct, 100), MemoryMB: int(mem >> 20)}, true
}
