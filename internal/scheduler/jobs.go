package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/store"
)

var (
	// GitHub can need some time after a job before its log is ready.
	logWaits = []time.Duration{0, 10 * time.Second, 30 * time.Second, 2 * time.Minute}
	// stepsEvery is how often the scheduler reads the steps of a running job.
	// Each read is one GitHub request.
	stepsEvery = 30 * time.Second
)

// finish starts the steps after a job completes, unless they already run
// for the job: the debug hold, the run data and the log from GitHub, and the
// metrics summary.
func (s *Scheduler) finish(jobID string) {
	s.mu.Lock()
	if stop := s.watching[jobID]; stop != nil {
		close(stop)
		delete(s.watching, jobID)
	}
	running := s.finishing[jobID]
	s.finishing[jobID] = true
	s.mu.Unlock()
	if running {
		return
	}
	s.goBg(func(ctx context.Context) {
		s.finishJob(ctx, jobID)
		s.mu.Lock()
		delete(s.finishing, jobID)
		s.mu.Unlock()
	})
}

// finishJob does the finish step. When ctx ends during the log download,
// the job keeps log_state live, so that the cleanup runs the step again
// after a restart; the metrics summary is saved anyway.
func (s *Scheduler) finishJob(ctx context.Context, jobID string) {
	job, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		s.log.Warn("finish job", "job", jobID, "err", err)
		return
	}
	if job.SandboxID != "" {
		if b, err := s.st.GetSandbox(ctx, job.SandboxID); err == nil {
			if job.Debug {
				s.hold(ctx, b)
			}
			s.flushSandbox(ctx, b.ID)
		}
	}
	if err := s.gh.Enrich(ctx, job); err != nil {
		s.log.Warn("enrich job", "job", job.ID, "err", err)
	}
	// A job that never ran has no log, for example when GitHub cancelled
	// an assignment that no runner took in time.
	if job.LogState != store.LogStored && (job.StartedAt != nil || job.SandboxID != "") {
		s.storeLog(ctx, job)
	}
	wctx := context.WithoutCancel(ctx)
	summary, err := s.st.JobMetricsSummary(wctx, job.ID)
	if err == nil {
		err = s.st.SetJobMetricsSummary(wctx, job.ID, summary)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("summarize job metrics", "job", job.ID, "err", err)
	}
	if job, err = s.st.GetJob(wctx, job.ID); err == nil {
		s.publish(events.JobState, job)
	}
}

// storeLog downloads the job log from GitHub to the logs folder. It writes
// no log state when ctx ends first.
func (s *Scheduler) storeLog(ctx context.Context, job store.Job) {
	path := filepath.Join(s.logsDir, job.ID+".log")
	var err error
	for _, wait := range logWaits {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err = s.gh.DownloadLog(ctx, job, path); err == nil {
			break
		}
	}
	state := store.LogStored
	if err != nil {
		s.log.Warn("download job log", "job", job.ID, "err", err)
		state = store.LogNone
	}
	if err := s.st.SetJobLogState(ctx, job.ID, state); err != nil {
		s.log.Warn("save job log state", "job", job.ID, "err", err)
	}
}

// enrich reads the run data of a started job from GitHub in the background.
func (s *Scheduler) enrich(job store.Job) {
	s.goBg(func(ctx context.Context) {
		if err := s.gh.Enrich(ctx, job); err != nil {
			s.log.Warn("enrich job", "job", job.ID, "err", err)
			return
		}
		if job, err := s.st.GetJob(ctx, job.ID); err == nil {
			s.publish(events.JobState, job)
		}
	})
}

