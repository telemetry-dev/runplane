package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// recorder keeps the calls of all fakes in one order.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

// matching returns the calls that start with prefix.
func (r *recorder) matching(prefix string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(r.calls), func(c string) bool { return !strings.HasPrefix(c, prefix) })
}

func (r *recorder) index(call string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Index(r.calls, call)
}

// fakeDriver counts a sandbox in its capacity from the moment Start
// returns, as the real drivers do. The scheduler calls no method of the nil
// sandbox.Driver.
type fakeDriver struct {
	sandbox.Driver
	rec  *recorder
	base sandbox.Capacity

	mu       sync.Mutex
	running  map[string]sandbox.Spec
	specs    []sandbox.Spec
	holds    map[string]time.Time
	startErr error
	// gate, if set, makes Start wait until it is closed. entered gets a
	// value when Start begins to wait.
	gate    chan struct{}
	entered chan string
}

func (d *fakeDriver) Mode() sandbox.Mode { return d.base.Mode }

func (d *fakeDriver) Start(ctx context.Context, spec sandbox.Spec) error {
	d.rec.add("start %s", spec.ID)
	d.mu.Lock()
	d.specs = append(d.specs, spec)
	gate, entered, startErr := d.gate, d.entered, d.startErr
	d.mu.Unlock()
	if gate != nil {
		entered <- spec.ID
		<-gate
	}
	if startErr != nil {
		return startErr
	}
	d.mu.Lock()
	d.running[spec.ID] = spec
	d.mu.Unlock()
	return nil
}

func (d *fakeDriver) Stop(ctx context.Context, id string) error {
	d.rec.add("stop %s", id)
	d.mu.Lock()
	delete(d.running, id)
	d.mu.Unlock()
	return nil
}

func (d *fakeDriver) List(ctx context.Context) ([]sandbox.Sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var list []sandbox.Sandbox
	for id, spec := range d.running {
		list = append(list, sandbox.Sandbox{ID: id, PoolID: spec.PoolID, State: sandbox.StateIdle, Spec: spec})
	}
	return list, nil
}

func (d *fakeDriver) Capacity(ctx context.Context) (sandbox.Capacity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.base
	for _, spec := range d.running {
		c.AllocVCPU += spec.VCPU
		c.AllocMemoryMB += spec.MemoryMB
		c.Sandboxes++
	}
	return c, nil
}

func (d *fakeDriver) Hold(ctx context.Context, id string, until time.Time) error {
	d.rec.add("hold %s", id)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.holds[id] = until
	return nil
}

type fakeFleet struct {
	mu       sync.Mutex
	hosts    []Host
	noImage  map[string]bool // server IDs without the image
	offline  map[string]bool // server IDs that are not connected
	imageRef sandbox.ImageRef
	runner   string // the runner version for new sandboxes
}

func (f *fakeFleet) Online() []Host {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(f.hosts), func(h Host) bool { return f.offline[h.ServerID] })
}

func (f *fakeFleet) Driver(serverID string) sandbox.Driver {
	for _, h := range f.Online() {
		if h.ServerID == serverID {
			return h.Driver
		}
	}
	return nil
}

func (f *fakeFleet) Image(serverID, imageID string) (sandbox.ImageRef, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imageRef, !f.noImage[serverID]
}

func (f *fakeFleet) RunnerVersion(serverID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runner, true
}

type fakeClient struct {
	rec *recorder

	mu       sync.Mutex
	nextID   int64
	runners  map[int64]Runner
	busy     map[int64]bool // runners that GitHub calls busy
	acquired [][]int64      // the IDs of each AcquireJobs call
	max      int
	// jitGate, if set, makes GenerateJITConfig wait until it is closed.
	// jitEntered gets a value when the call begins to wait.
	jitGate    chan struct{}
	jitEntered chan struct{}
}

func (c *fakeClient) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acquired = append(c.acquired, slices.Clone(requestIDs))
	return requestIDs, nil
}

