package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
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
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/runner"
	"github.com/runplane/runplane/internal/runner/versions"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/tlsutil"
	"github.com/runplane/runplane/internal/version"
)

// harness is a primary with a fleet, a real agent listener, a fake local
// server, and a clock that the test moves.
type harness struct {
	t        *testing.T
	st       *store.Store
	bus      *events.Bus
	f        *Fleet
	clock    *clock
	capacity atomic.Int32 // CapacityChanged calls
	fp       string
	cfg      config.Config
	version  string // of runplane on the primary and on the agents that start
	log      *slog.Logger
}

// newHarness makes the harness. The primary runs the version "dev", and
// opts can change the fleet config.
func newHarness(t *testing.T, local *fakeNode, opts ...func(*Config)) *harness {
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
	h := &harness{
		t: t, st: st, bus: &events.Bus{}, fp: fp, log: testLogger(t),
		cfg: config.Config{AgentListen: ln.Addr().String(), DataDir: dir, ConfigDir: dir}, clock: &clock{},
	}
	h.clock.ns.Store(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC).UnixNano())
	c := Config{
		Store: st, Bus: h.bus, Log: h.log.With("side", "fleet"), Config: h.cfg, Local: local, AgentFingerprint: fp,
		Version: "dev", Repo: version.Repo,
		OnEvent: func(string, sandbox.Event) {}, CapacityChanged: func() { h.capacity.Add(1) }, Now: h.clock.now,
		Runners: versions.New(versions.Config{Store: st, Bus: h.bus}),
	}
	for _, o := range opts {
		o(&c)
	}
	h.version = c.Version
	h.f = New(c)
	ctx, cancel := context.WithCancel(context.Background())
	l := &agent.Listener{Cert: cert, Authenticate: h.f.Authenticate, OnSession: h.f.AddSession, Logger: h.log.With("side", "listener")}
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
	waitFor(h.t, "the local server", func() bool {
		v, err := h.st.GetLocalServer(context.Background())
		return err == nil && h.f.Driver(v.ID) != nil
	})
}

func (h *harness) localID() string {
	h.t.Helper()
	v, err := h.st.GetLocalServer(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return v.ID
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
	a := h.startAgent(map[string]string{"primary": h.cfg.AgentListen, "fingerprint": h.fp, "token": tok.Token}, node)
	var id string
	waitFor(h.t, "the registration in agent.json", func() bool {
		id = a.file()["server_id"]
		return id != ""
	})
	waitFor(h.t, "the agent session", func() bool { return h.f.Driver(id) != nil })
	return a, id
}

type testAgent struct {
	h       *harness
	path    string
	node    *fakeNode
	version string // that the agent sends in its hello
	stop    func()
}

func (h *harness) startAgent(file map[string]string, node *fakeNode) *testAgent {
	b, _ := json.Marshal(file)
	path := filepath.Join(h.t.TempDir(), "agent.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		h.t.Fatal(err)
	}
	a := &testAgent{h: h, path: path, node: node, version: h.version}
	a.start()
	return a
}

// start runs the agent with its agent.json until stop.
func (a *testAgent) start() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		agent.Run(ctx, agent.Config{Path: a.path, Node: a.node, Version: a.version, Logger: a.h.log.With("side", "agent")})
		close(done)
	}()
	var once sync.Once
	a.stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	a.h.t.Cleanup(a.stop)
}

func (a *testAgent) file() map[string]string {
	var file map[string]string
	b, err := os.ReadFile(a.path)
	if err == nil {
		err = json.Unmarshal(b, &file)
	}
	if err != nil {
		a.h.t.Fatal(err)
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

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}
	panic("unreachable")
}

// waitEvent reads ch until an event matches.
func waitEvent[E any](t *testing.T, ch <-chan E, match func(E) bool) {
	t.Helper()
	for {
		if match(recv(t, ch)) {
			return
		}
	}
}

// clock is the time of the fleet. The test moves it.
type clock struct{ ns atomic.Int64 }

func (c *clock) now() time.Time      { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) add(d time.Duration) { c.ns.Add(int64(d)) }

// fakeNode is a server, and fakeDriver its driver. The test chooses the
// images and runner versions that work, and the node records its calls.
type fakeNode struct {
	info   host.Info
	events chan sandbox.Event
	stats  chan host.Stats
	mirror mirror.Stats
	limits sandbox.Limits

	mu        sync.Mutex
	sandboxes map[string]sandbox.Sandbox
	digests   map[string]string        // ref: digest; other refs fail
	runners   map[string]bool          // versions that download
	gates     map[string]chan struct{} // pulls of these refs wait until the channel closes
	updateErr error
	calls     []call
}

type call struct {
	method string
	arg    any
}

