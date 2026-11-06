package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// The cleanup stops each sandbox that has no live row, and sets each live
// row with no sandbox on a connected or removed server to gone. A draining
// server is connected, so its sandboxes get the cleanup too.
func TestCleanupSyncsServers(t *testing.T) {
	e := newTestEnv(t)
	serverID, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	offlineID, _ := e.addServer("fsn1-b", capacity("x64", 16, 65536))
	e.fleet.disconnected[offlineID] = true
	drainingID, draining := e.addServer("fsn1-c", capacity("x64", 16, 65536))
	e.fleet.notOnline[drainingID] = true
	pool := e.addPool("runplane")
	_, _, set := e.attach(pool)

	present := e.insertSandbox(pool, set, serverID, sandbox.StateIdle, start)
	gone := e.insertSandbox(pool, set, serverID, sandbox.StateIdle, start)
	e.advance(gone.ID, sandbox.StateGone, "removed", start)
	orphan := "01jbz4v5k2n8q3r6t9w0x1y2z3"
	for _, id := range []string{present.ID, gone.ID, orphan} {
		driver.run(sandbox.Spec{ID: id})
	}
	drainingOrphan := "01jbz4v5k2n8q3r6t9w0x1y2z4"
	draining.run(sandbox.Spec{ID: drainingOrphan})
	drainingMissing := e.insertSandbox(pool, set, drainingID, sandbox.StateIdle, start)
	missing := e.insertSandbox(pool, set, serverID, sandbox.StateBusy, start)
	unreachable := e.insertSandbox(pool, set, offlineID, sandbox.StateIdle, start)
	removedServer := e.insertSandbox(pool, set, "01jbz0000000000000000srv99", sandbox.StateIdle, start)
	e.clock.add(2 * time.Minute)
	// A row made at this moment can still wait for its Driver.Start.
	fresh := e.insertSandbox(pool, set, serverID, sandbox.StateStarting, e.clock.now())

	e.s.cleanup(e.ctx)
	got := e.rec.matching("stop ")
	slices.Sort(got)
	want := []string{"stop " + orphan, "stop " + gone.ID, "stop " + drainingOrphan}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("stops %v, want %v", got, want)
	}
	for _, tt := range []struct {
		b    store.Sandbox
		want sandbox.State
	}{
		{missing, sandbox.StateGone},
		{drainingMissing, sandbox.StateGone},
		{removedServer, sandbox.StateGone},
		{present, sandbox.StateIdle},
		{unreachable, sandbox.StateIdle},
		{fresh, sandbox.StateStarting},
	} {
		got := e.sandbox(tt.b.ID)
		if got.State != tt.want {
			t.Errorf("sandbox on %s created %v: state %q, want %q", tt.b.ServerID, tt.b.CreatedAt, got.State, tt.want)
		}
		if tt.want == sandbox.StateGone && (got.StoppedAt == nil || !got.StoppedAt.Equal(e.clock.now())) {
			t.Errorf("stopped at %v, want %v", got.StoppedAt, e.clock.now())
		}
	}
}

func TestCleanupRemovesRunnersWithoutSandbox(t *testing.T) {
	e := newTestEnv(t)
	serverID, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane")
	client, _, set := e.attach(pool)
	withSandbox := e.insertSandbox(pool, set, serverID, sandbox.StateIdle, start)
	driver.run(sandbox.Spec{ID: withSandbox.ID})
	goneSandbox := e.insertSandbox(pool, set, serverID, sandbox.StateIdle, start)
	e.advance(goneSandbox.ID, sandbox.StateGone, "removed", start)
	client.runners = map[int64]Runner{
		withSandbox.RunnerID: {ID: withSandbox.RunnerID, Name: withSandbox.RunnerName},
		goneSandbox.RunnerID: {ID: goneSandbox.RunnerID, Name: goneSandbox.RunnerName},
		501:                  {ID: 501, Name: "runplane-7q2m9x4k"},
		502:                  {ID: 502, Name: "runplane-3c8v1n6p"},
	}
	client.busy[502] = true
	e.clock.add(2 * time.Minute)

	e.s.cleanup(e.ctx)
	got := e.rec.matching("remove-runner ")
	slices.Sort(got)
	want := []string{"remove-runner 501", fmt.Sprintf("remove-runner %d", goneSandbox.RunnerID)}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("removed %v, want %v", got, want)
	}
}

