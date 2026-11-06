package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

func TestAcquisitionsStopAtCapacity(t *testing.T) {
	tests := []struct {
		name       string
		maxRunners int
		assigned   int
		want       []int64
	}{
		{"two fit", 5, 0, []int64{14, 12}},
		{"one assigned job takes a slot", 5, 1, []int64{14}},
		{"no slot left", 5, 2, nil},
		{"max_runners below capacity", 1, 0, []int64{14}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.addServer("fsn1-a", capacity("x64", 4, 8192)) // two sandboxes of 2 vCPU and 4 GB
			pool := e.addPool("runplane", func(p *store.Pool) { p.MaxRunners = tt.maxRunners })
			client, sc, _ := e.attach(pool)

			m := msg(tt.assigned,
				available(11, "acme/api", 900, "test (1)"),
				available(12, "acme/api", 900, "test (2)"),
				available(13, "acme/api", 900, "test (3)"),
				available(14, "acme/api", 900, "lint"),
			)
			for i, offset := range []int{3, 1, 2, 0} {
				m.Available[i].QueuedAt = start.Add(time.Duration(offset) * time.Second)
			}
			e.scale(sc, m)

			var got []int64
			if len(client.acquired) > 0 {
				got = client.acquired[0]
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("acquired %v, want %v", got, tt.want)
			}
			for _, id := range []int64{11, 12, 13, 14} {
				j := e.job(id)
				wantState := store.JobQueued
				if slices.Contains(tt.want, id) {
					wantState = store.JobAssigned
				}
				if j.State != wantState || j.RejectReason != "" {
					t.Errorf("job %d: state %q, reject %q; want %q and no reject reason", id, j.State, j.RejectReason, wantState)
				}
			}
		})
	}
}

func TestRedeliveredMessagesChangeNothing(t *testing.T) {
	e := newTestEnv(t)
	_, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane")
	client, sc, _ := e.attach(pool)

	offer := msg(0, available(101, "acme/api", 900, "build"))
	assign := msg(1, assigned(101, "acme/api", 900, "build"))
	for _, m := range []Message{offer, assign, assign, offer} {
		e.scale(sc, m)
	}
	if n := len(driver.specs); n != 1 {
		t.Fatalf("%d sandbox starts, want 1", n)
	}
	if n := len(e.rec.matching("jit ")); n != 1 {
		t.Errorf("%d JIT configs, want 1", n)
	}
	if n := len(client.acquired); n != 1 {
		t.Errorf("%d AcquireJobs calls, want 1", n)
	}

	b := e.sandboxes(pool.ID)[0]
	done := msg(0, completed(101, "acme/api", 900, "build", b, "succeeded"))
	e.scale(sc, done)
	e.settle()
	e.scale(sc, done)
	e.settle()
	// One finish step: one Enrich and one download.
	if n := len(e.rec.matching("enrich ")); n != 1 {
		t.Errorf("%d Enrich calls, want 1", n)
	}
	if n := len(e.rec.matching("download ")); n != 1 {
		t.Errorf("%d log downloads, want 1", n)
	}
	jobs, _, err := e.st.ListJobs(e.ctx, store.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d job rows, want 1", len(jobs))
	}
	if j := jobs[0]; j.State != store.JobCompleted || j.Result != store.ResultSucceeded || j.SandboxID != b.ID || j.LogState != store.LogStored {
		t.Errorf("job = state %q, result %q, sandbox %q, log %q", j.State, j.Result, j.SandboxID, j.LogState)
	}
}

func TestMinIdleKeepsWarmSandboxes(t *testing.T) {
	e := newTestEnv(t)
	e.setSettings(func(s *store.Settings) { s.DefaultTimeLimitMinutes = 45 })
	serverID, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 2 })
	_, sc, set := e.attach(pool)

	e.scale(sc, msg(0))
	specs := driver.specs
	if len(specs) != 2 {
		t.Fatalf("%d starts, want 2", len(specs))
	}
	names := map[string]bool{}
	for _, spec := range specs {
		b := e.sandbox(spec.ID)
		// GitHub lists the runners of the scale set by the label in the
		// name, and JobStarted names the runner of the sandbox.
		if b.State != sandbox.StateStarting || b.ScaleSetID != set.ID || b.RunnerID == 0 ||
			!IsRunnerOf(b.RunnerName, pool.Label) || e.rec.index("jit "+b.RunnerName) < 0 {
			t.Errorf("row = %+v; calls %v", b, e.rec.calls)
		}
		names[b.RunnerName] = true
		// The pool has no time limit of its own.
		if spec.TimeLimit != 45*time.Minute || spec.JITConfig == "" || spec.PoolID != pool.ID ||
			spec.Image.Digest != b.ImageDigest || spec.RunnerVersion != "2.330.0" {
			t.Errorf("spec = %+v", spec)
		}
	}
	if len(names) != 2 {
		t.Errorf("runner names %v, want one for each sandbox", names)
	}

	for _, s := range specs {
		e.listening(serverID, s.ID)
	}
	if n := len(e.sandboxes(pool.ID, sandbox.StateIdle)); n != 2 {
		t.Fatalf("%d idle sandboxes, want 2", n)
	}

	// A warm sandbox that goes away gets a replacement at the next poll.
	e.event(serverID, specs[1].ID, stateEvent(sandbox.StateGone, "exited"))
	e.scale(sc, Message{})
	if n := len(driver.specs); n != 3 {
		t.Fatalf("%d starts after a warm sandbox went away, want 3", n)
	}

	// An assigned job adds one sandbox, so that two stay warm.
	e.scale(sc, msg(1))
	if n := len(e.sandboxes(pool.ID, runnerStates...)); n != 3 {
		t.Errorf("%d runners with one assigned job, want 3", n)
	}
}