func newFakeNode(arch string, sandboxIDs ...string) *fakeNode {
	n := &fakeNode{
		info: host.Info{
			Hostname: "node-" + arch, OS: "Ubuntu 24.04.3 LTS", Kernel: "6.8.0-45-generic", Arch: arch,
			Resources: host.Resources{VCPU: 16, MemoryMB: 65536, DiskBytes: 500e9, FreeDiskBytes: 300e9},
			DiskKind:  "NVMe", DockerVersion: "29.1.0", SysboxVersion: "0.7.1", IPv4: "203.0.113.7", Provider: "Hetzner",
		},
		events:    make(chan sandbox.Event, 16),
		stats:     make(chan host.Stats, 1),
		sandboxes: map[string]sandbox.Sandbox{},
		digests:   map[string]string{"builtin:ubuntu-24.04": "sha256:b0b0"},
		runners:   map[string]bool{},
		gates:     map[string]chan struct{}{},
	}
	for _, id := range sandboxIDs {
		n.sandboxes[id] = sandbox.Sandbox{ID: id, State: sandbox.StateBusy}
	}
	return n
}

// set changes the node while the fleet uses it.
func (n *fakeNode) set(fn func(n *fakeNode)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	fn(n)
}

func (n *fakeNode) record(method string, arg any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, call{method, arg})
}

// args returns the argument of each call of method.
func (n *fakeNode) args(method string) []any {
	n.mu.Lock()
	defer n.mu.Unlock()
	var list []any
	for _, c := range n.calls {
		if c.method == method {
			list = append(list, c.arg)
		}
	}
	return list
}

func (n *fakeNode) Local() bool                              { return true }
func (n *fakeNode) Driver() sandbox.Driver                   { return fakeDriver{n: n} }
func (n *fakeNode) Facts(context.Context) (host.Info, error) { return n.info, nil }
func (n *fakeNode) Stats() <-chan host.Stats                 { return n.stats }
func (n *fakeNode) Close() error                             { return nil }

func (n *fakeNode) MirrorStats(context.Context) (mirror.Stats, error) { return n.mirror, nil }

func (n *fakeNode) Doctor(_ context.Context, minFree float64) (doctor.Report, error) {
	n.record("Doctor", minFree)
	return doctor.Report{Info: n.info, Checks: []doctor.Check{{ID: "kernel", Name: "Kernel", Status: doctor.StatusOK, Detail: n.info.Kernel}}}, nil
}

func (n *fakeNode) EnsureImage(ctx context.Context, ref string, progress func(image.Progress)) (sandbox.ImageRef, error) {
	n.record("EnsureImage", ref)
	progress(image.Progress{Status: image.StatusPulling, Total: 4096})
	n.mu.Lock()
	gate := n.gates[ref]
	n.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return sandbox.ImageRef{}, ctx.Err()
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	digest, ok := n.digests[ref]
	if !ok {
		return sandbox.ImageRef{}, fmt.Errorf("pull %s: not found", ref)
	}
	return sandbox.ImageRef{Ref: ref, Digest: digest}, nil
}

func (n *fakeNode) EnsureRunner(_ context.Context, version, arch string) (runner.Bundle, error) {
	n.record("EnsureRunner", version)
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.runners[version] {
		return runner.Bundle{}, fmt.Errorf("download runner %s: 404 Not Found", version)
	}
	return runner.Bundle{Version: version, Arch: arch, SHA256: "9e2f" + version, Patched: true, Dir: "/var/lib/runplane/runners/" + version + "-" + arch}, nil
}

func (n *fakeNode) GCImages(_ context.Context, keep []string) error {
	n.record("GCImages", slices.Sorted(slices.Values(keep)))
	return nil
}

func (n *fakeNode) PruneRunners(_ context.Context, keep []string) error {
	n.record("PruneRunners", slices.Sorted(slices.Values(keep)))
	return nil
}

func (n *fakeNode) Configure(_ context.Context, s agent.Settings) error {
	n.record("Configure", s)
	return nil
}

func (n *fakeNode) Update(_ context.Context, version string) error {
	n.record("Update", version)
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.updateErr
}

// fakeDriver leaves out the driver methods that the fleet does not call.
type fakeDriver struct {
	sandbox.Driver
	n *fakeNode
}

func (d fakeDriver) Mode() sandbox.Mode           { return sandbox.ModeContainer }
func (d fakeDriver) Events() <-chan sandbox.Event { return d.n.events }

func (d fakeDriver) Capacity(context.Context) (sandbox.Capacity, error) {
	return sandbox.Capacity{Mode: sandbox.ModeContainer, Arch: d.n.info.Arch, Limits: d.n.limits}, nil
}

func (d fakeDriver) List(context.Context) ([]sandbox.Sandbox, error) {
	d.n.mu.Lock()
	defer d.n.mu.Unlock()
	return slices.SortedFunc(maps.Values(d.n.sandboxes), func(a, b sandbox.Sandbox) int { return strings.Compare(a.ID, b.ID) }), nil
}

func (d fakeDriver) Stop(_ context.Context, id string) error {
	d.n.record("Stop", id)
	d.n.mu.Lock()
	defer d.n.mu.Unlock()
	delete(d.n.sandboxes, id)
	return nil
}

// testLogger keeps the log of a test and prints it when the test fails.
// Goroutines can still log after the test ends, so it does not use t.Log.
func testLogger(t *testing.T) *slog.Logger {
	buf := &logBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("log:\n%s", buf.String())
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
