package scheduler

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/policy"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

func (s *Scheduler) resizeAll(ctx context.Context) {
	for _, h := range s.attached() {
		h.mu.Lock()
		err := h.resize(ctx)
		h.mu.Unlock()
		if err != nil {
			s.log.Warn("resize scale set", "scale_set", h.set.ID, "err", err)
		}
	}
}

// resize starts or stops sandboxes until the scale set has the runners it
// needs: min(min_idle + assigned jobs, max_runners, capacity). It stops idle
// sandboxes only; busy ones end with their job. It first replaces the idle
// sandboxes that a new sandbox would not equal.
func (h *scaleSet) resize(ctx context.Context) error {
	s := h.s
	pool, err := s.pool(ctx, h.set.PoolID)
	if err != nil {
		return err
	}
	st, err := s.st.GetSettings(ctx)
	if err != nil {
		return err
	}
	live, err := h.live(ctx)
	if err != nil {
		return err
	}
	if pool != nil {
		live = h.stopStale(ctx, *pool, st, live)
	}
	need := 0
	if pool != nil && pool.Enabled {
		need = min(pool.MinIdle+h.stats.AssignedJobs, pool.MaxRunners)
	}
	switch {
	case need > len(live):
		h.start(ctx, *pool, st, need-len(live))
	case need < len(live):
		h.stopIdle(ctx, live, len(live)-need)
	}
	return nil
}

// live returns the sandboxes of the scale set that count as its runners.
func (h *scaleSet) live(ctx context.Context) ([]store.Sandbox, error) {
	list, err := h.s.st.ListSandboxes(ctx, store.SandboxFilter{PoolID: h.set.PoolID, States: runnerStates})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list, func(b store.Sandbox) bool { return b.ScaleSetID != h.set.ID }), nil
}

// start places n sandboxes, as many as fit, and starts them at the same time.
func (h *scaleSet) start(ctx context.Context, pool store.Pool, st store.Settings, n int) {
	var wg sync.WaitGroup
	for range n {
		p, ok := h.s.place(ctx, pool, st)
		if !ok {
			h.s.log.Debug("no server can take a sandbox", "pool", pool.Label)
			break
		}
		debug := h.s.takeDebugJob(ctx, h.set.ID)
		wg.Go(func() { h.startOne(ctx, pool, st, p, debug) })
	}
	wg.Wait()
}

func (h *scaleSet) startOne(ctx context.Context, pool store.Pool, st store.Settings, p placement, debug bool) {
	s := h.s
	defer s.reserve(p.host.ServerID, pool, -1)
	id := ids.New()
	name := pool.Label + "-" + id[len(id)-8:]
	s.mu.Lock()
	s.starting[id] = name
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.starting, id)
		s.mu.Unlock()
	}()

	jit, err := h.client.GenerateJITConfig(ctx, name)
	if err != nil {
		s.log.Warn("generate JIT config", "pool", pool.Label, "err", err)
		return
	}
	b := store.Sandbox{
		ID:            id,
		PoolID:        pool.ID,
		ServerID:      p.host.ServerID,
		ScaleSetID:    h.set.ID,
		Mode:          p.host.Driver.Mode(),
		State:         sandbox.StateStarting,
		RunnerID:      jit.RunnerID,
		RunnerName:    cmp.Or(jit.RunnerName, name),
		ImageDigest:   p.image.Digest,
		RunnerVersion: p.runnerVersion,
		VCPU:          pool.VCPU,
		MemoryMB:      pool.MemoryMB,
		DiskGB:        pool.DiskGB,
		Debug:         debug,
		CreatedAt:     s.now().UTC(),
	}
	spec := sandbox.Spec{
		ID:            id,
		PoolID:        pool.ID,
		Image:         p.image,
		RunnerVersion: p.runnerVersion,
		Arch:          pool.Arch,
		VCPU:          pool.VCPU,
		MemoryMB:      pool.MemoryMB,
		DiskGB:        pool.DiskGB,
		TimeLimit:     timeLimit(pool, st),
		JITConfig:     jit.Encoded,
		Debug:         debug,
	}
	if err := s.launch(ctx, p, b, spec); err == nil {
		return
	}
	if err := h.client.RemoveRunner(context.WithoutCancel(ctx), jit.RunnerID); err != nil && !errors.Is(err, ErrRunnerBusy) {
		s.log.Warn("remove runner", "runner", jit.RunnerID, "err", err)
	}
}

// launch inserts the row of a new sandbox and starts it on the placement.
// After an error, the row is failed, or gone when ctx ended first.
func (s *Scheduler) launch(ctx context.Context, p placement, b store.Sandbox, spec sandbox.Spec) error {
	if err := s.st.InsertSandbox(ctx, b); err != nil {
		s.log.Warn("insert sandbox", "err", err)
		return err
	}
	s.bus.Publish(events.SandboxState(b))
	s.publishPoolStats(ctx, b.PoolID)

	err := p.host.Driver.Start(ctx, spec)
	if err == nil {
		return nil
	}
	// Cleanup writes must happen also when ctx ends during the start.
	bctx := context.WithoutCancel(ctx)
	if ctx.Err() != nil {
		// runplane stops; the driver gave up the start, and nothing failed.
		s.setState(bctx, b.ID, sandbox.StateGone, "runplane stopped during the start")
	} else {
		s.log.Warn("start sandbox", "sandbox", b.ID, "server", p.host.ServerID, "err", err)
		s.setState(bctx, b.ID, sandbox.StateFailed, "start: "+err.Error())
	}
	return err
}

