package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/tlsutil"
)

var (
	_ scheduler.Fleet   = (*Fleet)(nil)
	_ api.Servers       = (*Fleet)(nil)
	_ api.InstallScript = (*Fleet)(nil)
	_ api.HostChecker   = (*Fleet)(nil)
	_ api.SettingsHook  = (*Fleet)(nil)
)

// harness is a primary with a fleet, a real agent listener, a fake local
// server, and a clock that the test moves.
type harness struct {
	t        *testing.T
	st       *store.Store
	bus      *events.Bus
	f        *Fleet
	local    *fakeNode
	clock    *clock
	notes    *notes
	capacity atomic.Int32 // CapacityChanged calls
	fp       string
	cfg      config.Config
}

func newHarness(t *testing.T, local *fakeNode) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cert, fp, err := tlsutil.AgentCert(filepath.Join(dir, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Listen: ":8484", AgentListen: ln.Addr().String(), ServicePort: 8486,
		DataDir: filepath.Join(dir, "data"), ConfigDir: filepath.Join(dir, "etc"),
	}
	for _, d := range []string{cfg.DataDir, cfg.ConfigDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h := &harness{
		t: t, st: st, bus: &events.Bus{}, local: local, notes: &notes{}, fp: fp, cfg: cfg,
		clock: &clock{at: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)},
	}
	h.f = New(Config{
		Store: st, Bus: h.bus, Log: testLogger(t, "fleet"), Config: cfg, Local: local, AgentFingerprint: fp,
		CapacityChanged: func() { h.capacity.Add(1) }, Notifier: h.notes, Now: h.clock.now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	l := &agent.Listener{Cert: cert, Authenticate: h.f.Authenticate, OnSession: h.f.AddSession, Logger: testLogger(t, "listener")}
	served := make(chan struct{})
	go func() {
		l.Serve(ctx, ln)
		close(served)
	}()
	t.Cleanup(func() {
		cancel()
		<-served
	})
	return h
}

// run starts Fleet.Run and waits until the local server is connected.
func (h *harness) run() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.f.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			h.t.Errorf("Run: %v", err)
		}
	})
	waitFor(h.t, "the local server", func() bool { return h.f.Driver(h.localID()) != nil })
}

func (h *harness) localID() string {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	return h.f.localID
}

func (h *harness) server(id string) store.Server {
	h.t.Helper()
	v, err := h.st.GetServer(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

// connectAgent registers a new agent with an install token and waits for
// its session.
func (h *harness) connectAgent(name string, node *fakeNode) (*testAgent, string) {
	h.t.Helper()
	tok, err := h.f.NewToken(context.Background(), "https://ci.example.com", name, "")
	if err != nil {
		h.t.Fatal(err)
	}
	a := h.startAgent(agentFile{Primary: h.cfg.AgentListen, Fingerprint: h.fp, Token: tok.Token}, node)
	id := a.waitRegistered()
	waitFor(h.t, "the agent session", func() bool { return h.f.Driver(id) != nil })
	return a, id
}

type agentFile struct {
	Primary     string `json:"primary"`
	Fingerprint string `json:"fingerprint"`
	Token       string `json:"token,omitempty"`
	ServerID    string `json:"server_id"`
	Secret      string `json:"secret"`
}

type testAgent struct {
	t    *testing.T
	path string
	node *fakeNode
	stop func()
}

func (h *harness) startAgent(file agentFile, node *fakeNode) *testAgent {
	h.t.Helper()
	b, _ := json.Marshal(file)
	path := filepath.Join(h.t.TempDir(), "agent.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		h.t.Fatal(err)
	}
	a := &testAgent{t: h.t, path: path, node: node}
	a.start()
	return a
}

// start runs the agent with its agent.json until stop.
func (a *testAgent) start() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		agent.Run(ctx, agent.Config{Path: a.path, Node: a.node, Logger: testLogger(a.t, "agent"),
			Mirror: func() mirror.Stats { return a.node.mirror }})
		close(done)
	}()
	var once sync.Once
	a.stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	a.t.Cleanup(a.stop)
}

