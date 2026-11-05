package scheduler

import (
	"fmt"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

func TestRunnerAndStateEvents(t *testing.T) {
	e := newTestEnv(t)
	serverID, _ := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	otherID, _ := e.addServer("fsn1-b", capacity("x64", 16, 65536))
	pool := e.addPool("runplane")
	client, _, set := e.attach(pool)
	b := e.insertSandbox(pool, set, serverID, sandbox.StateStarting, start)

	steps := []struct {
		server string
		event  sandbox.Event
		want   sandbox.State
	}{
		{serverID, runnerEvent(sandbox.RunnerListening), sandbox.StateIdle},
		{otherID, runnerEvent(sandbox.RunnerJobStarted), sandbox.StateIdle}, // another server cannot change it
		{serverID, runnerEvent(sandbox.RunnerJobStarted), sandbox.StateBusy},
		{serverID, runnerEvent(sandbox.RunnerListening), sandbox.StateBusy}, // a late event does not go back
		{serverID, runnerEvent(sandbox.RunnerExited), sandbox.StateStopping},
		{serverID, stateEvent(sandbox.StateGone, "removed"), sandbox.StateGone},
	}
	for i, step := range steps {
		e.clock.add(time.Second)
		e.event(step.server, b.ID, step.event)
		if got := e.sandbox(b.ID).State; got != step.want {
			t.Fatalf("step %d: state %q, want %q", i, got, step.want)
		}
	}
	got := e.sandbox(b.ID)
	if got.StoppedAt == nil || !got.StoppedAt.Equal(start.Add(6*time.Second)) || got.ExitReason != "runner exited with code 0" {
		t.Errorf("stopped at %v, exit reason %q", got.StoppedAt, got.ExitReason)
	}

	// The runner error from runplane-init is the exit reason, not the exit
	// code, which is 0 after a bad JIT config.
	noJIT := e.insertSandbox(pool, set, serverID, sandbox.StateStarting, start)
	exited := runnerEvent(sandbox.RunnerExited)
	exited.Runner.Error = "the runner did not connect to GitHub"
	e.event(serverID, noJIT.ID, exited)
	if got := e.sandbox(noJIT.ID); got.State != sandbox.StateStopping || got.ExitReason != "the runner did not connect to GitHub" {
		t.Errorf("state %q, exit reason %q; want stopping because the runner did not connect", got.State, got.ExitReason)
	}

	// A failed sandbox also loses its runner on GitHub.
	failed := e.insertSandbox(pool, set, serverID, sandbox.StateIdle, start)
	client.runners[failed.RunnerID] = Runner{ID: failed.RunnerID, Name: failed.RunnerName}
	e.event(serverID, failed.ID, stateEvent(sandbox.StateFailed, "init: connect control channel: timeout"))
	if f := e.sandbox(failed.ID); f.State != sandbox.StateFailed || f.StoppedAt == nil || f.Error == "" {
		t.Errorf("failed row = state %q, stopped %v, error %q", f.State, f.StoppedAt, f.Error)
	}
	if _, ok := client.runners[failed.RunnerID]; ok {
		t.Error("the runner of the failed sandbox is still on GitHub")
	}
}

func TestDebugRerunHoldsSandbox(t *testing.T) {
	e := newTestEnv(t)
	serverID, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane")
	_, sc, _ := e.attach(pool)
	first, _, err := e.st.UpsertJob(e.ctx, store.JobMessage{
		RunnerRequestID: 300, State: store.JobCompleted, Result: store.ResultFailed, PoolID: pool.ID,
		Repository: "acme/api", JobName: "integration", RunID: 5150, QueuedAt: start,
	}, start)
	if err != nil {
		t.Fatal(err)
	}

	if err := e.s.RerunJob(e.ctx, first.ID, true); err != nil {
		t.Fatal(err)
	}
	if len(e.rec.matching("rerun "+first.ID)) != 1 {
		t.Fatal("GitHub got no re-run")
	}
	// The next attempt has a new request ID. Another job of the run stays normal.
	e.scale(sc, msg(0, available(301, "acme/api", 5150, "integration"), available(302, "acme/api", 5150, "lint")))
	if !e.job(301).Debug || e.job(302).Debug {
		t.Fatalf("debug flags: attempt %v, other job %v; want true, false", e.job(301).Debug, e.job(302).Debug)
	}
	e.scale(sc, msg(1))
	if len(driver.specs) != 1 || !driver.specs[0].Debug {
		t.Fatalf("specs %+v, want one debug sandbox", driver.specs)
	}
	b := e.sandbox(driver.specs[0].ID)
	if !b.Debug {
		t.Error("sandbox row is not debug")
	}

	e.clock.add(4 * time.Minute)
	e.scale(sc, msg(1, started(301, "acme/api", 5150, "integration", b)))
	e.clock.add(3 * time.Minute)
	e.event(serverID, b.ID, sandbox.Event{Type: sandbox.EventRunner, Runner: &sandbox.RunnerEvent{
		Kind: sandbox.RunnerJobCompleted, JobName: "integration", Result: "failed"}})
	want := e.clock.now().Add(30 * time.Minute)
	if got := driver.holds[b.ID]; !got.Equal(want) {
		t.Errorf("driver hold until %v, want %v", got, want)
	}
	if got := e.sandbox(b.ID).HoldUntil; got == nil || !got.Equal(want) {
		t.Errorf("row hold until %v, want %v", got, want)
	}

	e.clock.add(time.Second)
	e.scale(sc, msg(0, completed(301, "acme/api", 5150, "integration", b, "failed")))
	e.settle()
	if n := len(e.rec.matching("hold ")); n != 1 {
		t.Errorf("%d hold calls, want 1 (the hold stays)", n)
	}
}

// GitHub can give a debug job to a warm sandbox that started without the
// debug flag. That sandbox needs a hold before its runner exits.
func TestDebugJobOnWarmSandbox(t *testing.T) {
	e := newTestEnv(t)
	serverID, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.TimeLimitMinutes = 60; p.MinIdle = 1 })
	_, sc, set := e.attach(pool)
	warm := e.insertSandbox(pool, set, serverID, sandbox.StateIdle, start)
	driver.running[warm.ID] = sandbox.Spec{ID: warm.ID, VCPU: 2, MemoryMB: 4096}
	e.s.reruns[rerunKey{"acme/api", 5150, "integration"}] = start

	e.scale(sc, msg(0, available(301, "acme/api", 5150, "integration")))
	e.scale(sc, msg(1, started(301, "acme/api", 5150, "integration", warm)))

	want := start.Add(60*time.Minute + 30*time.Minute)
	if got := driver.holds[warm.ID]; !got.Equal(want) {
		t.Errorf("hold until %v, want the time limit plus the debug hold (%v)", got, want)
	}
	if !e.sandbox(warm.ID).Debug {
		t.Error("the warm sandbox is not marked debug")
	}
}

