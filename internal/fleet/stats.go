package fleet

import (
	"context"
	"time"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/store"
)

// maxSamples limits the samples that wait for a flush, for the time that
// the database fails.
const maxSamples = 10_000

// addStats publishes a host sample of a server and keeps it for the next
// flush. It also sends a notification when the free disk space falls below
// min_free_disk_percent.
func (f *Fleet) addStats(n *node, s host.Stats) {
	if s.At.IsZero() {
		s.At = f.now()
	}
	st := store.ServerStat{
		ServerID: n.id, At: s.At.UTC(), CPUPercent: s.CPUPercent, MemoryPercent: s.MemoryPercent, DiskPercent: s.DiskPercent,
	}
	f.bus.Publish(events.Event{Type: events.HostStats, Data: st})

	free := 100 - s.DiskPercent
	f.mu.Lock()
	if len(f.samples) >= maxSamples {
		f.samples = f.samples[1:]
	}
	f.samples = append(f.samples, st)
	minFree, name := f.minFree, n.name
	low := s.DiskBytes > 0 && free < minFree
	alert := low && !f.diskLow[n.id]
	f.diskLow[n.id] = low
	f.mu.Unlock()
	if alert {
		f.log.Warn("disk almost full", "server_id", n.id, "free_percent", free)
		f.notify(func(dashboard string) notify.Event {
			return notify.DiskLowEvent(dashboard, n.id, name, free, int(minFree))
		})
	}
}

// flushStats writes the kept samples, and the time of the last sample of
// each agent as its last contact.
func (f *Fleet) flushStats(ctx context.Context) {
	f.mu.Lock()
	batch := make([]store.ServerStat, 0, len(f.samples))
	seen := map[string]time.Time{}
	for _, st := range f.samples {
		// The row of a removed server is gone, and its samples would fail the batch.
		if f.removed[st.ServerID] {
			continue
		}
		batch = append(batch, st)
		if st.ServerID != f.localID && st.At.After(seen[st.ServerID]) {
			seen[st.ServerID] = st.At
		}
	}
	f.samples = nil
	f.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	if err := f.st.AppendServerStats(ctx, batch); err != nil {
		f.log.Warn("record host stats", "samples", len(batch), "err", err)
	}
	for id, at := range seen {
		if err := f.st.TouchServer(ctx, id, at); err != nil {
			f.log.Debug("record the last contact", "server_id", id, "err", err)
		}
	}
}

// AgentMirrorStats sums the registry mirror stats of the connected agents.
// An agent that does not answer adds nothing.
func (f *Fleet) AgentMirrorStats(ctx context.Context) mirror.Stats {
	f.mu.Lock()
	var sessions []*agent.Session
	for _, n := range f.nodes {
		if n.sess != nil {
			sessions = append(sessions, n.sess)
		}
	}
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var sum mirror.Stats
	for _, s := range sessions {
		m, err := s.MirrorStats(ctx)
		if err != nil {
			f.log.Warn("read mirror stats", "server_id", s.ServerID(), "err", err)
			continue
		}
		sum.Add(m)
	}
	return sum
}
