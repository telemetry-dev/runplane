package fleet

import (
	"context"
	"time"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/store"
)

// maxSamples limits the samples that wait for a flush, for the time that
// the database fails.
const maxSamples = 10_000

// addStats publishes a host sample of a server and keeps it for the next
// flush.
func (f *Fleet) addStats(n *node, s host.Stats) {
	if s.At.IsZero() {
		s.At = f.now()
	}
	st := store.ServerStat{
		ServerID: n.id, At: s.At.UTC(), CPUPercent: s.CPUPercent, MemoryPercent: s.MemoryPercent, DiskPercent: s.DiskPercent,
	}
	f.mu.Lock()
	if len(f.samples) >= maxSamples {
		f.samples = f.samples[1:]
	}
	f.samples = append(f.samples, st)
	// The sample of an agent is its last contact.
	if !n.Local() && st.At.After(f.seen[n.id]) {
		f.seen[n.id] = st.At
	}
	f.mu.Unlock()
	f.bus.Publish(events.HostStats(st))
}

// flushStats writes the kept samples, and the last contact of each agent.
func (f *Fleet) flushStats(ctx context.Context) {
	f.mu.Lock()
	batch := make([]store.ServerStat, 0, len(f.samples))
	for _, st := range f.samples {
		// The row of a removed server is gone, and its samples would fail the batch.
		if !f.removed[st.ServerID] {
			batch = append(batch, st)
		}
	}
	seen := f.seen
	for id := range seen {
		if f.removed[id] {
			delete(seen, id)
		}
	}
	f.samples, f.seen = nil, map[string]time.Time{}
	f.mu.Unlock()
	if len(batch) > 0 {
		if err := f.st.AppendServerStats(ctx, batch); err != nil {
			f.log.Warn("record host stats", "samples", len(batch), "err", err)
		}
	}
	for id, at := range seen {
		if err := f.st.TouchServer(ctx, id, at); err != nil {
			f.log.Debug("record the last contact", "server_id", id, "err", err)
		}
	}
}

// MirrorStats sums the registry mirror stats of the connected servers:
// the api Mirrors service. A server that does not answer adds nothing.
func (f *Fleet) MirrorStats(ctx context.Context) agent.MirrorStats {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var sum agent.MirrorStats
	for _, n := range f.connected() {
		m, err := n.MirrorStats(ctx)
		if err != nil {
			f.log.Warn("read mirror stats", "server_id", n.id, "err", err)
			continue
		}
		sum.Add(m)
	}
	return sum
}