func TestMetricsFlush(t *testing.T) {
	e := newTestEnv(t)
	serverID, _ := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane")
	_, sc, set := e.attach(pool)
	b := e.insertSandbox(pool, set, serverID, sandbox.StateBusy, start)
	metrics := func(cpu float64, mem int) sandbox.Event {
		e.clock.add(5 * time.Second)
		return sandbox.Event{Type: sandbox.EventMetrics, Metrics: &sandbox.Metrics{CPUPercent: cpu, MemoryMB: mem}}
	}

	// Samples before the job is linked wait for it.
	e.event(serverID, b.ID, metrics(12.5, 900))
	e.s.flushMetrics(e.ctx)
	e.scale(sc, msg(1, started(401, "acme/api", 6000, "build", b)))
	job := e.job(401)
	e.event(serverID, b.ID, metrics(87.5, 3100))
	e.event(serverID, b.ID, metrics(50, 2048))
	if list, _ := e.st.ListJobMetrics(e.ctx, job.ID); len(list) != 0 {
		t.Fatalf("%d samples before the flush, want 0", len(list))
	}

	e.s.flushMetrics(e.ctx)
	list, err := e.st.ListJobMetrics(e.ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].CPUPercent != 12.5 || list[2].MemoryMB != 2048 {
		t.Fatalf("samples %+v, want the 3 samples in order", list)
	}

	e.scale(sc, msg(0, completed(401, "acme/api", 6000, "build", b, "succeeded")))
	e.settle()
	job = e.job(401)
	if job.CPUPeak == nil || *job.CPUPeak != 87.5 || *job.CPUAvg != 50 || *job.MemPeakMB != 3100 {
		t.Errorf("summary peak %v avg %v mem %v, want 87.5, 50, 3100", job.CPUPeak, job.CPUAvg, job.MemPeakMB)
	}
}

func TestStopSandboxStopsBusySandbox(t *testing.T) {
	e := newTestEnv(t)
	serverID, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane")
	client, _, set := e.attach(pool)
	b := e.insertSandbox(pool, set, serverID, sandbox.StateBusy, start)
	client.busy[b.RunnerID] = true
	driver.running[b.ID] = sandbox.Spec{ID: b.ID}

	if err := e.s.StopSandbox(e.ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if e.rec.index(fmt.Sprintf("remove-runner %d", b.RunnerID)) < 0 || e.rec.index("stop "+b.ID) < 0 {
		t.Errorf("calls %v: want a runner removal and a stop", e.rec.calls)
	}
	if got := e.sandbox(b.ID); got.State != sandbox.StateStopping || got.ExitReason == "" {
		t.Errorf("state %q, exit reason %q", got.State, got.ExitReason)
	}
}
