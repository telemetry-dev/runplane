package scheduler

import (
	"context"
	"errors"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// A sandbox keeps at most this many samples between flushes, for example
// while its job is not linked yet.
const maxBufferedSamples = 720

type metricsBuffer struct {
	serverID string
	samples  []store.JobMetric
}

// HandleEvent applies one event of a driver on the server. The fleet calls it
// for each event. Events for a sandbox of a different server are dropped, so
// an agent can change only its own sandboxes.
func (s *Scheduler) HandleEvent(serverID string, ev sandbox.Event) {
	if ev.Time.IsZero() {
		ev.Time = s.now()
	}
	switch {
	case ev.Type == sandbox.EventMetrics && ev.Metrics != nil:
		s.addSample(serverID, ev)
	case ev.Type == sandbox.EventState:
		s.onState(s.bg, serverID, ev)
	case ev.Type == sandbox.EventRunner && ev.Runner != nil:
		s.onRunner(s.bg, serverID, ev)
	}
}

func (s *Scheduler) onState(ctx context.Context, serverID string, ev sandbox.Event) {
	b, ok := s.sandboxOn(ctx, serverID, ev.SandboxID)
	if !ok {
		return
	}
	b, changed := s.setStateAt(ctx, b.ID, ev.State, ev.Reason, ev.Time)
	if !changed || !ev.State.Done() {
		return
	}
	s.flushSandbox(ctx, b.ID)
	if ev.State == sandbox.StateFailed {
		if err := s.removeRunner(ctx, b); err != nil {
			s.log.Warn("remove runner", "sandbox", b.ID, "runner", b.RunnerName, "err", err)
		}
	}
}

func (s *Scheduler) onRunner(ctx context.Context, serverID string, ev sandbox.Event) {
	b, ok := s.sandboxOn(ctx, serverID, ev.SandboxID)
	if !ok {
		return
	}
	if st, reason, ok := ev.Runner.State(); ok {
		s.setStateAt(ctx, b.ID, st, reason, ev.Time)
	}
	// The hold must come before init stops the sandbox, so do not wait for
	// the JobCompleted message.
	if ev.Runner.Kind != sandbox.RunnerJobCompleted || b.JobID == "" {
		return
	}
	job, err := s.st.GetJob(ctx, b.JobID)
	if err != nil {
		s.log.Warn("read job", "job", b.JobID, "err", err)
		return
	}
	if job.State != store.JobCompleted && ev.Runner.Result != "" {
		s.mu.Lock()
		s.results[job.ID] = runnerResult{jobResult(ev.Runner.Result), s.now()}
		s.mu.Unlock()
	}
	if job.Debug {
		s.hold(ctx, b)
	}
}

// sandboxOn returns the row of the sandbox if the sandbox is on the server.
func (s *Scheduler) sandboxOn(ctx context.Context, serverID, id string) (store.Sandbox, bool) {
	b, err := s.st.GetSandbox(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Sandbox{}, false
	}
	if err != nil {
		s.log.Warn("read sandbox", "sandbox", id, "err", err)
		return store.Sandbox{}, false
	}
	if b.ServerID != serverID {
		s.log.Warn("event for a sandbox of another server", "sandbox", id, "server", serverID)
		return store.Sandbox{}, false
	}
	return b, true
}

func (s *Scheduler) addSample(serverID string, ev sandbox.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := s.metrics[ev.SandboxID]
	if buf == nil {
		buf = &metricsBuffer{serverID: serverID}
		s.metrics[ev.SandboxID] = buf
	}
	if len(buf.samples) >= maxBufferedSamples {
		buf.samples = buf.samples[1:]
	}
	buf.samples = append(buf.samples, store.JobMetric{
		At: ev.Time, CPUPercent: ev.Metrics.CPUPercent, MemoryMB: ev.Metrics.MemoryMB,
	})
}

// flushMetrics writes the buffered samples of each sandbox to its job.
func (s *Scheduler) flushMetrics(ctx context.Context) {
	s.mu.Lock()
	all := s.metrics
	s.metrics = map[string]*metricsBuffer{}
	s.mu.Unlock()
	for id, buf := range all {
		if s.writeSamples(ctx, id, buf) {
			continue
		}
		// Keep the samples of a live sandbox until its job is linked.
		s.mu.Lock()
		if next := s.metrics[id]; next != nil {
			buf.samples = append(buf.samples, next.samples...)
		}
		buf.samples = buf.samples[max(0, len(buf.samples)-maxBufferedSamples):]
		s.metrics[id] = buf
		s.mu.Unlock()
	}
}

// flushSandbox writes the buffered samples of one sandbox to its job.
func (s *Scheduler) flushSandbox(ctx context.Context, id string) {
	s.mu.Lock()
	buf := s.metrics[id]
	delete(s.metrics, id)
	s.mu.Unlock()
	if buf != nil {
		s.writeSamples(ctx, id, buf)
	}
}

// writeSamples writes the samples to the job of the sandbox. It returns
// false, with nothing written, if the samples must wait: the sandbox is live
// but has no job yet.
func (s *Scheduler) writeSamples(ctx context.Context, id string, buf *metricsBuffer) bool {
	b, err := s.st.GetSandbox(ctx, id)
	if err != nil || b.ServerID != buf.serverID {
		return true
	}
	if b.JobID == "" {
		return b.State.Done()
	}
	if err := s.st.AppendJobMetrics(ctx, b.JobID, buf.samples); err != nil {
		s.log.Warn("save job metrics", "job", b.JobID, "err", err)
	}
	return true
}
