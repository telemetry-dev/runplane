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

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

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

			jobs := []*scaleset.JobAvailable{
				available(11, "acme/api", 900, "test (1)"),
				available(12, "acme/api", 900, "test (2)"),
				available(13, "acme/api", 900, "test (3)"),
				available(14, "acme/api", 900, "lint"),
			}
			for i, offset := range []int{3, 1, 2, 0} {
				jobs[i].QueueTime = start.Add(time.Duration(offset) * time.Second)
			}
			e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(tt.assigned), JobAvailableMessages: jobs})

			var got []int64
			if calls := client.acquireCalls(); len(calls) > 0 {
				got = calls[0]
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

	offer := &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(0),
		JobAvailableMessages: []*scaleset.JobAvailable{available(101, "acme/api", 900, "build")}}
	assign := &scaleset.RunnerScaleSetMessage{MessageID: 2, Statistics: stats(1),
		JobAssignedMessages: []*scaleset.JobAssigned{assigned(101, "acme/api", 900, "build")}}
	for _, msg := range []*scaleset.RunnerScaleSetMessage{offer, assign, assign, offer} {
		e.scale(sc, msg)
	}
	if n := len(driver.startedSpecs()); n != 1 {
		t.Fatalf("%d sandbox starts, want 1", n)
	}
	if n := len(e.rec.matching("jit ")); n != 1 {
		t.Errorf("%d JIT configs, want 1", n)
	}
	if n := len(client.acquireCalls()); n != 1 {
		t.Errorf("%d AcquireJobs calls, want 1", n)
	}

	b := e.sandboxes(pool.ID)[0]
	done := &scaleset.RunnerScaleSetMessage{MessageID: 3, Statistics: stats(0), JobCompletedMessages: []*scaleset.JobCompleted{
		completed(101, "acme/api", 900, "build", b, "succeeded")}}
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
	serverID, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 2 })
	_, sc, set := e.attach(pool)

	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: listener.InitialMessageID, Statistics: stats(0)})
	specs := driver.startedSpecs()
	if len(specs) != 2 {
		t.Fatalf("%d starts, want 2", len(specs))
	}
	spec := specs[0]
	b := e.sandbox(spec.ID)
	if b.State != sandbox.StateStarting || b.ScaleSetID != set.ID || b.RunnerID == 0 || b.RunnerName != "runplane-"+spec.ID[len(spec.ID)-8:] {
		t.Errorf("row = %+v", b)
	}
	if spec.TimeLimit != 360*time.Minute || spec.JITConfig == "" || spec.Labels[sandbox.LabelSandbox] != spec.ID ||
		spec.Labels[LabelPool] != pool.ID || spec.Image.Digest != b.ImageDigest || spec.RunnerVersion != "2.330.0" {
		t.Errorf("spec = %+v", spec)
	}

	for _, s := range specs {
		e.event(serverID, s.ID, runnerEvent(sandbox.RunnerListening))
	}
	if n := len(e.sandboxes(pool.ID, sandbox.StateIdle)); n != 2 {
		t.Fatalf("%d idle sandboxes, want 2", n)
	}

	// A warm sandbox that goes away gets a replacement at the next poll.
	e.event(serverID, specs[1].ID, stateEvent(sandbox.StateGone, "exited"))
	e.scale(sc, nil)
	if n := len(driver.startedSpecs()); n != 3 {
		t.Fatalf("%d starts after a warm sandbox went away, want 3", n)
	}

	// An assigned job adds one sandbox, so that two stay warm.
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 5, Statistics: stats(1)})
	if n := len(e.sandboxes(pool.ID, runnerStates...)); n != 3 {
		t.Errorf("%d runners with one assigned job, want 3", n)
	}
}

func TestCompletedJobKeepsWarmSandbox(t *testing.T) {
	e := newTestEnv(t)
	serverID, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
	_, sc, set := e.attach(pool)
	busy := e.insertSandbox(pool, set, serverID, sandbox.StateBusy, start)
	warm := e.insertSandbox(pool, set, serverID, sandbox.StateIdle, start.Add(time.Second))
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(1),
		JobStartedMessages: []*scaleset.JobStarted{started(101, "acme/api", 900, "build", busy)}})

	// The runner of the busy sandbox ends with its job; the driver has not
	// reported the end yet.
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 2, Statistics: stats(0),
		JobCompletedMessages: []*scaleset.JobCompleted{completed(101, "acme/api", 900, "build", busy, "succeeded")}})
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
	if n := len(driver.startedSpecs()); n != 0 {
		t.Errorf("%d starts, want 0: the warm sandbox is enough", n)
	}
}