// A change of the pools or the settings comes from the dashboard, not from
// a listener message. The scheduler resizes the scale sets at its event.
func TestDashboardChangesResizeTheScaleSets(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(e *testEnv) store.Pool    // a pool that gets no warm sandbox
		change func(e *testEnv, p store.Pool) // lets it get one, and publishes the change
	}{
		{"pools", func(e *testEnv) store.Pool {
			return e.addPool("runplane", func(p *store.Pool) { p.MinIdle, p.Enabled = 1, false })
		}, func(e *testEnv, p store.Pool) {
			p.Enabled = true
			if err := e.st.UpdatePool(e.ctx, p); err != nil {
				e.t.Fatal(err)
			}
			e.bus.Publish(events.PoolsChanged{})
		}},
		{"settings", func(e *testEnv) store.Pool {
			e.setSettings(func(s *store.Settings) { s.MinFreeDiskPercent = 60 }) // the server has 60% free
			return e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
		}, func(e *testEnv, _ store.Pool) {
			old, err := e.st.GetSettings(e.ctx)
			if err != nil {
				e.t.Fatal(err)
			}
			next, err := e.st.UpdateSettings(e.ctx, func(s *store.Settings) error {
				s.MinFreeDiskPercent = 15
				return nil
			})
			if err != nil {
				e.t.Fatal(err)
			}
			e.bus.Publish(events.SettingsChanged{Old: old, New: next})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.addServer("fsn1-a", capacity("x64", 16, 65536))
			pool := tt.setup(e)
			e.attach(pool)
			e.run()
			// Run resizes at its start, and then sets the max runners.
			e.rec.await(t, "max ")
			if calls := e.rec.matching("start "); len(calls) != 0 {
				t.Fatalf("starts before the change: %v", calls)
			}
			tt.change(e, pool)
			e.rec.await(t, "start ")
		})
	}
}

func TestCompletedJobKeepsWarmSandbox(t *testing.T) {
	e := newTestEnv(t)
	serverID, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
	_, sc, set := e.attach(pool)
	busy := e.insertSandbox(pool, set, serverID, sandbox.StateBusy, start)
	warm := e.insertSandbox(pool, set, serverID, sandbox.StateIdle, start.Add(time.Second))
	e.scale(sc, msg(1, started(101, "acme/api", 900, "build", busy)))

	// The runner of the busy sandbox ends with its job; the driver has not
	// reported the end yet.
	e.scale(sc, msg(0, completed(101, "acme/api", 900, "build", busy, "succeeded")))
	e.settle()
	if got := e.sandbox(warm.ID).State; got != sandbox.StateIdle {
		t.Errorf("warm sandbox state %q after the job completed, want idle", got)
	}
	if e.rec.index("stop "+warm.ID) >= 0 {
		t.Errorf("calls %v: stopped the warm sandbox", e.rec.calls)
	}
	if got := e.sandbox(busy.ID).State; got != sandbox.StateStopping {
		t.Errorf("sandbox of the completed job is %q, want stopping", got)
	}
	if n := len(driver.specs); n != 0 {
		t.Errorf("%d starts, want 0: the warm sandbox is enough", n)
	}
}

