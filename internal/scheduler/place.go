package scheduler

import (
	"context"
	"maps"

	"github.com/runplane/runplane/internal/policy"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// reservation is the size of the sandboxes that a server gets but that its
// driver may not count yet: from the placement until Driver.Start returns.
type reservation struct {
	vcpu, memoryMB, count int
}

type placement struct {
	host          Host
	image         sandbox.ImageRef
	runnerVersion string
}

// hostCapacity is an online server and its capacity, with its reservations
// counted as allocated.
type hostCapacity struct {
	Host
	cap sandbox.Capacity
}

// place selects the server for a new sandbox of the pool and reserves its
// size there. Of the servers that can take the sandbox, the one with the
// most free memory wins. It returns false if no server can take it now.
//
// Placements run one at a time. A placement reads the reservations before
// the capacities: a start that ends in between is then counted twice, never
// zero times, because a driver counts a sandbox from the moment Start
// returns.
func (s *Scheduler) place(ctx context.Context, pool store.Pool, st store.Settings) (placement, bool) {
	s.placeMu.Lock()
	defer s.placeMu.Unlock()
	var best placement
	bestFree := -1
	r := st.Reserve()
	for _, h := range s.hosts(ctx) {
		if h.cap.Room(pool.VCPU, pool.MemoryMB, r) == 0 {
			continue
		}
		img, version, ok := s.offer(h, pool, st)
		if !ok {
			continue
		}
		if free := h.cap.FreeMemoryMB(r); free > bestFree {
			best, bestFree = placement{h.Host, img, version}, free
		}
	}
	if bestFree < 0 {
		return placement{}, false
	}
	s.reserve(best.host.ServerID, pool, 1)
	return best, true
}

// reserve adds n sandboxes of the pool size to the reservation of the
// server. A negative n ends reservations.
func (s *Scheduler) reserve(serverID string, pool store.Pool, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.reserved[serverID]
	r.vcpu += n * pool.VCPU
	r.memoryMB += n * pool.MemoryMB
	r.count += n
	if r.count <= 0 {
		delete(s.reserved, serverID)
		return
	}
	s.reserved[serverID] = r
}

func (s *Scheduler) hosts(ctx context.Context) []hostCapacity {
	s.mu.Lock()
	reserved := maps.Clone(s.reserved)
	s.mu.Unlock()
	online := s.fleet.Online()
	list := make([]hostCapacity, 0, len(online))
	for _, h := range online {
		c, err := h.Driver.Capacity(ctx)
		if err != nil {
			s.log.Warn("read server capacity", "server", h.ServerID, "err", err)
			continue
		}
		r := reserved[h.ServerID]
		c.AllocVCPU += r.vcpu
		c.AllocMemoryMB += r.memoryMB + r.count*c.OverheadMB
		c.Sandboxes += r.count
		list = append(list, hostCapacity{h, c})
	}
	return list
}

// offer returns the image and the runner version for a sandbox of the pool
// on the server, or false if the server cannot take sandboxes of the pool.
// It does not check the CPU and memory.
func (s *Scheduler) offer(h hostCapacity, pool store.Pool, st store.Settings) (sandbox.ImageRef, string, bool) {
	if !pool.CanUse(h.ServerID, h.Arch) || !policy.RunnerAllowed(st, h.Driver.Mode()) ||
		h.cap.FreeDiskPercent() <= st.MinFreeDiskPercent {
		return sandbox.ImageRef{}, "", false
	}
	img, ok := s.fleet.Image(h.ServerID, pool.ImageID)
	if !ok {
		return sandbox.ImageRef{}, "", false
	}
	version, ok := s.fleet.RunnerVersion(h.ServerID)
	return img, version, ok
}

// maxRunners is the number of runners that a scale set of the pool can have
// now: its runners plus the new sandboxes of the pool size that the servers
// can take, at most max_runners.
func (s *Scheduler) maxRunners(runners int, hosts []hostCapacity, pool store.Pool, st store.Settings) int {
	n := runners
	for _, h := range hosts {
		if _, _, ok := s.offer(h, pool, st); ok {
			n += h.cap.Room(pool.VCPU, pool.MemoryMB, st.Reserve())
		}
	}
	return min(n, pool.MaxRunners)
}