func TestScaleDownRemovesRunnerBeforeStop(t *testing.T) {
	e := newTestEnv(t)
	serverID, _ := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 2 })
	client, sc, set := e.attach(pool)
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: listener.InitialMessageID, Statistics: stats(0)})
	list := e.sandboxes(pool.ID)
	for _, b := range list {
		e.event(serverID, b.ID, runnerEvent(sandbox.RunnerListening))
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
	e.scale(sc, nil)

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
		wantChecks   int
	}{
		{"container, private", sandbox.ModeContainer, false, false, JobCheck{Private: true}, false, false, 1},
		{"container, public", sandbox.ModeContainer, true, true, JobCheck{}, false, true, 1},
		{"vm, public, not allowed", sandbox.ModeVM, false, true, JobCheck{}, false, true, 1},
		{"vm, public, allowed", sandbox.ModeVM, true, true, JobCheck{}, false, false, 1},
		{"fork, not allowed", sandbox.ModeContainer, false, false, JobCheck{Private: true, Fork: true}, false, true, 1},
		{"fork, allowed", sandbox.ModeContainer, false, true, JobCheck{Private: true, Fork: true}, false, false, 1},
		{"pool disabled", sandbox.ModeContainer, false, false, JobCheck{Private: true}, true, true, 0},
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
			e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(0), JobAvailableMessages: []*scaleset.JobAvailable{
				available(21, "acme/site", 4242, "test"), available(22, "acme/site", 4242, "deploy"),
			}})

			acquired := client.acquireCalls()
			if tt.wantRejected != (len(acquired) == 0) {
				t.Errorf("acquire calls %v, rejected = %v", acquired, tt.wantRejected)
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
			if e.gh.checkCalls != tt.wantChecks {
				t.Errorf("%d CheckJob calls, want %d", e.gh.checkCalls, tt.wantChecks)
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
			client, _, set := e.attach(pool)
			b := e.insertSandbox(pool, set, serverID, sandbox.StateBusy, start)
			driver.running[b.ID] = sandbox.Spec{ID: b.ID, VCPU: 2, MemoryMB: 4096}

			e.s.updateMaxRunners(e.ctx)
			if got := client.maxRunners(); got != tt.want {
				t.Errorf("max runners %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFailedStartMarksSandboxFailedAndRemovesRunner(t *testing.T) {
	e := newTestEnv(t)
	_, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	driver.startErr = errors.New("sysbox-runc: create container: no space left on device")
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
	client, sc, _ := e.attach(pool)

	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: listener.InitialMessageID, Statistics: stats(0)})
	list := e.sandboxes(pool.ID)
	if len(list) != 1 {
		t.Fatalf("%d rows, want 1", len(list))
	}
	b := list[0]
	if b.State != sandbox.StateFailed || b.StoppedAt == nil || b.Error == "" {
		t.Errorf("row = state %q, stopped %v, error %q", b.State, b.StoppedAt, b.Error)
	}
	if _, ok := client.runners[b.RunnerID]; ok {
		t.Error("the runner of the failed sandbox is still on GitHub")
	}
}

// runplane stops while a sandbox starts. The driver gives up the start,
// which is no failure of the sandbox.
func TestCanceledStartMarksSandboxGone(t *testing.T) {
	e := newTestEnv(t)
	_, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	driver.gate, driver.entered = make(chan struct{}), make(chan string, 1)
	driver.startErr = fmt.Errorf("start container: %w", context.Canceled)
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
	client, sc, _ := e.attach(pool)

	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc.Scale(ctx, &scaleset.RunnerScaleSetMessage{MessageID: listener.InitialMessageID, Statistics: stats(0)})
	}()
	<-driver.entered
	cancel()
	close(driver.gate)
	<-done

	list := e.sandboxes(pool.ID)
	if len(list) != 1 {
		t.Fatalf("%d rows, want 1", len(list))
	}
	if b := list[0]; b.State != sandbox.StateGone || b.StoppedAt == nil {
		t.Errorf("row = state %q, stopped %v, error %q; want gone", b.State, b.StoppedAt, b.Error)
	}
	if len(client.runners) != 0 {
		t.Errorf("runners on GitHub after the canceled start: %v", client.runners)
	}
}

