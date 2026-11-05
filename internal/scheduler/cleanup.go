package scheduler

import (
	"context"
	"errors"
	"maps"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// cleanup compares the full state, for the messages and events that
// runplane did not get.
func (s *Scheduler) cleanup(ctx context.Context) {
	s.syncServers(ctx)
	s.removeStrayRunners(ctx)
	s.stopOverdue(ctx)
	s.cancelLostJobs(ctx)
	s.closeUnclaimedJobs(ctx)
	s.resumeFinish(ctx)
	s.dropExpired()
}

// syncServers stops each sandbox that has no row, or a gone or failed row,
// and sets each live row with no sandbox on a connected server to gone. It
// also drops the waiting sandboxes of a server with no connection for
// lostServerAfter.
func (s *Scheduler) syncServers(ctx context.Context) {
	servers, err := s.st.ListServers(ctx)
	if err != nil {
		s.log.Warn("cleanup: list servers", "err", err)
		return
	}
	// A start that is in flight now, or that begins after now, can have a
	// row before its driver lists it. Take this snapshot before the lists.
	s.mu.Lock()
	starting := maps.Clone(s.starting)
	s.mu.Unlock()
	since := s.now().Truncate(time.Millisecond)

	known := map[string]bool{}
	for _, v := range servers {
		known[v.ID] = true
		d := s.fleet.Driver(v.ID)
		if d == nil {
			if v.LastSeenAt != nil && s.now().Sub(*v.LastSeenAt) > lostServerAfter {
				s.dropWaiting(ctx, v.ID)
			}
			continue
		}
		list, err := d.List(ctx)
		if err != nil {
			s.log.Warn("cleanup: list sandboxes", "server", v.ID, "err", err)
			continue
		}
		listed := map[string]bool{}
		for _, x := range list {
			listed[x.ID] = true
			b, err := s.st.GetSandbox(ctx, x.ID)
			if err == nil && !b.State.Done() {
				continue
			}
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				s.log.Warn("cleanup: read sandbox", "sandbox", x.ID, "err", err)
				continue
			}
			s.log.Info("cleanup: stop a sandbox with no live row", "sandbox", x.ID, "server", v.ID)
			if err := d.Stop(ctx, x.ID); err != nil {
				s.log.Warn("cleanup: stop sandbox", "sandbox", x.ID, "err", err)
			}
		}
		rows, err := s.st.ListSandboxes(ctx, store.SandboxFilter{ServerID: v.ID, States: sandbox.LiveStates()})
		if err != nil {
			s.log.Warn("cleanup: list rows", "server", v.ID, "err", err)
			continue
		}
		for _, b := range rows {
			if listed[b.ID] || starting[b.ID] != "" || !b.CreatedAt.Before(since) {
				continue
			}
			s.setState(ctx, b.ID, sandbox.StateGone, "not found on the server")
			s.flushSandbox(ctx, b.ID)
		}
	}

	rows, err := s.st.ListLiveSandboxes(ctx)
	if err != nil {
		s.log.Warn("cleanup: list rows", "err", err)
		return
	}
	for _, b := range rows {
		if !known[b.ServerID] {
			s.setState(ctx, b.ID, sandbox.StateGone, "server removed")
			s.flushSandbox(ctx, b.ID)
		}
	}
}

// dropWaiting sets the starting and idle rows of a lost server to gone, so
// that they stop counting as runners and the pools start sandboxes on other
// servers. It removes each runner from GitHub first and keeps a row whose
// runner GitHub calls busy. A busy row ends with its job. If the server
// comes back, the cleanup stops the sandboxes that have a gone row.
func (s *Scheduler) dropWaiting(ctx context.Context, serverID string) {
	rows, err := s.st.ListSandboxes(ctx, store.SandboxFilter{ServerID: serverID,
		States: []sandbox.State{sandbox.StateStarting, sandbox.StateIdle}})
	if err != nil {
		s.log.Warn("cleanup: list rows", "server", serverID, "err", err)
		return
	}
	for _, b := range rows {
		if err := s.removeRunner(ctx, b); err != nil {
			if !errors.Is(err, ErrRunnerBusy) {
				s.log.Warn("cleanup: remove runner", "sandbox", b.ID, "runner", b.RunnerName, "err", err)
			}
			continue
		}
		s.log.Info("cleanup: drop a sandbox of an unreachable server", "sandbox", b.ID, "server", serverID)
		s.setState(ctx, b.ID, sandbox.StateGone, "server unreachable")
		s.flushSandbox(ctx, b.ID)
	}
}