func TestCleanupStopsOverdueSandboxes(t *testing.T) {
	e := newTestEnv(t)
	serverID, driver := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.TimeLimitMinutes = 60 })
	_, _, set := e.attach(pool)
	now := start.Add(61 * time.Minute)

	over := e.insertSandbox(pool, set, serverID, sandbox.StateIdle, start)
	e.advance(over.ID, sandbox.StateBusy, "", start)
	// A warm sandbox that waited 55 minutes gives its job the whole limit.
	warm := e.insertSandbox(pool, set, serverID, sandbox.StateIdle, start)
	e.advance(warm.ID, sandbox.StateBusy, "", start.Add(55*time.Minute))
	under := e.insertSandbox(pool, set, serverID, sandbox.StateIdle, start.Add(2*time.Minute))
	held := e.insertSandbox(pool, set, serverID, sandbox.StateStopping, start)
	if err := e.st.SetSandboxHold(e.ctx, held.ID, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	holdEnded := e.insertSandbox(pool, set, serverID, sandbox.StateStopping, start.Add(30*time.Minute))
	if err := e.st.SetSandboxHold(e.ctx, holdEnded.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	orphanPool := e.addPool("runplane-old")
	waiting := e.insertSandbox(orphanPool, set, serverID, sandbox.StateIdle, start.Add(50*time.Minute))
	if err := e.st.DeletePool(e.ctx, orphanPool.ID); err != nil {
		t.Fatal(err)
	}
	for _, b := range []store.Sandbox{over, warm, under, held, holdEnded, waiting} {
		driver.run(sandbox.Spec{ID: b.ID})
	}
	e.clock.add(61 * time.Minute)

	e.s.cleanup(e.ctx)
	got := e.rec.matching("stop ")
	slices.Sort(got)
	want := []string{"stop " + over.ID, "stop " + holdEnded.ID, "stop " + waiting.ID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("stops %v, want %v", got, want)
	}
	if e.rec.index(fmt.Sprintf("remove-runner %d", over.RunnerID)) < 0 {
		t.Error("the runner of the overdue sandbox stays on GitHub")
	}
	if got := e.sandbox(over.ID); got.State != sandbox.StateStopping || got.ExitReason != "time limit" {
		t.Errorf("overdue sandbox: state %q, exit reason %q", got.State, got.ExitReason)
	}

	// The job on the warm sandbox reaches the limit 60 minutes after its start.
	e.clock.add(55 * time.Minute)
	e.s.cleanup(e.ctx)
	if got := e.sandbox(warm.ID); got.State != sandbox.StateStopping || got.ExitReason != "time limit" {
		t.Errorf("warm sandbox after its job limit: state %q, exit reason %q", got.State, got.ExitReason)
	}
}

// The cleanup completes a job whose sandbox is gone for lostJobAfter with
// the result that the runner reported, or else cancelled. A late
// JobCompleted still sets the real result.
func TestCleanupCompletesLostJobs(t *testing.T) {
	e := newTestEnv(t)
	serverID, _ := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane")
	_, sc, set := e.attach(pool)
	e.runningJob(pool, set, serverID, 601, start)
	e.runningJob(pool, set, serverID, 602, start.Add(7*time.Minute))
	e.runningJob(pool, set, serverID, 603, start)
	j603 := e.job(603)
	if _, _, err := e.st.LinkJobSandbox(e.ctx, j603.ID, j603.SandboxID); err != nil {
		t.Fatal(err)
	}
	e.event(serverID, j603.SandboxID, sandbox.Event{Type: sandbox.EventRunner,
		Runner: &sandbox.RunnerEvent{Kind: sandbox.RunnerJobCompleted, JobName: "e2e", Result: "Succeeded"}})
	e.clock.add(11 * time.Minute)

	e.s.cleanup(e.ctx)
	e.settle()
	if j := e.job(601); j.State != store.JobCompleted || j.Result != store.ResultCancelled || j.FinishedAt == nil || !j.FinishedAt.Equal(start) {
		t.Errorf("job with no result: state %q, result %q, finished %v; want cancelled at %v", j.State, j.Result, j.FinishedAt, start)
	}
	if j := e.job(602); j.State != store.JobRunning {
		t.Errorf("job with a sandbox gone for 4 minutes: state %q, want running", j.State)
	}
	if j := e.job(603); j.State != store.JobCompleted || j.Result != store.ResultSucceeded {
		t.Errorf("job with a runner result: state %q, result %q; want completed, succeeded", j.State, j.Result)
	}

	b := e.sandbox(e.job(601).SandboxID)
	e.scale(sc, msg(0, completed(601, "acme/api", 7000, "e2e", b, store.ResultFailed)))
	e.settle()
	if j := e.job(601); j.Result != store.ResultFailed || !j.FinishedAt.Equal(start.Add(3*time.Minute)) {
		t.Errorf("after the late JobCompleted: result %q, finished %v; want failed at the message time", j.Result, j.FinishedAt)
	}
}

// The result of a lost job is a guess. The finish step reads the job on
// GitHub, and the result from GitHub replaces the guess.
func TestFinishStepTakesTheGitHubResultOfALostJob(t *testing.T) {
	e := newTestEnv(t)
	serverID, _ := e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane")
	_, _, set := e.attach(pool)
	e.runningJob(pool, set, serverID, 611, start)
	e.gh.results[611] = store.ResultFailed
	e.clock.add(11 * time.Minute)

	e.s.cleanup(e.ctx)
	e.settle()
	j := e.job(611)
	if j.Result != store.ResultFailed || j.ResultGuessed || j.FinishedAt == nil || !j.FinishedAt.Equal(start.Add(4*time.Minute)) {
		t.Errorf("job = %s (guessed %v) finished %v; want failed from GitHub at %v", j.Result, j.ResultGuessed, j.FinishedAt, start.Add(4*time.Minute))
	}
}

// runningJob makes a running job whose sandbox went away at stopped.
func (e *testEnv) runningJob(p store.Pool, set store.ScaleSet, serverID string, requestID int64, stopped time.Time) {
	e.t.Helper()
	b := e.insertSandbox(p, set, serverID, sandbox.StateBusy, start.Add(-time.Minute))
	j, err := e.st.UpsertJob(e.ctx, store.JobMessage{
		RunnerRequestID: requestID, State: store.JobRunning, PoolID: p.ID, ScaleSetID: set.ID,
		Repository: "acme/api", JobName: "e2e", RunID: 7000, QueuedAt: start.Add(-2 * time.Minute), StartedAt: start.Add(-time.Minute),
	}, start)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := e.st.LinkJobSandbox(e.ctx, j.ID, b.ID); err != nil {
		e.t.Fatal(err)
	}
	e.advance(b.ID, sandbox.StateGone, "removed", stopped)
}

func TestRetentionDeletesOldJobsAndLogs(t *testing.T) {
	e := newTestEnv(t)
	e.setSettings(func(s *store.Settings) { s.JobRetentionDays, s.LogRetentionDays = 60, 20 })
	day := 24 * time.Hour
	job := func(requestID int64, finished time.Time) string {
		j, err := e.st.UpsertJob(e.ctx, store.JobMessage{
			RunnerRequestID: requestID, State: store.JobCompleted, Result: store.ResultSucceeded,
			Repository: "acme/api", JobName: "build", QueuedAt: finished.Add(-time.Hour), FinishedAt: finished,
		}, start)
		if err != nil {
			t.Fatal(err)
		}
		e.jobLogs.Fetch(e.ctx, j.ID, func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(fakeLog)), nil
		})
		return j.ID
	}
	old := job(701, start.Add(-70*day))    // past job_retention_days
	oldLog := job(702, start.Add(-25*day)) // past log_retention_days
	recent := job(703, start.Add(-2*day))

	e.s.retention(e.ctx)
	if _, err := e.st.GetJob(e.ctx, old); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old job: err %v, want not found", err)
	}
	if j, _ := e.st.GetJob(e.ctx, oldLog); j.LogState != store.LogExpired {
		t.Errorf("job with an old log: log state %q, want expired", j.LogState)
	}
	if files, err := os.ReadDir(e.logs); err != nil || len(files) != 1 {
		t.Errorf("logs folder has %v, %v; want only the log of the recent job", files, err)
	}
	log, err := e.jobLogs.Job(e.ctx, recent, 0, false)
	if err != nil {
		t.Fatalf("log of the recent job: %v", err)
	}
	log.Close()
}