func TestPoolStatsEvent(t *testing.T) {
	e := newTestEnv(t)
	e.addServer("fsn1-a", capacity("x64", 2, 4096)) // room for one sandbox
	e.gh.checks["acme/site"] = JobCheck{Private: false}
	pool := e.addPool("runplane")
	_, sc, _ := e.attach(pool)
	ch, cancel := e.bus.Subscribe(256)
	defer cancel()
	last := func() poolStats {
		t.Helper()
		var got *poolStats
		for {
			select {
			case ev := <-ch:
				if p, ok := ev.Data.(poolStats); ok && ev.Type == events.PoolStats && p.PoolID == pool.ID {
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

	jobs := []*scaleset.JobAvailable{
		available(31, "acme/site", 800, "build"), // public: rejected
		available(32, "acme/api", 801, "build"),  // acquired
		available(33, "acme/api", 801, "test"),   // waits: no room
	}
	jobs[2].QueueTime = start.Add(time.Second)
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(0), JobAvailableMessages: jobs})
	if got, want := last(), (poolStats{PoolID: pool.ID, Queued: 1, Assigned: 1}); got != want {
		t.Errorf("after the offer: %+v, want %+v", got, want)
	}

	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 2, Statistics: stats(1)})
	if got, want := last(), (poolStats{PoolID: pool.ID, Starting: 1, Queued: 1, Assigned: 1}); got != want {
		t.Errorf("after the start: %+v, want %+v", got, want)
	}
}

// The GitHub package may close a client after Detach returns, so Detach
// waits for a call in progress and the scheduler makes no later calls.
func TestDetachStopsClientCalls(t *testing.T) {
	e := newTestEnv(t)
	_, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	driver.startErr = errors.New("sysbox-runc: create container: image not found")
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
	client, _, set := e.attach(pool)
	client.jitGate = make(chan struct{})
	client.jitEntered = make(chan struct{}, 1)

	resized := make(chan struct{})
	go func() { e.s.resizeAll(e.ctx); close(resized) }()
	<-client.jitEntered
	detached := make(chan struct{})
	go func() { e.s.Detach(set.ID); close(detached) }()
	select {
	case <-detached:
		t.Fatal("Detach returned during a client call")
	case <-time.After(100 * time.Millisecond):
	}
	close(client.jitGate)
	<-detached
	<-resized

	// The failed start must not remove its runner through the detached client.
	if calls := e.rec.matching("remove-runner "); len(calls) != 0 {
		t.Errorf("client calls after Detach: %v", calls)
	}
}

// A warm sandbox must equal a new sandbox of its pool. Else the next job
// runs with the old size, image, or runner after a change.
func TestReplacesStaleWarmSandboxes(t *testing.T) {
	tests := []struct {
		name       string
		change     func(e *testEnv, p *store.Pool)
		githubBusy bool // GitHub gave a job to the runner of the warm sandbox
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			serverID, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
			pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 2 })
			client, sc, set := e.attach(pool)
			busy := e.insertSandbox(pool, set, serverID, sandbox.StateBusy, start)
			e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: listener.InitialMessageID, Statistics: stats(0)})
			var warm store.Sandbox
			for _, b := range e.sandboxes(pool.ID) {
				if b.ID != busy.ID {
					warm = b
				}
			}
			e.event(serverID, warm.ID, runnerEvent(sandbox.RunnerListening))
			client.busy[warm.RunnerID] = tt.githubBusy

			tt.change(e, &pool)
			if err := e.st.UpdatePool(e.ctx, pool); err != nil {
				t.Fatal(err)
			}
			e.scale(sc, nil)

			stop := e.rec.index("stop " + warm.ID)
			if (stop >= 0) != tt.wantStop {
				t.Fatalf("calls %v: warm sandbox stopped = %v, want %v", e.rec.calls, stop >= 0, tt.wantStop)
			}
			if e.rec.index("stop "+busy.ID) >= 0 {
				t.Error("stopped the busy sandbox")
			}
			specs := driver.startedSpecs()
			if started := len(specs) > 1; started != tt.wantStart {
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
			s := specs[len(specs)-1]
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
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(0),
		JobAvailableMessages: []*scaleset.JobAvailable{available(41, "acme/api", 950, "test")}})

	if j := e.job(41); j.Event != "pull_request" || j.HeadBranch != "fix" || j.PRNumber != 7 || j.BaseRef != "main" {
		t.Errorf("job run data: event %q branch %q pr %d base %q, want pull_request fix 7 main", j.Event, j.HeadBranch, j.PRNumber, j.BaseRef)
	}
}