func TestScaleDownRemovesRunnerBeforeStop(t *testing.T) {
	e := newTestEnv(t)
	serverID, _ := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 2 })
	client, sc, set := e.attach(pool)
	e.scale(sc, msg(0))
	list := e.sandboxes(pool.ID)
	for _, b := range list {
		e.listening(serverID, b.ID)
	}
	// A busy sandbox whose job GitHub no longer counts ends with its job.
	busy := e.insertSandbox(pool, set, serverID, sandbox.StateBusy, start)
	slices.SortFunc(list, func(a, b store.Sandbox) int { return a.CreatedAt.Compare(b.CreatedAt) })
	taken, free := list[0], list[1]
	// GitHub gave a job to the runner of the oldest sandbox just now.
	client.busy[taken.RunnerID] = true

	pool.MinIdle = 0
	if err := e.st.UpdatePool(e.ctx, pool); err != nil {
		t.Fatal(err)
	}
	e.scale(sc, Message{})

	removeFree := e.rec.index("remove-runner " + strconv.FormatInt(free.RunnerID, 10))
	stopFree := e.rec.index("stop " + free.ID)
	if removeFree < 0 || stopFree < 0 || removeFree > stopFree {
		t.Errorf("calls %v: want the runner removed before the stop", e.rec.calls)
	}
	if e.rec.index("stop "+taken.ID) >= 0 || e.rec.index("stop "+busy.ID) >= 0 {
		t.Error("stopped the sandbox of a busy runner")
	}
	if got := e.sandbox(taken.ID).State; got != sandbox.StateIdle {
		t.Errorf("busy runner sandbox state %q, want idle", got)
	}
	if got := e.sandbox(free.ID).State; got != sandbox.StateStopping {
		t.Errorf("free sandbox state %q, want stopping", got)
	}
}

func TestRejectsPublicAndForkJobs(t *testing.T) {
	tests := []struct {
		name         string
		mode         sandbox.Mode // of the server's driver
		allowPublic  bool
		allowForks   bool
		check        JobCheck
		disabled     bool
		wantRejected bool
		wantAcquired bool
		wantChecks   int
	}{
		{"container, private", sandbox.ModeContainer, false, false, JobCheck{Private: true}, false, false, true, 1},
		// The setting takes the job, but the container server waits, so the job waits too.
		{"container, public", sandbox.ModeContainer, true, true, JobCheck{}, false, false, false, 1},
		{"vm, public, not allowed", sandbox.ModeVM, false, true, JobCheck{}, false, true, false, 1},
		{"vm, public, allowed", sandbox.ModeVM, true, true, JobCheck{}, false, false, true, 1},
		{"fork, not allowed", sandbox.ModeContainer, false, false, JobCheck{Private: true, Fork: true}, false, true, false, 1},
		{"fork, allowed", sandbox.ModeContainer, false, true, JobCheck{Private: true, Fork: true}, false, false, true, 1},
		{"pool disabled", sandbox.ModeContainer, false, false, JobCheck{Private: true}, true, true, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			// The setting says VM in each case: only the running mode counts.
			e.setSettings(func(s *store.Settings) {
				s.Mode, s.AllowPublicRepos, s.AllowForkPRs = sandbox.ModeVM, tt.allowPublic, tt.allowForks
			})
			e.gh.checks["acme/site"] = tt.check
			c := capacity("x64", 16, 65536)
			c.Mode = tt.mode
			e.addServer("fsn1-a", c)
			pool := e.addPool("runplane", func(p *store.Pool) { p.Enabled = !tt.disabled })
			client, sc, _ := e.attach(pool)

			// Two jobs of one run: GitHub is asked about the run one time.
			e.scale(sc, msg(0, available(21, "acme/site", 4242, "test"), available(22, "acme/site", 4242, "deploy")))

			if tt.wantAcquired != (len(client.acquired) > 0) {
				t.Errorf("acquire calls %v, want acquired = %v", client.acquired, tt.wantAcquired)
			}
			for _, id := range []int64{21, 22} {
				j := e.job(id)
				if tt.wantRejected && (j.RejectReason == "" || j.State != store.JobQueued) {
					t.Errorf("job %d: state %q, reject %q; want queued with a reason", id, j.State, j.RejectReason)
				}
				if !tt.wantRejected && j.RejectReason != "" {
					t.Errorf("job %d rejected: %q", id, j.RejectReason)
				}
			}
			if n := len(e.rec.matching("check ")); n != tt.wantChecks {
				t.Errorf("%d CheckJob calls, want %d", n, tt.wantChecks)
			}
		})
	}
}

func TestMaxRunners(t *testing.T) {
	tests := []struct {
		name       string
		maxRunners int
		disabled   bool
		want       int
	}{
		{"live plus fits", 10, false, 4},
		{"capped by max_runners", 2, false, 2},
		{"disabled pool", 10, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			// 8 vCPU: one live sandbox of 2 vCPU, room for 3 more.
			serverID, driver := e.addServer("fsn1-a", capacity("x64", 8, 65536))
			// A second server of another arch adds no room.
			e.addServer("fsn1-arm", capacity("arm64", 64, 262144))
			pool := e.addPool("runplane", func(p *store.Pool) { p.MaxRunners = tt.maxRunners; p.Enabled = !tt.disabled })
			client, sc, set := e.attach(pool)
			b := e.insertSandbox(pool, set, serverID, sandbox.StateBusy, start)
			driver.run(sandbox.Spec{ID: b.ID, VCPU: 2, MemoryMB: 4096})

			e.scale(sc, Message{})
			if client.max != tt.want {
				t.Errorf("max runners %d, want %d", client.max, tt.want)
			}
		})
	}
}