func (c *fakeClient) GenerateJITConfig(ctx context.Context, runnerName string) (JITConfig, error) {
	if c.jitGate != nil {
		c.jitEntered <- struct{}{}
		<-c.jitGate
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	c.runners[c.nextID] = Runner{ID: c.nextID, Name: runnerName}
	c.rec.add("jit %s", runnerName)
	return JITConfig{Encoded: "eyJqaXQiOiI" + runnerName, RunnerID: c.nextID, RunnerName: runnerName}, nil
}

func (c *fakeClient) RemoveRunner(ctx context.Context, runnerID int64) error {
	c.rec.add("remove-runner %d", runnerID)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy[runnerID] {
		return ErrRunnerBusy
	}
	delete(c.runners, runnerID)
	return nil
}

func (c *fakeClient) ListRunners(ctx context.Context) ([]Runner, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var list []Runner
	for _, r := range c.runners {
		r.Busy = c.busy[r.ID]
		list = append(list, r)
	}
	return list, nil
}

func (c *fakeClient) SetMaxRunners(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.max = n
}

type fakeGitHub struct {
	rec *recorder

	mu         sync.Mutex
	checks     map[string]JobCheck // by repository; absent: private, no fork
	checkErrs  map[string]error    // by repository
	checkCalls int
	noLog      bool // DownloadLog fails, as for a log that GitHub has not ready yet
	steps      []store.JobStep
}

func (g *fakeGitHub) CheckJob(ctx context.Context, repository string, runID int64) (JobCheck, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checkCalls++
	if err := g.checkErrs[repository]; err != nil {
		return JobCheck{}, err
	}
	c, ok := g.checks[repository]
	if !ok {
		c = JobCheck{Private: true}
	}
	return c, nil
}

func (g *fakeGitHub) Enrich(ctx context.Context, job store.Job) error {
	g.rec.add("enrich %s", job.ID)
	return nil
}

func (g *fakeGitHub) Steps(ctx context.Context, job store.Job) ([]store.JobStep, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rec.add("steps %s", job.ID)
	return g.steps, nil
}

func (g *fakeGitHub) DownloadLog(ctx context.Context, job store.Job, path string) error {
	g.rec.add("download %s", job.ID)
	g.mu.Lock()
	noLog := g.noLog
	g.mu.Unlock()
	if noLog {
		return errors.New("GitHub API GET /repos/acme/api/actions/jobs/1/logs: 404 Not Found")
	}
	return os.WriteFile(path, []byte("2026-11-04T12:00:00.0000000Z ##[group]Run actions/checkout@v5\n"), 0o600)
}

func (g *fakeGitHub) Rerun(ctx context.Context, job store.Job) error {
	g.rec.add("rerun %s", job.ID)
	return nil
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type testEnv struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	bus   *events.Bus
	rec   *recorder
	fleet *fakeFleet
	gh    *fakeGitHub
	clock *fakeClock
	logs  string
	s     *Scheduler

	runnerIDs int64
}

var start = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &testEnv{
		t:     t,
		ctx:   context.Background(),
		st:    st,
		bus:   &events.Bus{},
		rec:   &recorder{},
		clock: &fakeClock{t: start},
		logs:  t.TempDir(),
	}
	e.fleet = &fakeFleet{
		noImage: map[string]bool{}, offline: map[string]bool{},
		imageRef: sandbox.ImageRef{Ref: "ghcr.io/runplane/ubuntu-24.04:latest", Digest: "sha256:5f1c0e7c3d0b4a8e9f6a2b1c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e"},
		runner:   "2.330.0",
	}
	e.gh = &fakeGitHub{rec: e.rec, checks: map[string]JobCheck{}}
	e.setSettings(func(s *store.Settings) { s.Mode, s.ReserveVCPU, s.ReserveMemoryMB = sandbox.ModeContainer, 0, 0 })
	e.s = New(Config{Store: st, Bus: e.bus, Fleet: e.fleet, GitHub: e.gh, LogsDir: e.logs, Now: e.clock.now})
	t.Cleanup(e.s.stopBg)
	return e
}

