package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/sandbox"
)

// note is the backend part of a test sandbox. Note goes in the record.
type note struct {
	Note string `json:"note"`
}

// fakeBackend operates sandboxes that exist only in memory. A sandbox ends
// when the test or a shutdown calls MarkExited.
type fakeBackend struct {
	mu        sync.Mutex
	calls     []string
	create    func(b *Box[note]) error // the work of Create; nil does nothing
	alive     map[string]bool          // the sandboxes that Adopt finds
	usage     map[string]Usage
	fault     string
	noGuest   bool // Shutdown fails, as for a guest that does not answer
	exitOnEnd bool // Shutdown and Kill end the sandbox
}

func (f *fakeBackend) record(call string, b *Box[note]) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call+" "+b.ID)
}

func (f *fakeBackend) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeBackend) Create(_ context.Context, b *Box[note], spec sandbox.Spec, _ sandbox.Settings) error {
	f.record("create", b)
	if spec.JITConfig == "" {
		return errors.New("no JIT config")
	}
	if f.create != nil {
		return f.create(b)
	}
	return nil
}

func (f *fakeBackend) Adopt(_ context.Context, b *Box[note]) (bool, error) {
	f.record("adopt", b)
	return f.alive[b.ID], nil
}

func (f *fakeBackend) Shutdown(_ context.Context, b *Box[note]) error {
	f.record("shutdown", b)
	if f.noGuest {
		return errors.New("no guest")
	}
	if f.exitOnEnd {
		b.MarkExited()
	}
	return nil
}

func (f *fakeBackend) Kill(_ context.Context, b *Box[note]) error {
	f.record("kill", b)
	if f.exitOnEnd {
		b.MarkExited()
	}
	return nil
}

func (f *fakeBackend) Fault(context.Context, *Box[note]) string { return f.fault }

func (f *fakeBackend) Remove(_ context.Context, b *Box[note]) error {
	f.record("remove", b)
	return nil
}

func (f *fakeBackend) Usage(_ context.Context, b *Box[note]) (Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.usage[b.ID]
	if !ok {
		return Usage{}, errors.New("no usage")
	}
	return u, nil
}