// stopIdle stops up to n idle sandboxes, the oldest first.
func (h *scaleSet) stopIdle(ctx context.Context, live []store.Sandbox, n int) {
	idle := slices.DeleteFunc(slices.Clone(live), func(b store.Sandbox) bool { return b.State != sandbox.StateIdle })
	slices.SortFunc(idle, func(a, b store.Sandbox) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for _, b := range idle[:min(n, len(idle))] {
		h.stopIfFree(ctx, b, "scale down")
	}
}

// stopStale stops the idle sandboxes for which staleReason has a reason. It
// returns the sandboxes that stay.
func (h *scaleSet) stopStale(ctx context.Context, pool store.Pool, st store.Settings, live []store.Sandbox) []store.Sandbox {
	arch := map[string]string{}
	for _, host := range h.s.fleet.Online() {
		arch[host.ServerID] = host.Arch
	}
	var kept []store.Sandbox
	for _, b := range live {
		reason := ""
		if b.State == sandbox.StateIdle {
			reason = h.s.staleReason(b, pool, st, arch[b.ServerID])
		}
		if reason != "" && h.stopIfFree(ctx, b, "replaced: "+reason) {
			h.s.log.Info("replace a warm sandbox", "sandbox", b.ID, "pool", pool.Label, "reason", reason)
			continue
		}
		kept = append(kept, b)
	}
	return kept
}

// stopIfFree removes the runner of the sandbox from GitHub before it stops
// the sandbox, so that GitHub cannot give a job to a runner that goes away.
// It returns false, and keeps the sandbox, if the removal fails or GitHub
// calls the runner busy.
func (h *scaleSet) stopIfFree(ctx context.Context, b store.Sandbox, reason string) bool {
	if err := h.client.RemoveRunner(ctx, b.RunnerID); err != nil {
		if !errors.Is(err, ErrRunnerBusy) {
			h.s.log.Warn("remove runner", "sandbox", b.ID, "runner", b.RunnerName, "err", err)
		}
		return false
	}
	if err := h.s.stopNow(ctx, b, reason); err != nil {
		h.s.log.Warn("stop sandbox", "sandbox", b.ID, "err", err)
	}
	return true
}

// staleReason tells why the idle sandbox b must give way to a new sandbox of
// the pool, or returns "". arch is the arch of its server, empty if the
// server is not online.
func (s *Scheduler) staleReason(b store.Sandbox, pool store.Pool, st store.Settings, arch string) string {
	if s.now().Sub(b.CreatedAt) >= timeLimit(pool, st) {
		return "it waited for a job for the whole time limit"
	}
	if !policy.RunnerAllowed(st, b.Mode) {
		return "public repositories need VM mode"
	}
	if b.VCPU != pool.VCPU || b.MemoryMB != pool.MemoryMB || b.DiskGB != pool.DiskGB {
		return "the pool size changed"
	}
	if arch != "" && arch != pool.Arch {
		return "the pool arch changed"
	}
	if img, ok := s.fleet.Image(b.ServerID, pool.ImageID); ok && img.Digest != b.ImageDigest {
		return "the image changed"
	}
	if v, ok := s.fleet.RunnerVersion(b.ServerID); ok && v != b.RunnerVersion {
		return "a new runner version is ready"
	}
	return ""
}

// updateMaxRunners gives each listener its maxCapacity: the runners of its
// scale set plus the new sandboxes of its pool size that fit now, at most
// max_runners. A disabled or deleted pool gets 0.
func (s *Scheduler) updateMaxRunners(ctx context.Context) {
	sets := s.attached()
	if len(sets) == 0 {
		return
	}
	st, err := s.st.GetSettings(ctx)
	if err != nil {
		s.log.Warn("update max runners", "err", err)
		return
	}
	pools, err := s.st.ListPools(ctx)
	if err != nil {
		s.log.Warn("update max runners", "err", err)
		return
	}
	live, err := s.st.ListSandboxes(ctx, store.SandboxFilter{States: runnerStates})
	if err != nil {
		s.log.Warn("update max runners", "err", err)
		return
	}
	runners := map[string]int{}
	for _, b := range live {
		runners[b.ScaleSetID]++
	}
	hosts := s.hosts(ctx)
	for _, h := range sets {
		n := 0
		if i := slices.IndexFunc(pools, func(p store.Pool) bool { return p.ID == h.set.PoolID }); i >= 0 && pools[i].Enabled {
			n = min(runners[h.set.ID]+s.fits(hosts, pools[i], st), pools[i].MaxRunners)
		}
		h.client.SetMaxRunners(n)
	}
}