func (e *testEnv) setSettings(fn func(*store.Settings)) {
	e.t.Helper()
	if _, err := e.st.UpdateSettings(e.ctx, func(s *store.Settings) error { fn(s); return nil }); err != nil {
		e.t.Fatal(err)
	}
}

// capacity is a server. The settings of the test have no host reserve, so
// that sizes add up simply.
func capacity(arch string, vcpu, memoryMB int) sandbox.Capacity {
	return sandbox.Capacity{
		Mode: sandbox.ModeContainer, Arch: arch, VCPU: vcpu, MemoryMB: memoryMB,
		DiskBytes: 500 << 30, FreeDiskBytes: 300 << 30,
	}
}

func (e *testEnv) addServer(name string, c sandbox.Capacity) (string, *fakeDriver) {
	e.t.Helper()
	id := ids.New()
	err := e.st.CreateServer(e.ctx, store.Server{
		ID: id, Name: name, Kind: store.ServerAgent, Status: store.ServerOnline,
		ServerFacts: store.ServerFacts{Arch: c.Arch, Mode: c.Mode, VCPU: c.VCPU, MemoryMB: c.MemoryMB, DiskBytes: c.DiskBytes},
		CreatedAt:   start, UpdatedAt: start,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	d := &fakeDriver{rec: e.rec, base: c, running: map[string]sandbox.Spec{}, holds: map[string]time.Time{}}
	e.fleet.mu.Lock()
	e.fleet.hosts = append(e.fleet.hosts, Host{ServerID: id, Name: name, Arch: c.Arch, Driver: d})
	e.fleet.mu.Unlock()
	return id, d
}

func (e *testEnv) addPool(label string, opts ...func(*store.Pool)) store.Pool {
	e.t.Helper()
	p := store.Pool{
		ID: ids.New(), Name: label, Label: label, Arch: "x64", VCPU: 2, MemoryMB: 4096, DiskGB: 64,
		MaxRunners: 5, Enabled: true, CreatedAt: start, UpdatedAt: start,
	}
	for _, o := range opts {
		o(&p)
	}
	if err := e.st.CreatePool(e.ctx, p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *testEnv) attach(p store.Pool) (*fakeClient, listener.Scaler, store.ScaleSet) {
	e.t.Helper()
	set := store.ScaleSet{
		ID: ids.New(), PoolID: p.ID, InstallationID: 41029384, GitHubID: 7, RunnerGroupID: 1,
		Name: p.Label, Status: store.ScaleSetListening, UpdatedAt: start,
	}
	if err := e.st.UpsertScaleSet(e.ctx, set); err != nil {
		e.t.Fatal(err)
	}
	c := &fakeClient{rec: e.rec, runners: map[int64]Runner{}, busy: map[int64]bool{}}
	return c, e.s.Attach(set, c), set
}

func (e *testEnv) scale(sc listener.Scaler, msg *scaleset.RunnerScaleSetMessage) {
	e.t.Helper()
	if err := sc.Scale(e.ctx, msg); err != nil {
		e.t.Fatal(err)
	}
}

// settle waits for the background steps, such as the job finish step.
func (e *testEnv) settle() { e.s.wg.Wait() }

func (e *testEnv) sandboxes(poolID string, states ...sandbox.State) []store.Sandbox {
	e.t.Helper()
	list, err := e.st.ListSandboxes(e.ctx, store.SandboxFilter{PoolID: poolID, States: states})
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

func (e *testEnv) sandbox(id string) store.Sandbox {
	e.t.Helper()
	b, err := e.st.GetSandbox(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *testEnv) job(requestID int64) store.Job {
	e.t.Helper()
	j, err := e.st.GetJobByRunnerRequestID(e.ctx, requestID)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *testEnv) event(serverID, sandboxID string, ev sandbox.Event) {
	ev.SandboxID = sandboxID
	ev.Time = e.clock.now()
	e.s.HandleEvent(serverID, ev)
}

func runnerEvent(kind string) sandbox.Event {
	return sandbox.Event{Type: sandbox.EventRunner, Runner: &sandbox.RunnerEvent{Kind: kind}}
}

func stateEvent(state sandbox.State, reason string) sandbox.Event {
	return sandbox.Event{Type: sandbox.EventState, State: state, Reason: reason}
}

// insertSandbox writes a sandbox row as a start would.
func (e *testEnv) insertSandbox(p store.Pool, set store.ScaleSet, serverID string, state sandbox.State, created time.Time) store.Sandbox {
	e.t.Helper()
	id := ids.New()
	e.runnerIDs++
	b := store.Sandbox{
		ID: id, PoolID: p.ID, ServerID: serverID, ScaleSetID: set.ID, Mode: sandbox.ModeContainer, State: state,
		RunnerID: 1000 + e.runnerIDs, RunnerName: p.Label + "-" + id[len(id)-8:], VCPU: p.VCPU, MemoryMB: p.MemoryMB,
		DiskGB: p.DiskGB, ImageDigest: e.fleet.imageRef.Digest, RunnerVersion: e.fleet.runner, CreatedAt: created,
	}
	if err := e.st.InsertSandbox(e.ctx, b); err != nil {
		e.t.Fatal(err)
	}
	return b
}

// msg makes a listener message with the job messages, and with
// assignedJobs assigned and running jobs in its statistics.
func msg(assignedJobs int, jobs ...any) *scaleset.RunnerScaleSetMessage {
	m := &scaleset.RunnerScaleSetMessage{
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assignedJobs, TotalRunningJobs: assignedJobs},
	}
	for _, j := range jobs {
		switch j := j.(type) {
		case *scaleset.JobAvailable:
			m.JobAvailableMessages = append(m.JobAvailableMessages, j)
		case *scaleset.JobAssigned:
			m.JobAssignedMessages = append(m.JobAssignedMessages, j)
		case *scaleset.JobStarted:
			m.JobStartedMessages = append(m.JobStartedMessages, j)
		case *scaleset.JobCompleted:
			m.JobCompletedMessages = append(m.JobCompletedMessages, j)
		default:
			panic(fmt.Sprintf("msg: %T is no job message", j))
		}
	}
	return m
}

func base(requestID int64, repo string, runID int64, jobName string) scaleset.JobMessageBase {
	owner, name, _ := strings.Cut(repo, "/")
	return scaleset.JobMessageBase{
		RunnerRequestID: requestID,
		RepositoryName:  name,
		OwnerName:       owner,
		JobID:           fmt.Sprintf("b6a1c2d3-%04d-4e5f-8a9b-0c1d2e3f4a5b", requestID%10000),
		JobWorkflowRef:  repo + "/.github/workflows/ci.yml@refs/heads/main",
		JobDisplayName:  jobName,
		WorkflowRunID:   runID,
		EventName:       "push",
		RequestLabels:   []string{"runplane"},
		QueueTime:       start,
	}
}

func available(requestID int64, repo string, runID int64, jobName string) *scaleset.JobAvailable {
	return &scaleset.JobAvailable{JobMessageBase: base(requestID, repo, runID, jobName)}
}

func assigned(requestID int64, repo string, runID int64, jobName string) *scaleset.JobAssigned {
	b := base(requestID, repo, runID, jobName)
	b.ScaleSetAssignTime = start.Add(time.Second)
	return &scaleset.JobAssigned{JobMessageBase: b}
}

func started(requestID int64, repo string, runID int64, jobName string, runner store.Sandbox) *scaleset.JobStarted {
	b := base(requestID, repo, runID, jobName)
	b.ScaleSetAssignTime = start.Add(time.Second)
	b.RunnerAssignTime = start.Add(5 * time.Second)
	return &scaleset.JobStarted{RunnerID: int(runner.RunnerID), RunnerName: runner.RunnerName, JobMessageBase: b}
}

func completed(requestID int64, repo string, runID int64, jobName string, runner store.Sandbox, result string) *scaleset.JobCompleted {
	s := started(requestID, repo, runID, jobName, runner)
	s.FinishTime = start.Add(3 * time.Minute)
	return &scaleset.JobCompleted{Result: result, RunnerID: s.RunnerID, RunnerName: s.RunnerName, JobMessageBase: s.JobMessageBase}
}