// A sandbox that does not start loses its runner on GitHub. When runplane
// stops during the start, the driver gives up the start, which is no
// failure of the sandbox.
func TestStartErrors(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		stop      bool // runplane stops while Start runs
		wantState sandbox.State
	}{
		{"start fails", errors.New("sysbox-runc: create container: no space left on device"), false, sandbox.StateFailed},
		{"runplane stops", fmt.Errorf("start container: %w", context.Canceled), true, sandbox.StateGone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			_, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
			driver.startErr = tt.err
			pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
			client, sc, _ := e.attach(pool)
			ctx, cancel := context.WithCancel(e.ctx)
			defer cancel()
			if tt.stop {
				driver.gate, driver.entered = make(chan struct{}), make(chan string, 1)
				go func() {
					<-driver.entered
					cancel()
					close(driver.gate)
				}()
			}

			if err := sc.Scale(ctx, msg(0)); err != nil {
				t.Fatal(err)
			}
			list := e.sandboxes(pool.ID)
			if len(list) != 1 {
				t.Fatalf("%d rows, want 1", len(list))
			}
			if b := list[0]; b.State != tt.wantState || b.StoppedAt == nil || (b.Error != "") != (tt.wantState == sandbox.StateFailed) {
				t.Errorf("row = state %q, stopped %v, error %q; want %q", b.State, b.StoppedAt, b.Error, tt.wantState)
			}
			if len(client.runners) != 0 {
				t.Errorf("runners on GitHub after the start: %v", client.runners)
			}
		})
	}
}

func TestPoolStatsEvent(t *testing.T) {
	e := newTestEnv(t)
	e.addServer("fsn1-a", capacity("x64", 2, 4096)) // room for one sandbox
	e.gh.checks["acme/site"] = JobCheck{Private: false}
	pool := e.addPool("runplane")
	_, sc, _ := e.attach(pool)
	ch := events.Subscribe[events.PoolStats](t.Context(), e.bus)
	last := func() events.PoolStats {
		t.Helper()
		var got *events.PoolStats
		for {
			select {
			case p := <-ch:
				if p.PoolID == pool.ID {
					got = &p
				}
			default:
				if got == nil {
					t.Fatal("no pool.stats event")
				}
				return *got
			}
		}
	}

	m := msg(0,
		available(31, "acme/site", 800, "build"), // public: rejected
		available(32, "acme/api", 801, "build"),  // acquired
		available(33, "acme/api", 801, "test"),   // waits: no room
	)
	m.Available[2].QueuedAt = start.Add(time.Second)
	e.scale(sc, m)
	// The acquired job and the job with no room wait for a runner.
	if got, want := last(), (events.PoolStats{PoolID: pool.ID, PoolCounts: store.PoolCounts{Queued: 2, Assigned: 1}}); got != want {
		t.Errorf("after the offer: %+v, want %+v", got, want)
	}

	e.scale(sc, msg(1))
	if got, want := last(), (events.PoolStats{PoolID: pool.ID, PoolCounts: store.PoolCounts{Starting: 1, Queued: 2, Assigned: 1}}); got != want {
		t.Errorf("after the start: %+v, want %+v", got, want)
	}
}

// The GitHub package may close a client after Detach returns, so Detach
// waits for a call in progress and the scheduler makes no later calls.
func TestDetachStopsClientCalls(t *testing.T) {
	e := newTestEnv(t)
	_, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	driver.startErr = errors.New("sysbox-runc: create container: image not found")
	driver.gate, driver.entered = make(chan struct{}), make(chan string, 1)
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
	client, sc, set := e.attach(pool)
	client.jitGate = make(chan struct{})
	client.jitEntered = make(chan struct{}, 1)

	scaled := make(chan error, 1)
	go func() { scaled <- sc.Scale(e.ctx, Message{}) }()
	<-client.jitEntered
	detached := make(chan struct{})
	go func() { e.s.Detach(set.ID); close(detached) }()
	// A Detach that does not wait returns at once. This wait cannot fail
	// when Detach waits.
	select {
	case <-detached:
		t.Fatal("Detach returned during a client call")
	case <-time.After(100 * time.Millisecond):
	}
	close(client.jitGate)
	<-detached
	// The start fails after Detach returned. It must not remove its runner
	// through the detached client.
	<-driver.entered
	close(driver.gate)
	if err := <-scaled; err != nil {
		t.Fatal(err)
	}
	if calls := e.rec.matching("remove-runner "); len(calls) != 0 {
		t.Errorf("client calls after Detach: %v", calls)
	}
}