// A job that runplane never takes, for lack of room or because GitHub gave
// it to another runner, must not wait in queued forever.
func TestCleanupClosesUnclaimedJobs(t *testing.T) {
	e := newTestEnv(t)
	client, sc, _ := e.attach(e.addPool("runplane")) // no server: no room
	e.scale(sc, msg(0, available(31, "acme/api", 900, "test")))
	if len(client.acquired) != 0 {
		t.Fatalf("acquired %v with no room", client.acquired)
	}
	states := events.Subscribe[events.JobState](t.Context(), e.bus)

	e.clock.add(59 * time.Minute)
	e.s.cleanup(e.ctx)
	if j := e.job(31); j.State != store.JobQueued {
		t.Fatalf("job %s after 59 minutes, want queued", j.State)
	}
	e.clock.add(2 * time.Minute)
	e.s.cleanup(e.ctx)
	j := e.job(31)
	if j.State != store.JobCompleted || j.Result != store.ResultCancelled || j.RejectReason != "" {
		t.Errorf("job %s %s %q after 61 minutes, want completed cancelled with no reject reason", j.State, j.Result, j.RejectReason)
	}
	published := false
	for len(states) > 0 {
		if x := <-states; x.ID == j.ID && x.State == store.JobCompleted {
			published = true
		}
	}
	if !published {
		t.Error("no job.state event for the closed job")
	}
}