// waitRegistered waits until agent.json has a server ID and returns it.
func (a *testAgent) waitRegistered() string {
	a.t.Helper()
	var file agentFile
	waitFor(a.t, "the registration in agent.json", func() bool {
		b, err := os.ReadFile(a.path)
		return err == nil && json.Unmarshal(b, &file) == nil && file.ServerID != ""
	})
	return file.ServerID
}

func (a *testAgent) file() agentFile {
	var file agentFile
	b, err := os.ReadFile(a.path)
	if err == nil {
		err = json.Unmarshal(b, &file)
	}
	if err != nil {
		a.t.Fatal(err)
	}
	return file
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// watch collects the bus events of the test.
func watch(t *testing.T, bus *events.Bus) func() []events.Event {
	ch, cancel := bus.Subscribe(4096)
	t.Cleanup(cancel)
	var mu sync.Mutex
	var list []events.Event
	go func() {
		for e := range ch {
			mu.Lock()
			list = append(list, e)
			mu.Unlock()
		}
	}()
	return func() []events.Event {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(list)
	}
}

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

type notes struct {
	mu   sync.Mutex
	list []notify.Event
}

func (n *notes) Notify(_ context.Context, e notify.Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.list = append(n.list, e)
	return nil
}

func (n *notes) events() []notify.Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.list)
}

// fakeNode is a server whose images and runner versions the test chooses.
type fakeNode struct {
	driver *fakeDriver
	info   host.Info
	stats  chan host.Stats

	mu        sync.Mutex
	digests   map[string]string        // ref: digest; other refs fail
	runners   map[string]bool          // versions that download
	gates     map[string]chan struct{} // pulls of these refs wait until the channel closes
	pulled    []string                 // refs of the EnsureImage calls
	versions  []string                 // versions of the EnsureRunner calls
	settings  []sandbox.Settings
	kept      [][]string   // the sorted keep list of each GCImages call
	pruned    [][]string   // the sorted keep list of each PruneRunners call
	updates   []string     // versions of the Update calls
	updateErr error        // the result of Update
	mirror    mirror.Stats // what an agent with this node reports
}

func newFakeNode(arch string) *fakeNode {
	return &fakeNode{
		driver: &fakeDriver{events: make(chan sandbox.Event, 16), sandboxes: map[string]sandbox.Sandbox{}},
		info: host.Info{
			Hostname: "node-" + arch, OS: "Ubuntu 24.04.3 LTS", Kernel: "6.8.0-45-generic", Arch: arch,
			Resources: host.Resources{VCPU: 16, MemoryMB: 65536, DiskBytes: 500e9, FreeDiskBytes: 300e9},
			DiskKind:  "NVMe", DockerVersion: "29.1.0", SysboxVersion: "0.7.1", IPv4: "203.0.113.7", Provider: "Hetzner",
		},
		stats:   make(chan host.Stats, 1),
		digests: map[string]string{"builtin:ubuntu-24.04": "sha256:b0b0"},
		runners: map[string]bool{},
		gates:   map[string]chan struct{}{},
	}
}

func (n *fakeNode) Driver() sandbox.Driver                   { return n.driver }
func (n *fakeNode) Facts(context.Context) (host.Info, error) { return n.info, nil }
func (n *fakeNode) Update(_ context.Context, v string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.updates = append(n.updates, v)
	return n.updateErr
}
func (n *fakeNode) Stats() <-chan host.Stats { return n.stats }
func (n *fakeNode) Doctor(context.Context) ([]host.Check, error) {
	return []host.Check{{ID: "kernel", Name: "Kernel", Status: host.StatusOK, Detail: n.info.Kernel}}, nil
}