// A server keeps an image only while a build row or a live sandbox row has
// its digest, and a pull can record a new digest while GitHub makes the
// runner of a start. Thus the row of the start comes first. A cleanup in
// that time keeps the row and the runner.
func TestStartWritesItsRowBeforeTheRunner(t *testing.T) {
	e := newTestEnv(t)
	serverID, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
	client, sc, _ := e.attach(pool)

	var rows []store.Sandbox
	e.duringJIT(sc, client, func() {
		var err error
		rows, err = e.st.ListSandboxes(e.ctx, store.SandboxFilter{ServerID: serverID, States: sandbox.LiveStates()})
		if err != nil {
			t.Fatal(err)
		}
		e.clock.add(2 * time.Minute)
		e.s.cleanup(e.ctx)
	})
	if len(rows) != 1 || rows[0].ImageDigest != e.fleet.imageRef.Digest || rows[0].RunnerVersion != "2.330.0" {
		t.Fatalf("live rows during the JIT request: %+v, want one with the image and the runner version of the start", rows)
	}
	if b := e.sandbox(rows[0].ID); b.State != sandbox.StateStarting || b.RunnerID != 1 {
		t.Errorf("row after the start: state %q, runner %d; want starting with runner 1", b.State, b.RunnerID)
	}
	if n := len(driver.specs); n != 1 {
		t.Errorf("%d starts, want 1", n)
	}
	if calls := e.rec.matching("remove-runner "); len(calls) != 0 {
		t.Errorf("the cleanup removed the runner of the start: %v", calls)
	}
}

// When GitHub makes no runner, for example in an outage, the row of the
// start goes away. A gone row would stay for each try.
func TestStartWithoutARunnerLeavesNoRow(t *testing.T) {
	e := newTestEnv(t)
	_, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
	client, sc, _ := e.attach(pool)
	client.jitErr = errors.New("make JIT config: 503 Service Unavailable")

	e.scale(sc, msg(0))
	if rows := e.sandboxes(pool.ID); len(rows) != 0 {
		t.Errorf("rows after the JIT error: %+v, want none", rows)
	}
	if n := len(driver.specs); n != 0 {
		t.Errorf("%d starts, want 0", n)
	}
}

// A user or the cleanup can stop a sandbox while GitHub makes its runner.
// The sandbox then does not start, and its runner goes away.
func TestStopDuringTheJITRequestCancelsTheStart(t *testing.T) {
	e := newTestEnv(t)
	e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
	client, sc, _ := e.attach(pool)

	var b store.Sandbox
	e.duringJIT(sc, client, func() {
		rows := e.sandboxes(pool.ID)
		if len(rows) != 1 {
			t.Fatalf("%d rows during the JIT request, want 1", len(rows))
		}
		b = rows[0]
		if err := e.s.StopSandbox(e.ctx, b.ID); err != nil {
			t.Fatal(err)
		}
	})
	if e.rec.index("start "+b.ID) >= 0 {
		t.Error("the stopped sandbox started")
	}
	if len(client.runners) != 0 {
		t.Errorf("runners on GitHub after the stop: %v", client.runners)
	}
	if got := e.sandbox(b.ID); got.State != sandbox.StateStopping || got.ExitReason != "stopped by a user" {
		t.Errorf("row: state %q, exit reason %q; want stopping, stopped by a user", got.State, got.ExitReason)
	}
}

// A warm sandbox must equal a new sandbox of its pool. Else the next job
// runs with the old size, image, or runner after a change. A warm sandbox
// that waits for a job for the whole time limit gives way to a new one
// before its guest stops it, so that a job never gets less than the limit.
func TestReplacesStaleWarmSandboxes(t *testing.T) {
	tests := []struct {
		name       string
		change     func(e *testEnv, p *store.Pool) // of a pool with a time limit of 60 minutes
		githubBusy bool                            // GitHub gave a job to the runner of the warm sandbox
		wantStop   bool
		wantStart  bool
	}{
		{"nothing changed", func(*testEnv, *store.Pool) {}, false, false, false},
		{"pool vCPU", func(_ *testEnv, p *store.Pool) { p.VCPU = 4 }, false, true, true},
		{"pool disk", func(_ *testEnv, p *store.Pool) { p.DiskGB = 128 }, false, true, true},
		{"pool arch with no server of the new arch", func(_ *testEnv, p *store.Pool) { p.Arch = "arm64" }, false, true, false},
		{"image digest on the server", func(e *testEnv, _ *store.Pool) {
			e.fleet.imageRef.Digest = "sha256:0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e"
		}, false, true, true},
		{"runner version", func(e *testEnv, _ *store.Pool) { e.fleet.runner = "2.331.0" }, false, true, true},
		{"runner busy on GitHub", func(e *testEnv, _ *store.Pool) { e.fleet.runner = "2.331.0" }, true, false, false},
		{"waited less than the time limit", func(e *testEnv, _ *store.Pool) { e.clock.add(59 * time.Minute) }, false, false, false},
		{"waited the whole time limit", func(e *testEnv, _ *store.Pool) { e.clock.add(60 * time.Minute) }, false, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			serverID, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
			pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle, p.TimeLimitMinutes = 2, 60 })
			client, sc, set := e.attach(pool)
			busy := e.insertSandbox(pool, set, serverID, sandbox.StateBusy, start)
			e.scale(sc, msg(0))
			warm := e.sandbox(driver.specs[0].ID)
			e.listening(serverID, warm.ID)
			client.busy[warm.RunnerID] = tt.githubBusy

			tt.change(e, &pool)
			if err := e.st.UpdatePool(e.ctx, pool); err != nil {
				t.Fatal(err)
			}
			e.scale(sc, Message{})

			stop := e.rec.index("stop " + warm.ID)
			if (stop >= 0) != tt.wantStop {
				t.Fatalf("calls %v: warm sandbox stopped = %v, want %v", e.rec.calls, stop >= 0, tt.wantStop)
			}
			if e.rec.index("stop "+busy.ID) >= 0 {
				t.Error("stopped the busy sandbox")
			}
			if started := len(driver.specs) > 1; started != tt.wantStart {
				t.Fatalf("new sandbox started = %v, want %v", started, tt.wantStart)
			}
			if !tt.wantStop {
				return
			}
			if remove := e.rec.index("remove-runner " + strconv.FormatInt(warm.RunnerID, 10)); remove < 0 || remove > stop {
				t.Errorf("calls %v: want the runner removed before the stop", e.rec.calls)
			}
			if got := e.sandbox(warm.ID).ExitReason; !strings.HasPrefix(got, "replaced: ") {
				t.Errorf("exit reason %q, want a replaced reason", got)
			}
			if !tt.wantStart {
				return
			}
			s := driver.specs[len(driver.specs)-1]
			if s.VCPU != pool.VCPU || s.DiskGB != pool.DiskGB || s.Image.Digest != e.fleet.imageRef.Digest || s.RunnerVersion != e.fleet.runner {
				t.Errorf("new sandbox spec %+v, want the pool %+v with image %s and runner %s", s, pool, e.fleet.imageRef.Digest, e.fleet.runner)
			}
		})
	}
}