// removeStrayRunners removes the runners of each attached scale set that
// have no live sandbox.
func (s *Scheduler) removeStrayRunners(ctx context.Context) {
	for _, h := range s.attached() {
		runners, err := h.client.ListRunners(ctx)
		if err != nil {
			s.log.Warn("cleanup: list runners", "scale_set", h.set.ID, "err", err)
			continue
		}
		// A runner can exist on GitHub before its row. Take this snapshot
		// after the runner list and before the rows.
		s.mu.Lock()
		starting := map[string]bool{}
		for _, name := range s.starting {
			starting[name] = true
		}
		s.mu.Unlock()
		rows, err := s.st.ListSandboxes(ctx, store.SandboxFilter{PoolID: h.set.PoolID, States: sandbox.LiveStates()})
		if err != nil {
			s.log.Warn("cleanup: list rows", "err", err)
			continue
		}
		known := map[string]bool{}
		for _, b := range rows {
			known[b.RunnerName] = true
		}
		for _, r := range runners {
			if r.Busy || known[r.Name] || starting[r.Name] {
				continue
			}
			s.log.Info("cleanup: remove a runner with no sandbox", "runner", r.Name, "scale_set", h.set.ID)
			if err := h.client.RemoveRunner(ctx, r.ID); err != nil && !errors.Is(err, ErrRunnerBusy) {
				s.log.Warn("cleanup: remove runner", "runner", r.Name, "err", err)
			}
		}
	}
}

// stopOverdue stops the sandboxes past their time limit or debug hold, and
// the waiting sandboxes of deleted pools and scale sets.
func (s *Scheduler) stopOverdue(ctx context.Context) {
	rows, err := s.st.ListLiveSandboxes(ctx)
	if err != nil {
		s.log.Warn("cleanup: list rows", "err", err)
		return
	}
	st, err := s.st.GetSettings(ctx)
	if err != nil {
		s.log.Warn("cleanup: read settings", "err", err)
		return
	}
	pools, err := s.st.ListPools(ctx)
	if err != nil {
		s.log.Warn("cleanup: list pools", "err", err)
		return
	}
	sets, err := s.st.ListScaleSets(ctx)
	if err != nil {
		s.log.Warn("cleanup: list scale sets", "err", err)
		return
	}
	poolByID := map[string]store.Pool{}
	for _, p := range pools {
		poolByID[p.ID] = p
	}
	hasSet := map[string]bool{}
	for _, x := range sets {
		hasSet[x.ID] = true
	}
	now := s.now()
	for _, b := range rows {
		pool, hasPool := poolByID[b.PoolID]
		// The time limit counts from the job start. After the job, the hold
		// takes its place.
		var busy time.Time
		if b.BusyAt != nil {
			busy = *b.BusyAt
		}
		end := sandbox.Deadline(b.CreatedAt, busy, timeLimit(pool, st))
		if b.HoldUntil != nil {
			end = *b.HoldUntil
		}
		reason := ""
		switch {
		case now.After(end):
			reason = "time limit"
		case (!hasPool || !hasSet[b.ScaleSetID]) && (b.State == sandbox.StateIdle || b.State == sandbox.StateStarting):
			reason = "pool removed"
		default:
			continue
		}
		if err := s.stop(ctx, b, reason); err != nil {
			s.log.Warn("cleanup: stop sandbox", "sandbox", b.ID, "reason", reason, "err", err)
		}
	}
}