func (n *fakeNode) EnsureImage(ctx context.Context, ref string, progress func(image.Progress)) (sandbox.ImageRef, error) {
	n.mu.Lock()
	n.pulled = append(n.pulled, ref)
	gate := n.gates[ref]
	n.mu.Unlock()
	progress(image.Progress{Status: image.StatusPulling, Total: 4096})
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return sandbox.ImageRef{}, ctx.Err()
		}
	}
	n.mu.Lock()
	digest, ok := n.digests[ref]
	n.mu.Unlock()
	if !ok {
		return sandbox.ImageRef{}, fmt.Errorf("pull %s: not found", ref)
	}
	return sandbox.ImageRef{Ref: ref, Digest: digest}, nil
}

func (n *fakeNode) EnsureRunner(_ context.Context, version, arch string) (image.Bundle, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.versions = append(n.versions, version)
	if !n.runners[version] {
		return image.Bundle{}, fmt.Errorf("download runner %s: 404 Not Found", version)
	}
	return image.Bundle{Version: version, Arch: arch, SHA256: "9e2f" + version, Patched: true, Dir: "/var/lib/runplane/runners/" + version + "-" + arch}, nil
}

func (n *fakeNode) GCImages(_ context.Context, keep []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.kept = append(n.kept, slices.Sorted(slices.Values(keep)))
	return nil
}

func (n *fakeNode) PruneRunners(_ context.Context, keep []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pruned = append(n.pruned, slices.Sorted(slices.Values(keep)))
	return nil
}

func (n *fakeNode) pruneCalls() [][]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.pruned)
}

func (n *fakeNode) gcCalls() [][]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.kept)
}

func (n *fakeNode) Configure(_ context.Context, s sandbox.Settings) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.settings = append(n.settings, s)
	return nil
}

func (n *fakeNode) set(fn func(n *fakeNode)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	fn(n)
}

func (n *fakeNode) pulls() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.pulled)
}

func (n *fakeNode) runnerCalls() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.versions)
}

func (n *fakeNode) configured() []sandbox.Settings {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.settings)
}

// fakeDriver keeps sandboxes in memory.
type fakeDriver struct {
	events chan sandbox.Event

	mu        sync.Mutex
	sandboxes map[string]sandbox.Sandbox
	stopped   []string
}

func (d *fakeDriver) Mode() sandbox.Mode { return sandbox.ModeContainer }

func (d *fakeDriver) Start(_ context.Context, spec sandbox.Spec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sandboxes[spec.ID] = sandbox.Sandbox{ID: spec.ID, PoolID: spec.PoolID, State: sandbox.StateStarting, Spec: spec}
	return nil
}

func (d *fakeDriver) Stop(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.sandboxes, id)
	d.stopped = append(d.stopped, id)
	return nil
}

func (d *fakeDriver) List(context.Context) ([]sandbox.Sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	list := []sandbox.Sandbox{}
	for _, sb := range d.sandboxes {
		list = append(list, sb)
	}
	slices.SortFunc(list, func(a, b sandbox.Sandbox) int { return strings.Compare(a.ID, b.ID) })
	return list, nil
}

func (d *fakeDriver) Capacity(context.Context) (sandbox.Capacity, error) {
	return sandbox.Capacity{Mode: sandbox.ModeContainer, VCPU: 16, MemoryMB: 65536}, nil
}

func (d *fakeDriver) Events() <-chan sandbox.Event { return d.events }

func (d *fakeDriver) Logs(context.Context, string, bool) (io.ReadCloser, error) {
	return nil, errors.New("no logs in tests")
}

func (d *fakeDriver) Shell(context.Context, string, sandbox.TermSize) (sandbox.Terminal, error) {
	return nil, errors.New("no shell in tests")
}

func (d *fakeDriver) Hold(context.Context, string, time.Time) error { return nil }
func (d *fakeDriver) Close() error                                  { return nil }

func (d *fakeDriver) stoppedIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Sorted(slices.Values(d.stopped))
}

// testLogger keeps the log of a test and prints it when the test fails.
// Goroutines can still log after the test ends, so it does not use t.Log.
func testLogger(t *testing.T, side string) *slog.Logger {
	buf := &logBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s log:\n%s", side, buf.String())
		}
	})
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