// An admin can allow a rejected job after the cleanup closed it. When GitHub
// offers the job again, runplane takes it.
func TestClosedRejectedJobRunsWhenAllowed(t *testing.T) {
	e := newTestEnv(t)
	e.gh.checks["acme/site"] = JobCheck{Private: true, Fork: true}
	e.addServer("fsn1-a", capacity("x64", 16, 65536))
	client, sc, _ := e.attach(e.addPool("runplane"))
	offer := msg(0, available(41, "acme/site", 950, "test"))
	e.scale(sc, offer)
	e.clock.add(61 * time.Minute)
	e.s.cleanup(e.ctx)
	if j := e.job(41); j.State != store.JobCompleted || j.RejectReason == "" {
		t.Fatalf("job %s with reason %q after the cleanup, want completed with the fork reason", j.State, j.RejectReason)
	}

	e.setSettings(func(s *store.Settings) { s.AllowForkPRs = true })
	e.scale(sc, offer)
	if calls := client.acquired; len(calls) != 1 || !slices.Equal(calls[0], []int64{41}) {
		t.Fatalf("acquire calls %v, want [[41]]", calls)
	}
	if j := e.job(41); j.State != store.JobAssigned || j.RejectReason != "" {
		t.Errorf("job %s with reason %q after the new offer, want assigned with no reason", j.State, j.RejectReason)
	}
}

// GitHub sends JobCompleted one time, so a finish step that a restart cuts
// off must run again after the restart. Its metrics summary stays. A job
// whose JobStarted runplane did not get gets its sandbox only with
// JobCompleted, and it had no live log.
func TestFinishStepRunsAgainAfterRestart(t *testing.T) {
	for _, jobStarted := range []bool{true, false} {
		t.Run(fmt.Sprintf("JobStarted %v", jobStarted), func(t *testing.T) {
			e := newTestEnv(t)
			e.newScheduler(0, time.Hour)
			serverID, _ := e.addServer("fsn1-a", capacity("x64", 16, 65536))
			pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
			_, sc, _ := e.attach(pool)
			e.scale(sc, msg(0))
			b := e.sandboxes(pool.ID)[0]
			if jobStarted {
				e.scale(sc, msg(1, started(71, "acme/api", 970, "build", b)))
			}
			e.event(serverID, b.ID, sandbox.Event{Type: sandbox.EventMetrics, Metrics: &sandbox.Metrics{CPUPercent: 80, MemoryMB: 900}})
			e.gh.noLog = true
			stop := e.run()
			e.scale(sc, msg(0, completed(71, "acme/api", 970, "build", b, store.ResultSucceeded)))
			e.rec.await(t, "download ")
			// runplane stops while the finish step waits for the log.
			stop()
			if j := e.job(71); !j.FinishPending || j.CPUPeak == nil || *j.CPUPeak != 80 {
				t.Fatalf("after the stop: finish pending %v, cpu peak %v; want true and 80", j.FinishPending, j.CPUPeak)
			}

			e.gh.noLog = false
			e.newScheduler(0, time.Hour)
			jobs := events.Subscribe[events.JobState](t.Context(), e.bus)
			e.run()
			j := next(t, jobs, "the end of the finish step after the restart", func(j events.JobState) bool {
				return j.RunnerRequestID == 71 && !j.FinishPending
			})
			if j.LogState != store.LogStored {
				t.Errorf("after the restart: log %q, want stored", j.LogState)
			}
		})
	}
}