// GitHub can give any job of a scale set to any of its runners, so with
// public repositories allowed, a container server takes no sandboxes, and a
// public job waits while a container runner exists.
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
	agentDriver.running[old.ID] = sandbox.Spec{ID: old.ID}

	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(0),
		JobAvailableMessages: []*scaleset.JobAvailable{available(51, "acme/site", 4300, "test")}})

	if n := len(agentDriver.startedSpecs()); n != 0 {
		t.Errorf("%d sandboxes started on the container server, want 0", n)
	}
	if n := len(vmDriver.startedSpecs()); n != 2 {
		t.Errorf("%d sandboxes started on the VM server, want the 2 that fit", n)
	}
	if got := e.sandbox(old.ID); got.State != sandbox.StateStopping {
		t.Errorf("warm container sandbox: state %q, want stopping", got.State)
	}
	if j := e.job(51); j.RejectReason == "" || len(client.acquireCalls()) != 0 {
		t.Errorf("public job: reject %q, acquire calls %v; want a reject while a container server is online", j.RejectReason, client.acquireCalls())
	}

	// Without the container server, the VM servers take the job.
	e.fleet.mu.Lock()
	e.fleet.offline[agentID] = true
	e.fleet.mu.Unlock()
	e.s.cleanup(e.ctx)
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 2, Statistics: stats(0),
		JobAvailableMessages: []*scaleset.JobAvailable{available(51, "acme/site", 4300, "test")}})
	if j := e.job(51); j.RejectReason != "" || j.State != store.JobAssigned {
		t.Errorf("public job with VM servers only: state %q, reject %q; want assigned", j.State, j.RejectReason)
	}
	for _, b := range e.sandboxes(pool.ID, sandbox.StateStarting) {
		if b.ServerID != vmID {
			t.Errorf("sandbox %s on server %s, want the VM server", b.ID, b.ServerID)
		}
	}
}

// GitHub cancels an assignment that no runner takes in time and offers the
// job again. runplane must take it again, and must not keep it as cancelled.
func TestRequeuedJobIsAcquiredAgain(t *testing.T) {
	e := newTestEnv(t)
	e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane")
	client, sc, _ := e.attach(pool)
	offer := &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(0),
		JobAvailableMessages: []*scaleset.JobAvailable{available(31, "acme/api", 960, "build")}}
	e.scale(sc, offer)
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 2, Statistics: stats(1),
		JobAssignedMessages: []*scaleset.JobAssigned{assigned(31, "acme/api", 960, "build")}})
	cancel := base(31, "acme/api", 960, "build", scaleset.MessageTypeJobCompleted)
	cancel.ScaleSetAssignTime = start.Add(time.Second)
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 3, Statistics: stats(0),
		JobCompletedMessages: []*scaleset.JobCompleted{{Result: "Canceled", JobMessageBase: cancel}}})
	e.settle()

	offer.MessageID = 4
	e.scale(sc, offer)
	if calls := client.acquireCalls(); len(calls) != 2 || !slices.Equal(calls[1], []int64{31}) {
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
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: listener.InitialMessageID, Statistics: stats(0)})
	b := e.sandboxes(pool.ID)[0]
	offer := &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(0),
		JobAvailableMessages: []*scaleset.JobAvailable{available(32, "acme/api", 961, "build")}}
	e.scale(sc, offer)
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 2, Statistics: stats(1),
		JobStartedMessages: []*scaleset.JobStarted{started(32, "acme/api", 961, "build", b)}})
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 3, Statistics: stats(0),
		JobCompletedMessages: []*scaleset.JobCompleted{completed(32, "acme/api", 961, "build", b, "Canceled")}})
	e.settle()

	offer.MessageID = 4
	e.scale(sc, offer)
	if n := len(client.acquireCalls()); n != 1 {
		t.Errorf("%d AcquireJobs calls, want 1", n)
	}
	if j := e.job(32); j.State != store.JobCompleted || j.Result != store.ResultCancelled {
		t.Errorf("job = state %q, result %q; want completed cancelled", j.State, j.Result)
	}
}