func newCore(t *testing.T, dir string, f *fakeBackend, limits sandbox.Limits) *Core[note] {
	t.Helper()
	c := New(Options[note]{
		Mode: sandbox.ModeVM, DataDir: dir, Dir: filepath.Join(dir, "sandboxes"), Limits: limits, Backend: f,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	t.Cleanup(func() { c.Close() })
	return c
}

func testSpec(id string) sandbox.Spec {
	return sandbox.Spec{
		ID: id, PoolID: "pool1", Image: sandbox.ImageRef{Ref: "runner:1", Digest: "sha256:5f1c"}, Arch: host.Arch(),
		VCPU: 2, MemoryMB: 512, DiskGB: 1, TimeLimit: time.Hour, JITConfig: "jit",
	}
}

// writeRecord leaves the record of a sandbox as a driver before a restart.
func writeRecord(t *testing.T, dir string, rec record[note]) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sandboxes", rec.Spec.ID, recordFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func next(t *testing.T, c *Core[note]) sandbox.Event {
	t.Helper()
	select {
	case ev := <-c.Events():
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return sandbox.Event{}
	}
}

func quiet(t *testing.T, c *Core[note]) {
	t.Helper()
	select {
	case ev := <-c.Events():
		t.Errorf("unexpected event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func describe(ev sandbox.Event) string {
	if ev.Type == sandbox.EventRunner {
		return "runner " + ev.Runner.Kind
	}
	return fmt.Sprintf("%s %s %s", ev.Type, ev.State, ev.Reason)
}

// The events that the guest sends during Start come after starting. A state
// moves only forward, so a late listening event does not undo busy.
func TestEventsOfASandbox(t *testing.T) {
	f := &fakeBackend{create: func(b *Box[note]) error {
		b.OnConnect()
		b.OnEvent(sandbox.RunnerEvent{Kind: sandbox.RunnerListening})
		return nil
	}}
	c := newCore(t, t.TempDir(), f, sandbox.Limits{})
	if err := c.Start(context.Background(), testSpec("sb1")); err != nil {
		t.Fatal(err)
	}
	b := c.Box("sb1")
	b.OnEvent(sandbox.RunnerEvent{Kind: sandbox.RunnerJobStarted})
	b.OnEvent(sandbox.RunnerEvent{Kind: sandbox.RunnerListening})
	b.OnEvent(sandbox.RunnerEvent{Kind: sandbox.RunnerExited, ExitCode: 3})
	var got []string
	for range 8 {
		got = append(got, describe(next(t, c)))
	}
	want := []string{
		"state starting ", "runner listening", "state idle ",
		"runner job.started", "state busy ", "runner listening",
		"runner exited", "state stopping the runner exited with code 3",
	}
	if !slices.Equal(got, want) {
		t.Errorf("events\n%q\nwant\n%q", got, want)
	}
	quiet(t, c)
}

// A failed Start leaves no sandbox and sends no events, also not the events
// of the guest during the start.
func TestFailedStartSendsNoEvents(t *testing.T) {
	dir := t.TempDir()
	f := &fakeBackend{create: func(b *Box[note]) error {
		b.OnEvent(sandbox.RunnerEvent{Kind: sandbox.RunnerListening})
		b.Console("Kernel panic")
		return errors.New("boot failed")
	}}
	c := newCore(t, dir, f, sandbox.Limits{})
	err := c.Start(context.Background(), testSpec("sb1"))
	if err == nil || err.Error() != "start sandbox sb1: boot failed: Kernel panic" {
		t.Fatalf("Start: %v, want the error with the end of the console", err)
	}
	if list, _ := c.List(context.Background()); len(list) != 0 {
		t.Errorf("listed %+v after a failed start", list)
	}
	if _, err := os.Stat(filepath.Join(dir, "sandboxes", "sb1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the sandbox folder remains: %v", err)
	}
	if got := f.called(); !slices.Equal(got, []string{"create sb1", "remove sb1"}) {
		t.Errorf("backend calls %v, want create and remove", got)
	}
	quiet(t, c)
}

func TestStartChecksTheSpec(t *testing.T) {
	f := &fakeBackend{}
	c := newCore(t, t.TempDir(), f, sandbox.Limits{MaxVCPU: 4})
	for name, change := range map[string]func(*sandbox.Spec){
		"an ID that is not a folder name": func(s *sandbox.Spec) { s.ID = "../x" },
		"another arch":                    func(s *sandbox.Spec) { s.Arch = "riscv64" },
		"no image digest":                 func(s *sandbox.Spec) { s.Image.Digest = "" },
		"more vCPUs than the driver has":  func(s *sandbox.Spec) { s.VCPU = 5 },
		"no vCPU":                         func(s *sandbox.Spec) { s.VCPU = 0 },
		"too little memory":               func(s *sandbox.Spec) { s.MemoryMB = 64 },
		"no disk":                         func(s *sandbox.Spec) { s.DiskGB = 0 },
	} {
		spec := testSpec("sb1")
		change(&spec)
		if err := c.Start(context.Background(), spec); err == nil {
			t.Errorf("%s: Start took the spec", name)
		}
	}
	if got := f.called(); len(got) != 0 {
		t.Errorf("backend calls %v, want none", got)
	}
	spec := testSpec("sb2")
	spec.Arch = ""
	if err := c.Start(context.Background(), spec); err != nil {
		t.Errorf("an empty arch, which is the arch of the server: %v", err)
	}
}

// Stop asks the guest first and kills the sandbox only after the grace. A
// Stop whose ctx ends returns, and the stop goes on.
func TestStop(t *testing.T) {
	f := &fakeBackend{}
	c := newCore(t, t.TempDir(), f, sandbox.Limits{})
	if err := c.Start(context.Background(), testSpec("sb1")); err != nil {
		t.Fatal(err)
	}
	next(t, c) // starting
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.Stop(ctx, "sb1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop with an ended ctx: %v, want its error", err)
	}
	if ev := next(t, c); ev.State != sandbox.StateStopping || ev.Reason != "stopped by runplane" {
		t.Fatalf("got %+v, want stopping", ev)
	}
	c.Box("sb1").MarkExited() // the guest powers off
	if ev := next(t, c); ev.State != sandbox.StateGone || ev.Reason != "stopped by runplane" {
		t.Errorf("got %+v, want gone", ev)
	}
	if got := f.called(); !slices.Equal(got, []string{"create sb1", "shutdown sb1", "remove sb1"}) {
		t.Errorf("backend calls %v, want a shutdown and no kill", got)
	}
	if err := c.Stop(context.Background(), "sb1"); err != nil {
		t.Errorf("Stop of a stopped sandbox: %v", err)
	}
}

func TestStopKillsASandboxWithoutGuest(t *testing.T) {
	f := &fakeBackend{noGuest: true, exitOnEnd: true}
	c := newCore(t, t.TempDir(), f, sandbox.Limits{})
	if err := c.Start(context.Background(), testSpec("sb1")); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(context.Background(), "sb1"); err != nil {
		t.Fatal(err)
	}
	if got := f.called(); !slices.Equal(got, []string{"create sb1", "shutdown sb1", "kill sb1", "remove sb1"}) {
		t.Errorf("backend calls %v, want a kill after the failed shutdown", got)
	}
}

// The end state comes from what runplane found, what the runner reported,
// and what the backend found, in this order.
func TestOutcome(t *testing.T) {
	code := 0
	tests := []struct {
		name   string
		box    *Box[note]
		fault  string
		state  sandbox.State
		reason string
	}{
		{"a fault of runplane", &Box[note]{failure: "the sandbox passed its time limit", stopReason: "stopped by runplane", runnerExit: &code}, "", sandbox.StateFailed, "the sandbox passed its time limit"},
		{"a runner error", &Box[note]{runnerErr: "the runner did not connect to GitHub", stopReason: "stopped by runplane", runnerExit: &code}, "", sandbox.StateFailed, "the runner did not connect to GitHub"},
		{"a stop", &Box[note]{stopReason: "stopped by runplane", heard: true}, "out of memory", sandbox.StateGone, "stopped by runplane"},
		{"a fault of the backend", &Box[note]{heard: true, runnerExit: &code}, "out of memory", sandbox.StateFailed, "out of memory"},
		{"no guest", &Box[note]{}, "", sandbox.StateFailed, "the sandbox stopped before runplane-init connected: Kernel panic"},
		{"no runner exit", &Box[note]{heard: true}, "", sandbox.StateFailed, "the sandbox stopped unexpectedly: Kernel panic"},
		{"the runner ended", &Box[note]{heard: true, runnerExit: &code}, "", sandbox.StateGone, "the runner exited with code 0"},
	}
	for _, tt := range tests {
		b := tt.box
		b.lastLines = []string{"Kernel panic"}
		state, reason := b.outcome(tt.fault)
		if state != tt.state || reason != tt.reason {
			t.Errorf("%s: %s %q, want %s %q", tt.name, state, reason, tt.state, tt.reason)
		}
	}
}

func TestDeadline(t *testing.T) {
	created := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		limit      time.Duration
		jobStarted time.Time
		hold       time.Time
		want       time.Time
	}{
		// Without a job: the limit, the idle grace of 5 minutes, and the grace for init.
		{"a warm sandbox", time.Hour, time.Time{}, time.Time{}, created.Add(65*time.Minute + limitGrace)},
		// A sandbox that waited 3 hours for its job gets the full hour for it.
		{"a job after a long wait", time.Hour, created.Add(3 * time.Hour), time.Time{}, created.Add(4*time.Hour + limitGrace)},
		{"a debug hold past the limit", time.Hour, created.Add(10 * time.Minute), created.Add(2 * time.Hour), created.Add(2*time.Hour + limitGrace)},
		{"a hold inside the limit", time.Hour, created.Add(10 * time.Minute), created.Add(20 * time.Minute), created.Add(70*time.Minute + limitGrace)},
		{"no time limit", 0, created.Add(10 * time.Minute), time.Time{}, time.Time{}},
	}
	for _, tt := range tests {
		b := &Box[note]{created: created, spec: sandbox.Spec{TimeLimit: tt.limit}, jobStarted: tt.jobStarted, holdUntil: tt.hold}
		if got := b.deadline(); !got.Equal(tt.want) {
			t.Errorf("%s: deadline %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A sandbox past its deadline fails: runplane-init did not end it, and the
// scheduler then removes its runner from GitHub.
func TestTimeLimitFailsTheSandbox(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, record[note]{
		Mode: sandbox.ModeVM, Spec: testSpec("sb1"), CreatedAt: time.Now().Add(-3 * time.Hour), State: sandbox.StateIdle,
	})
	f := &fakeBackend{alive: map[string]bool{"sb1": true}, exitOnEnd: true}
	c := newCore(t, dir, f, sandbox.Limits{})
	if _, err := c.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, c); ev.State != sandbox.StateStopping || ev.Reason != "the sandbox passed its time limit" {
		t.Fatalf("got %+v, want stopping at the time limit", ev)
	}
	if ev := next(t, c); ev.State != sandbox.StateFailed || ev.Reason != "the sandbox passed its time limit" {
		t.Errorf("got %+v, want failed at the time limit", ev)
	}
}

// A restart keeps the record: the part of the backend, the job start from
// which the time limit counts, and a stop that runs. A sandbox that stops
// because its runner exited, as in a debug hold, is not stopped by runplane.
func TestRecover(t *testing.T) {
	dir := t.TempDir()
	created, jobStarted := time.Now().UTC().Add(-3*time.Hour), time.Now().UTC().Add(-10*time.Minute)
	code := 0
	for _, rec := range []record[note]{
		{Spec: testSpec("job"), CreatedAt: created, State: sandbox.StateBusy, JobStarted: jobStarted, Host: &note{"lease 3"}},
		{Spec: testSpec("held"), CreatedAt: created, State: sandbox.StateStopping, JobStarted: jobStarted, RunnerExit: &code},
		{Spec: testSpec("stop"), CreatedAt: created, State: sandbox.StateStopping, JobStarted: jobStarted, StopReason: "stopped by runplane"},
		{Spec: testSpec("dead"), CreatedAt: created, State: sandbox.StateIdle},
		{Spec: testSpec("other"), CreatedAt: created, State: sandbox.StateIdle},
	} {
		rec.Mode = sandbox.ModeVM
		if rec.Spec.ID == "other" {
			rec.Mode = sandbox.ModeContainer
		}
		writeRecord(t, dir, rec)
	}
	f := &fakeBackend{alive: map[string]bool{"job": true, "held": true, "stop": true}, exitOnEnd: true}
	c := newCore(t, dir, f, sandbox.Limits{})
	taken, err := c.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"job": true, "held": true, "stop": true}; len(taken) != len(want) || !taken["job"] || !taken["held"] || !taken["stop"] {
		t.Errorf("took over %v, want %v", taken, want)
	}
	if ev := next(t, c); ev.SandboxID != "stop" || ev.State != sandbox.StateGone || ev.Reason != "stopped by runplane" {
		t.Errorf("got %+v, want the stop to go on", ev)
	}
	quiet(t, c)
	if got := c.Box("job").Host.Note; got != "lease 3" {
		t.Errorf("the backend part after the restart: %q, want lease 3", got)
	}
	if got, want := c.Box("job").deadline(), jobStarted.Add(time.Hour+limitGrace); !got.Equal(want) {
		t.Errorf("deadline after the restart %v, want %v from the job start", got, want)
	}
	for id, want := range map[string]bool{"job": true, "held": true, "dead": false, "other": true} {
		if _, err := os.Stat(filepath.Join(dir, "sandboxes", id)); (err == nil) != want {
			t.Errorf("the folder of %s exists: %v, want %v", id, err == nil, want)
		}
	}
	if slices.Contains(f.called(), "adopt other") {
		t.Error("the sandbox of another mode was adopted")
	}
}

// The metrics of a busy sandbox are its CPU use between two readings, of its
// vCPUs, and its memory without the file cache. An idle sandbox has none.
func TestMetricsOfBusySandboxes(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	f := &fakeBackend{usage: map[string]Usage{}}
	c := newCore(t, t.TempDir(), f, sandbox.Limits{})
	for _, id := range []string{"busy", "idle"} {
		if err := c.Start(context.Background(), testSpec(id)); err != nil {
			t.Fatal(err)
		}
		next(t, c) // starting
	}
	c.Box("busy").OnEvent(sandbox.RunnerEvent{Kind: sandbox.RunnerJobStarted})
	next(t, c)
	next(t, c)
	read := func(at time.Duration, cpu time.Duration, memory, cache int64) {
		f.mu.Lock()
		for _, id := range []string{"busy", "idle"} {
			f.usage[id] = Usage{At: t0.Add(at), CPU: cpu, Memory: memory, Cache: cache}
		}
		f.mu.Unlock()
		c.sample(context.Background())
	}
	read(0, time.Second, 512<<20, 128<<20) // the first reading has no CPU percent yet
	quiet(t, c)
	// 5 s of CPU time in 5 s on 2 vCPUs; 512 MiB minus 128 MiB file cache.
	read(5*time.Second, 6*time.Second, 512<<20, 128<<20)
	if ev := next(t, c); ev.SandboxID != "busy" || ev.Type != sandbox.EventMetrics || *ev.Metrics != (sandbox.Metrics{CPUPercent: 50, MemoryMB: 384}) {
		t.Fatalf("got %+v %+v, want 50%% and 384 MB of the busy sandbox", ev, ev.Metrics)
	}
	// The process can use a little CPU time above the vCPUs. A cache above
	// the use leaves the use, as docker stats does.
	read(10*time.Second, 17*time.Second, 300<<20, 400<<20)
	if ev := next(t, c); *ev.Metrics != (sandbox.Metrics{CPUPercent: 100, MemoryMB: 300}) {
		t.Errorf("got %+v, want 100%% and 300 MB", ev.Metrics)
	}
	quiet(t, c)
}

func TestCapacityCountsTheOverhead(t *testing.T) {
	c := newCore(t, t.TempDir(), &fakeBackend{}, sandbox.Limits{OverheadMB: 64, MaxVCPU: 32, MaxSandboxes: 100})
	for _, id := range []string{"sb1", "sb2"} {
		if err := c.Start(context.Background(), testSpec(id)); err != nil {
			t.Fatal(err)
		}
	}
	cp, err := c.Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cp.AllocVCPU != 4 || cp.AllocMemoryMB != 2*(512+64) || cp.Sandboxes != 2 || cp.OverheadMB != 64 || cp.MaxVCPU != 32 || cp.MaxSandboxes != 100 {
		t.Errorf("capacity %+v, want 4 vCPUs, 1152 MB, 2 sandboxes, and the limits", cp)
	}
}
