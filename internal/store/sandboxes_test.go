package store

import (
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

func TestAdvanceSandboxStateTimes(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	must(t, s.InsertSandbox(ctx, Sandbox{ID: "b1", PoolID: "p", ServerID: "v", Mode: sandbox.ModeContainer,
		State: sandbox.StateStarting, VCPU: 2, MemoryMB: 4096, CreatedAt: sec(0)}))
	var b Sandbox
	for _, step := range []struct {
		state  sandbox.State
		reason string
		sec    int
	}{
		{sandbox.StateIdle, "", 1},
		{sandbox.StateIdle, "", 5}, // a repeat keeps the first time
		{sandbox.StateBusy, "", 9},
		{sandbox.StateStopping, "job done", 20},
		{sandbox.StateGone, "removed", 21},
		{sandbox.StateIdle, "", 30}, // a done sandbox keeps its state
	} {
		var err error
		b, _, err = s.AdvanceSandboxState(ctx, "b1", step.state, step.reason, sec(step.sec))
		must(t, err)
	}

	if b.State != sandbox.StateGone {
		t.Errorf("state = %s, want gone", b.State)
	}
	for _, c := range []struct {
		name string
		got  *time.Time
		sec  int
	}{{"idle_at", b.IdleAt, 1}, {"busy_at", b.BusyAt, 9}, {"stopped_at", b.StoppedAt, 21}} {
		if c.got == nil || !c.got.Equal(sec(c.sec)) {
			t.Errorf("%s = %v, want t0+%ds", c.name, c.got, c.sec)
		}
	}
	if b.ExitReason != "job done" || b.Error != "" {
		t.Errorf("exit reason %q, error %q", b.ExitReason, b.Error)
	}

	if _, _, err := s.AdvanceSandboxState(ctx, "missing", sandbox.StateIdle, "", sec(0)); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown sandbox: err = %v, want ErrNotFound", err)
	}
}

// The job and its sandbox name each other. A link to a job that does not
// exist must not give the sandbox the job.
func TestLinkJobSandbox(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	must(t, s.InsertSandbox(ctx, Sandbox{ID: "b1", PoolID: "p", ServerID: "v1", Mode: sandbox.ModeContainer,
		State: sandbox.StateBusy, CreatedAt: sec(0)}))
	j := addJob(t, s, JobMessage{RunnerRequestID: 1, State: JobRunning, QueuedAt: sec(0)})

	if _, _, err := s.LinkJobSandbox(ctx, "missing", "b1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("link to a missing job: err = %v, want ErrNotFound", err)
	}
	if b, _ := s.GetSandbox(ctx, "b1"); b.JobID != "" {
		t.Errorf("sandbox job %q after a failed link, want none", b.JobID)
	}
	job, b, err := s.LinkJobSandbox(ctx, j.ID, "b1")
	must(t, err)
	if job.SandboxID != "b1" || job.ServerID != "v1" || b.JobID != j.ID {
		t.Errorf("job sandbox %q server %q, sandbox job %q; want b1, v1, %s", job.SandboxID, job.ServerID, b.JobID, j.ID)
	}
}

// The runners page shows the runners of each pool and its jobs that wait
// for a runner: queued or assigned, and not rejected.
func TestPoolCounts(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	for i, b := range []struct {
		pool  string
		state sandbox.State
	}{
		{"p1", sandbox.StateIdle}, {"p1", sandbox.StateIdle}, {"p1", sandbox.StateBusy}, {"p1", sandbox.StateStarting},
		{"p1", sandbox.StateStopping}, {"p1", sandbox.StateGone}, {"p2", sandbox.StateBusy},
	} {
		must(t, s.InsertSandbox(ctx, Sandbox{ID: fmt.Sprintf("b%d", i), PoolID: b.pool, ServerID: "v1",
			Mode: sandbox.ModeContainer, State: b.state, CreatedAt: sec(0)}))
	}
	for i, j := range []struct {
		pool, state, reject string
	}{
		{"p1", JobQueued, ""},
		{"p1", JobQueued, "Public repository: the setting allow_public_repos is off."},
		{"p1", JobAssigned, ""},
		{"p1", JobRunning, ""},
		{"p2", JobQueued, ""},
	} {
		job := addJob(t, s, JobMessage{RunnerRequestID: int64(i + 1), State: j.state, PoolID: j.pool, QueuedAt: sec(i)})
		if j.reject != "" {
			must(t, s.SetJobRejectReason(ctx, job.ID, j.reject))
		}
	}

	got, err := s.PoolCounts(ctx)
	must(t, err)
	want := map[string]PoolCounts{
		"p1": {Idle: 2, Busy: 1, Starting: 1, Queued: 2, Assigned: 1},
		"p2": {Busy: 1, Queued: 1},
	}
	if !maps.Equal(got, want) {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
}