// The cache server takes the refs of a job from the run data that the
// scheduler records before it takes the job, also when no setting needs a
// check of the run.
func TestRecordsRunData(t *testing.T) {
	e := newTestEnv(t)
	e.setSettings(func(s *store.Settings) { s.Mode, s.AllowPublicRepos, s.AllowForkPRs = sandbox.ModeVM, true, true })
	e.gh.checks["acme/api"] = JobCheck{Private: true, Ref: store.RunRef{Event: "pull_request", HeadBranch: "fix", PRNumber: 7, BaseRef: "main"}}
	e.addServer("fsn1-a", capacity("x64", 16, 65536))
	_, sc, _ := e.attach(e.addPool("runplane"))
	e.scale(sc, msg(0, available(41, "acme/api", 950, "test")))

	if j := e.job(41); j.Event != "pull_request" || j.HeadBranch != "fix" || j.PRNumber != 7 || j.BaseRef != "main" {
		t.Errorf("job run data: event %q branch %q pr %d base %q, want pull_request fix 7 main", j.Event, j.HeadBranch, j.PRNumber, j.BaseRef)
	}
}

// GitHub can give any job of a scale set to any of its runners, so with
// public repositories allowed, a container server takes no sandboxes, and a
// public job waits while a container runner of the scale set is live. The
// container server itself does not stop public jobs.
func TestPublicJobsNeverReachContainerServers(t *testing.T) {
	e := newTestEnv(t)
	e.setSettings(func(s *store.Settings) { s.Mode, s.AllowPublicRepos = sandbox.ModeVM, true })
	e.gh.checks["acme/site"] = JobCheck{}
	vm := capacity("x64", 4, 8192)
	vm.Mode = sandbox.ModeVM
	vmID, vmDriver := e.addServer("primary", vm)
	agentID, agentDriver := e.addServer("cheap-vps", capacity("x64", 64, 262144))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 4 })
	client, sc, set := e.attach(pool)
	// A warm container sandbox from before the setting changed.
	old := e.insertSandbox(pool, set, agentID, sandbox.StateIdle, start)
	agentDriver.run(sandbox.Spec{ID: old.ID})

	e.scale(sc, msg(0, available(51, "acme/site", 4300, "test")))
	if n := len(agentDriver.specs); n != 0 {
		t.Errorf("%d sandboxes started on the container server, want 0", n)
	}
	if n := len(vmDriver.specs); n != 2 {
		t.Errorf("%d sandboxes started on the VM server, want the 2 that fit", n)
	}
	if got := e.sandbox(old.ID); got.State != sandbox.StateStopping {
		t.Errorf("warm container sandbox: state %q, want stopping", got.State)
	}
	if j := e.job(51); j.RejectReason == "" || len(client.acquired) != 0 {
		t.Errorf("public job: reject %q, acquire calls %v; want a reject while a container runner is live", j.RejectReason, client.acquired)
	}

	// The container server stays online, but the scale set has only VM
	// runners now, so it takes the job.
	e.scale(sc, msg(0, available(51, "acme/site", 4300, "test")))
	if j := e.job(51); j.RejectReason != "" || j.State != store.JobAssigned {
		t.Errorf("public job with a container server online: state %q, reject %q; want assigned", j.State, j.RejectReason)
	}
	if n := len(agentDriver.specs); n != 0 {
		t.Errorf("%d sandboxes started on the container server, want 0", n)
	}
	for _, b := range e.sandboxes(pool.ID, sandbox.StateStarting) {
		if b.ServerID != vmID {
			t.Errorf("sandbox %s on server %s, want the VM server", b.ID, b.ServerID)
		}
	}
}