// cancelLostJobs completes the running jobs whose sandbox is gone for more
// than lostJobAfter with no completion message. The result is a guess: the
// one that the runner reported, or else cancelled. A late JobCompleted, or
// the conclusion of the GitHub job that the finish step reads, replaces it.
func (s *Scheduler) cancelLostJobs(ctx context.Context) {
	jobs, err := s.st.ListStaleRunningJobs(ctx, s.now().Add(-lostJobAfter))
	if err != nil {
		s.log.Warn("cleanup: list lost jobs", "err", err)
		return
	}
	for _, j := range jobs {
		finished := s.now()
		if b, err := s.st.GetSandbox(ctx, j.SandboxID); err == nil && b.StoppedAt != nil {
			finished = *b.StoppedAt
		}
		result := store.ResultCancelled
		s.mu.Lock()
		if r, ok := s.results[j.ID]; ok {
			result = r.result
			delete(s.results, j.ID)
		}
		s.mu.Unlock()
		s.log.Info("cleanup: complete a job whose sandbox is gone", "job", j.ID, "sandbox", j.SandboxID, "result", result)
		done, _, err := s.st.UpsertJob(ctx, store.JobMessage{
			RunnerRequestID: j.RunnerRequestID, State: store.JobCompleted, Result: result, Guessed: true, FinishedAt: finished,
		}, s.now())
		if err != nil {
			s.log.Warn("cleanup: complete job", "job", j.ID, "err", err)
			continue
		}
		s.bus.Publish(events.JobState(done))
		s.finish(j.ID)
	}
}

// resumeFinish starts the finish step again for the completed jobs whose
// step did not end, for example because runplane stopped during it. GitHub
// does not send the JobCompleted message again.
func (s *Scheduler) resumeFinish(ctx context.Context) {
	jobs, err := s.st.ListUnfinishedJobs(ctx, s.now().Add(-resumeFinishWithin), resumeFinishBatch)
	if err != nil {
		s.log.Warn("cleanup: list unfinished jobs", "err", err)
		return
	}
	for _, j := range jobs {
		s.finish(j.ID)
	}
}

// closeUnclaimedJobs closes the jobs that wait in queued for longer than
// unclaimedJobAfter: runplane had no room for them, or GitHub gave them to
// another runner.
func (s *Scheduler) closeUnclaimedJobs(ctx context.Context) {
	now := s.now()
	jobs, err := s.st.ExpireQueuedJobs(ctx, now.Add(-unclaimedJobAfter), now)
	if err != nil {
		s.log.Warn("cleanup: close unclaimed jobs", "err", err)
		return
	}
	pools := map[string]bool{}
	for _, j := range jobs {
		s.log.Info("cleanup: close a job that runplane did not run", "job", j.ID, "queued_at", j.QueuedAt)
		s.bus.Publish(events.JobState(j))
		pools[j.PoolID] = true
	}
	for id := range pools {
		s.publishPoolStats(ctx, id)
	}
}

func (s *Scheduler) dropExpired() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	maps.DeleteFunc(s.checks, func(_ checkKey, e checkEntry) bool { return now.Sub(e.at) >= checkTTL })
	maps.DeleteFunc(s.reruns, func(_ rerunKey, at time.Time) bool { return now.Sub(at) >= rerunTTL })
	maps.DeleteFunc(s.results, func(_ string, r runnerResult) bool { return now.Sub(r.at) >= resultTTL })
}

// retention deletes the jobs and sandbox rows older than job_retention_days,
// and the stored logs older than log_retention_days.
func (s *Scheduler) retention(ctx context.Context) {
	st, err := s.st.GetSettings(ctx)
	if err != nil {
		s.log.Warn("retention: read settings", "err", err)
		return
	}
	now := s.now()
	before := now.AddDate(0, 0, -st.JobRetentionDays)
	ids, err := s.st.DeleteJobsBefore(ctx, before)
	if err != nil {
		s.log.Warn("retention: delete jobs", "err", err)
	}
	s.jobLogs.Remove(ids)
	if err := s.st.DeleteSandboxesBefore(ctx, before); err != nil {
		s.log.Warn("retention: delete sandboxes", "err", err)
	}
	if err := s.jobLogs.Expire(ctx, now.AddDate(0, 0, -st.LogRetentionDays)); err != nil {
		s.log.Warn("retention: expire logs", "err", err)
	}
}