// One job whose run runplane cannot read must not hold up the other jobs of
// the message: the listener acks a message only when Scale returns nil.
func TestCheckErrorDoesNotBlockTheMessage(t *testing.T) {
	e := newTestEnv(t)
	e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane")
	client, sc, _ := e.attach(pool)
	e.gh.checkErrs = map[string]error{
		"acme/not-selected": fmt.Errorf("%w: GitHub API GET /repos/acme/not-selected/actions/runs/1: 404 Not Found", ErrRunUnreadable),
		"acme/flaky":        errors.New("GitHub API GET /repos/acme/flaky/actions/runs/2: 502 Bad Gateway"),
	}
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(0), JobAvailableMessages: []*scaleset.JobAvailable{
		available(61, "acme/not-selected", 1, "test"),
		available(62, "acme/flaky", 2, "test"),
		available(63, "acme/api", 3, "test"),
	}}
	for range 2 {
		if err := sc.Scale(e.ctx, msg); err != nil {
			t.Fatalf("Scale: %v, want nil so that the listener acks the message", err)
		}
	}

	if calls := client.acquireCalls(); len(calls) != 1 || !slices.Equal(calls[0], []int64{63}) {
		t.Errorf("acquire calls %v, want [[63]]", calls)
	}
	if j := e.job(61); j.State != store.JobQueued || !strings.Contains(j.RejectReason, "install the GitHub App") {
		t.Errorf("unreadable job: state %q, reject %q; want queued with the install reason", j.State, j.RejectReason)
	}
	if j := e.job(62); j.State != store.JobQueued || j.RejectReason != "" {
		t.Errorf("job with a GitHub outage: state %q, reject %q; want queued with no reason", j.State, j.RejectReason)
	}
	// Two deliveries: the unreadable run is read one time, the failing run each time.
	if e.gh.checkCalls != 1+2+1 {
		t.Errorf("%d CheckJob calls, want 4", e.gh.checkCalls)
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
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(0)})
	b := e.sandboxes(pool.ID)[0]
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 2, Statistics: stats(1),
		JobStartedMessages: []*scaleset.JobStarted{started(81, "acme/api", 980, "build", b)}})

	now := start.Add(time.Minute)
	e.gh.mu.Lock()
	e.gh.steps = []store.JobStep{
		{Number: 1, Name: "Set up job", Status: "completed", Conclusion: "success", StartedAt: &start, CompletedAt: &now},
		{Number: 2, Name: "Run tests", Status: "in_progress", StartedAt: &now},
	}
	e.gh.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.job(81).Steps) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("the steps of the running job stay as they were at the start")
		}
		time.Sleep(time.Millisecond)
	}
	if got := e.job(81).Steps[1]; got.Name != "Run tests" || got.Status != "in_progress" {
		t.Errorf("step 2 = %+v, want Run tests in progress", got)
	}

	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 3, Statistics: stats(0),
		JobCompletedMessages: []*scaleset.JobCompleted{completed(81, "acme/api", 980, "build", b, "Succeeded")}})
	time.Sleep(50 * time.Millisecond)
	reads := len(e.rec.matching("steps "))
	time.Sleep(50 * time.Millisecond)
	if n := len(e.rec.matching("steps ")); n != reads {
		t.Errorf("%d step reads after the job completed, want none", n-reads)
	}
}

// GitHub sends runnerRequestId 0 in scale set messages and names each job by
// its jobId. Two such jobs must stay two job rows, each with its own sandbox.
func TestJobsWithoutRequestIDsStayApart(t *testing.T) {
	e := newTestEnv(t)
	serverID, _ := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane")
	_, sc, set := e.attach(pool)
	first := e.insertSandbox(pool, set, serverID, sandbox.StateIdle, start)
	second := e.insertSandbox(pool, set, serverID, sandbox.StateIdle, start.Add(time.Second))

	lint := started(0, "acme/api", 900, "lint", first)
	lint.JobID = "e8c96f8d-1f58-5afe-b930-91e0f3f291fe"
	test := started(0, "acme/api", 900, "test", second)
	test.JobID = "eb596b22-9a5a-56b8-bf07-7b3f1be34661"
	e.scale(sc, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: stats(2),
		JobStartedMessages: []*scaleset.JobStarted{lint, test}})

	jobs, _, err := e.st.ListJobs(e.ctx, store.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, j := range jobs {
		names[j.JobName] = j.SandboxID
	}
	if len(jobs) != 2 || names["lint"] != first.ID || names["test"] != second.ID {
		t.Errorf("jobs %v (name -> sandbox), want lint on %s and test on %s", names, first.ID, second.ID)
	}
}