// After an admin turns allow_public_repos off, a public job that a scale
// set acquired before can still wait for a runner. GitHub can give it to
// each runner of the scale set, so a container server gets no runner of
// that scale set until the job stops waiting. Other scale sets get
// container runners at once.
func TestWaitingPublicJobKeepsContainerServersOut(t *testing.T) {
	e := newTestEnv(t)
	e.setSettings(func(s *store.Settings) { s.Mode, s.AllowPublicRepos = sandbox.ModeVM, true })
	e.gh.checks["acme/site"] = JobCheck{}
	vm := capacity("x64", 2, 4096) // room for one sandbox
	vm.Mode = sandbox.ModeVM
	_, vmDriver := e.addServer("primary", vm)
	_, agentDriver := e.addServer("cheap-vps", capacity("x64", 64, 262144))
	pool := e.addPool("runplane")
	client, sc, _ := e.attach(pool)
	otherPool := e.addPool("runplane-other")
	_, otherSC, _ := e.attach(otherPool)

	e.scale(sc, msg(0, available(51, "acme/site", 4300, "test")))
	if j := e.job(51); j.State != store.JobAssigned {
		t.Fatalf("public job: state %q, reject %q; want assigned while the setting is on", j.State, j.RejectReason)
	}

	e.setSettings(func(s *store.Settings) { s.AllowPublicRepos = false })
	// A job of another pool fills the VM server.
	vmDriver.run(sandbox.Spec{ID: "other", VCPU: 2, MemoryMB: 4096})
	e.scale(sc, msg(1))
	if n := len(agentDriver.specs); n != 0 {
		t.Errorf("%d sandboxes started on the container server while a public job waits, want 0", n)
	}
	if client.max != 0 {
		t.Errorf("max runners %d while a public job waits and the VM server is full, want 0", client.max)
	}
	e.scale(otherSC, msg(1))
	if specs := agentDriver.specs; len(specs) != 1 || specs[0].PoolID != otherPool.ID {
		t.Fatalf("container sandboxes %+v, want one for the other scale set", specs)
	}

	cancel := assigned(51, "acme/site", 4300, "test")
	cancel.State, cancel.Result = store.JobCompleted, store.ResultCancelled
	e.scale(sc, msg(1, cancel))
	e.settle()
	if specs := agentDriver.specs; len(specs) != 2 || specs[1].PoolID != pool.ID {
		t.Errorf("container sandboxes %+v, want one more for the scale set after the public job stopped waiting", specs)
	}
}

// GitHub cancels an assignment that no runner takes in time and offers the
// job again. runplane must take it again, and must not keep it as cancelled.
func TestRequeuedJobIsAcquiredAgain(t *testing.T) {
	e := newTestEnv(t)
	e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane")
	client, sc, _ := e.attach(pool)
	offer := msg(0, available(31, "acme/api", 960, "build"))
	e.scale(sc, offer)
	e.scale(sc, msg(1, assigned(31, "acme/api", 960, "build")))
	cancel := assigned(31, "acme/api", 960, "build")
	cancel.State, cancel.Result = store.JobCompleted, store.ResultCancelled
	e.scale(sc, msg(0, cancel))
	e.settle()

	e.scale(sc, offer)
	if calls := client.acquired; len(calls) != 2 || !slices.Equal(calls[1], []int64{31}) {
		t.Errorf("acquire calls %v, want [[31] [31]]", calls)
	}
	if j := e.job(31); j.State != store.JobAssigned || j.Result != "" || j.FinishedAt != nil {
		t.Errorf("job = state %q, result %q, finished %v; want assigned again", j.State, j.Result, j.FinishedAt)
	}
	if n := len(e.rec.matching("download ")); n != 0 {
		t.Errorf("%d log downloads for a job that never ran, want 0", n)
	}
}

// A job that ran keeps its result when an old offer comes again.
func TestCancelledJobThatRanStaysCompleted(t *testing.T) {
	e := newTestEnv(t)
	e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
	client, sc, _ := e.attach(pool)
	e.scale(sc, msg(0))
	b := e.sandboxes(pool.ID)[0]
	offer := msg(0, available(32, "acme/api", 961, "build"))
	e.scale(sc, offer)
	e.scale(sc, msg(1, started(32, "acme/api", 961, "build", b)))
	e.scale(sc, msg(0, completed(32, "acme/api", 961, "build", b, store.ResultCancelled)))
	e.settle()

	e.scale(sc, offer)
	if n := len(client.acquired); n != 1 {
		t.Errorf("%d AcquireJobs calls, want 1", n)
	}
	if j := e.job(32); j.State != store.JobCompleted || j.Result != store.ResultCancelled {
		t.Errorf("job = state %q, result %q; want completed cancelled", j.State, j.Result)
	}
}