// watchSteps reads the steps of a running job from GitHub each stepsEvery
// until finish starts for the job, so that the job page shows its progress.
func (s *Scheduler) watchSteps(jobID string) {
	stop := make(chan struct{})
	s.mu.Lock()
	if s.watching[jobID] != nil {
		s.mu.Unlock()
		return
	}
	s.watching[jobID] = stop
	s.mu.Unlock()
	s.goBg(func(ctx context.Context) {
		defer func() {
			s.mu.Lock()
			if s.watching[jobID] == stop {
				delete(s.watching, jobID)
			}
			s.mu.Unlock()
		}()
		tick := time.NewTicker(stepsEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-tick.C:
			}
			job, err := s.st.GetJob(ctx, jobID)
			if err != nil || job.State == store.JobCompleted {
				return
			}
			steps, err := s.gh.Steps(ctx, job)
			if err != nil {
				s.log.Warn("read job steps", "job", jobID, "err", err)
				continue
			}
			if steps == nil || sameSteps(steps, job.Steps) {
				continue
			}
			if err := s.st.SetJobSteps(ctx, jobID, steps); err != nil {
				s.log.Warn("save job steps", "job", jobID, "err", err)
				continue
			}
			job.Steps = steps
			s.publish(events.JobState, job)
		}
	})
}

func sameSteps(a, b []store.JobStep) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// takeDebugJob reports if the next sandbox of the scale set is for a debug
// job that still waits for a runner.
func (s *Scheduler) takeDebugJob(ctx context.Context, scaleSetID string) bool {
	for {
		s.mu.Lock()
		list := s.debugJobs[scaleSetID]
		if len(list) == 0 {
			delete(s.debugJobs, scaleSetID)
			s.mu.Unlock()
			return false
		}
		jobID := list[0]
		s.debugJobs[scaleSetID] = list[1:]
		s.mu.Unlock()
		job, err := s.st.GetJob(ctx, jobID)
		if err == nil && job.SandboxID == "" && job.State != store.JobCompleted {
			return true
		}
	}
}

// hold keeps a sandbox for debug_hold_minutes after its debug job. It does
// nothing if the sandbox already has a hold.
func (s *Scheduler) hold(ctx context.Context, b store.Sandbox) {
	if b.HoldUntil != nil || b.State.Done() {
		return
	}
	st, err := s.st.GetSettings(ctx)
	if err != nil {
		s.log.Warn("hold sandbox", "sandbox", b.ID, "err", err)
		return
	}
	until := s.now().UTC().Add(time.Duration(st.DebugHoldMinutes) * time.Minute)
	if !s.driverHold(ctx, b, until) {
		return
	}
	if err := s.st.SetSandboxHold(ctx, b.ID, until); err != nil {
		s.log.Warn("save sandbox hold", "sandbox", b.ID, "err", err)
		return
	}
	b.HoldUntil = &until
	s.publish(events.SandboxState, b)
}

// holdForDebug keeps a sandbox that got a debug job after it started without
// the debug flag. Such a sandbox stops when the runner exits, before the
// scheduler can set the real hold, so it gets a hold past the time limit of
// the job, which starts now.
func (s *Scheduler) holdForDebug(ctx context.Context, b store.Sandbox) {
	pool, err := s.pool(ctx, b.PoolID)
	if err != nil {
		s.log.Warn("hold sandbox", "sandbox", b.ID, "err", err)
		return
	}
	st, err := s.st.GetSettings(ctx)
	if err != nil {
		s.log.Warn("hold sandbox", "sandbox", b.ID, "err", err)
		return
	}
	var p store.Pool
	if pool != nil {
		p = *pool
	}
	until := s.now().UTC().Add(timeLimit(p, st) + time.Duration(st.DebugHoldMinutes)*time.Minute)
	if !s.driverHold(ctx, b, until) {
		return
	}
	if err := s.st.SetSandboxDebug(ctx, b.ID); err != nil {
		s.log.Warn("mark sandbox debug", "sandbox", b.ID, "err", err)
	}
}

func (s *Scheduler) driverHold(ctx context.Context, b store.Sandbox, until time.Time) bool {
	d := s.fleet.Driver(b.ServerID)
	if d == nil {
		s.log.Warn("hold sandbox: server is not connected", "sandbox", b.ID, "server", b.ServerID)
		return false
	}
	if err := d.Hold(ctx, b.ID, until); err != nil {
		s.log.Warn("hold sandbox", "sandbox", b.ID, "err", err)
		return false
	}
	return true
}