// The job log goes to its file in one step, so a download that breaks
// leaves no part of a log.
func TestBrokenLogDownloadLeavesNoFile(t *testing.T) {
	e := newTestEnv(t)
	e.newScheduler(0)
	e.addServer("fsn1-a", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle = 1 })
	_, sc, _ := e.attach(pool)
	e.scale(sc, msg(0))
	b := e.sandboxes(pool.ID)[0]
	e.scale(sc, msg(1, started(91, "acme/api", 990, "build", b)))
	e.gh.brokenLog = true

	e.scale(sc, msg(0, completed(91, "acme/api", 990, "build", b, store.ResultSucceeded)))
	e.settle()
	if j := e.job(91); j.LogState != store.LogNone {
		t.Errorf("log state %q after a broken download, want none", j.LogState)
	}
	if files, err := os.ReadDir(e.logs); err != nil || len(files) != 0 {
		t.Errorf("logs folder has %v, %v; want no file", files, err)
	}
}

// Warm sandboxes on a server that went away fill max_runners. After a short
// loss they still count, so that an agent update starts no extra sandboxes;
// after lostServerAfter, other servers take their place.
func TestLostServerSandboxesStopCountingAsRunners(t *testing.T) {
	e := newTestEnv(t)
	_, primary := e.addServer("primary", capacity("x64", 16, 65536))
	agentID, _ := e.addServer("agent", capacity("x64", 16, 65536))
	pool := e.addPool("runplane", func(p *store.Pool) { p.MinIdle, p.MaxRunners = 2, 2 })
	client, sc, set := e.attach(pool)
	a := e.insertSandbox(pool, set, agentID, sandbox.StateIdle, start)
	b := e.insertSandbox(pool, set, agentID, sandbox.StateIdle, start)
	client.runners[a.RunnerID] = Runner{ID: a.RunnerID, Name: a.RunnerName}
	client.runners[b.RunnerID] = Runner{ID: b.RunnerID, Name: b.RunnerName}
	if err := e.st.TouchServer(e.ctx, agentID, start); err != nil {
		t.Fatal(err)
	}
	e.fleet.mu.Lock()
	e.fleet.disconnected[agentID] = true
	e.fleet.mu.Unlock()

	e.clock.add(5 * time.Minute)
	e.s.cleanup(e.ctx)
	e.scale(sc, msg(0))
	if n := len(primary.specs); n != 0 {
		t.Fatalf("%d starts 5 minutes after the agent went away, want 0", n)
	}

	e.clock.add(11 * time.Minute)
	e.s.cleanup(e.ctx)
	e.scale(sc, msg(0))
	if n := len(primary.specs); n != 2 {
		t.Errorf("%d starts on the primary 16 minutes after the agent went away, want 2", n)
	}
	for _, x := range []store.Sandbox{a, b} {
		if got := e.sandbox(x.ID); got.State != sandbox.StateGone || got.ExitReason != "server unreachable" {
			t.Errorf("sandbox on the lost agent: state %q, reason %q; want gone, server unreachable", got.State, got.ExitReason)
		}
		if e.rec.index(fmt.Sprintf("remove-runner %d", x.RunnerID)) < 0 {
			t.Errorf("runner %s stays on GitHub", x.RunnerName)
		}
	}
}