// One job whose run runplane cannot read must not hold up the other jobs of
// the message: the listener acks a message only when Scale returns nil.
// runplane reads a run that it can never read one time, but it reads a run
// again after a GitHub outage.
func TestCheckErrorDoesNotBlockTheMessage(t *testing.T) {
	e := newTestEnv(t)
	e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane")
	client, sc, _ := e.attach(pool)
	e.gh.checkErrs = map[string]error{
		"acme/not-selected": fmt.Errorf("%w: GitHub API GET /repos/acme/not-selected/actions/runs/1: 404 Not Found", ErrRunUnreadable),
		"acme/flaky":        errors.New("GitHub API GET /repos/acme/flaky/actions/runs/2: 502 Bad Gateway"),
	}
	m := msg(0,
		available(61, "acme/not-selected", 1, "test"),
		available(62, "acme/flaky", 2, "test"),
		available(63, "acme/api", 3, "test"),
	)
	for range 2 {
		if err := sc.Scale(e.ctx, m); err != nil {
			t.Fatalf("Scale: %v, want nil so that the listener acks the message", err)
		}
	}

	if calls := client.acquired; len(calls) != 1 || !slices.Equal(calls[0], []int64{63}) {
		t.Errorf("acquire calls %v, want [[63]]", calls)
	}
	if j := e.job(61); j.State != store.JobQueued || !strings.Contains(j.RejectReason, "install the GitHub App") {
		t.Errorf("unreadable job: state %q, reject %q; want queued with the install reason", j.State, j.RejectReason)
	}
	if j := e.job(62); j.State != store.JobQueued || j.RejectReason != "" {
		t.Errorf("job with a GitHub outage: state %q, reject %q; want queued with no reason", j.State, j.RejectReason)
	}

	e.gh.mu.Lock()
	delete(e.gh.checkErrs, "acme/flaky")
	e.gh.mu.Unlock()
	e.scale(sc, m)
	if calls := client.acquired; len(calls) != 2 || !slices.Equal(calls[1], []int64{62}) {
		t.Errorf("acquire calls %v after the outage, want [[63] [62]]", calls)
	}
	if calls := e.rec.matching("check acme/not-selected "); len(calls) != 1 {
		t.Errorf("CheckJob calls %v for three offers of a run that runplane cannot read, want 1", calls)
	}
}

// The job page shows the steps of a running job as GitHub reports them, not
// only the list from the job start.
func TestStepsRefreshWhileTheJobRuns(t *testing.T) {
	every := stepsEvery
	stepsEvery = 5 * time.Millisecond
	t.Cleanup(func() { stepsEvery = every })
	e := newTestEnv(t)
	e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
	_, sc, _ := e.attach(pool)
	e.scale(sc, msg(0))
	b := e.sandboxes(pool.ID)[0]
	jobs := events.Subscribe[events.JobState](t.Context(), e.bus)
	e.scale(sc, msg(1, started(81, "acme/api", 980, "build", b)))
	// The run data comes with the steps of that moment: none.
	next(t, jobs, "the run data of the job", func(j events.JobState) bool { return j.RunnerRequestID == 81 && j.GitHubJobID != 0 })

	now := start.Add(time.Minute)
	e.gh.mu.Lock()
	e.gh.steps = []store.JobStep{
		{Number: 1, Name: "Set up job", State: "completed", Conclusion: "success", StartedAt: &start, CompletedAt: &now},
		{Number: 2, Name: "Run tests", State: "in_progress", StartedAt: &now},
	}
	e.gh.mu.Unlock()
	j := next(t, jobs, "the steps of the running job", func(j events.JobState) bool { return j.RunnerRequestID == 81 && len(j.Steps) == 2 })
	if got := j.Steps[1]; got.Name != "Run tests" || got.State != "in_progress" {
		t.Errorf("step 2 = %+v, want Run tests in progress", got)
	}
	if got := e.job(81).Steps; len(got) != 2 {
		t.Errorf("stored steps %+v, want the 2 of the event", got)
	}

	// The reads of the steps end with the job: the background work of the
	// scheduler then ends.
	e.scale(sc, msg(0, completed(81, "acme/api", 980, "build", b, store.ResultSucceeded)))
	settled := make(chan struct{})
	go func() {
		e.settle()
		close(settled)
	}()
	select {
	case <-settled:
	case <-time.After(timeout):
		t.Fatalf("the steps of the completed job are still read: %d reads", len(e.rec.matching("steps ")))
	}
}
