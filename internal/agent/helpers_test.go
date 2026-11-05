package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/tlsutil"
)

const (
	goodToken  = "rp_srv_good"
	testServer = "01jbserver"
	testSecret = "s3cret"
)

// testPrimary is a Listener over TLS on 127.0.0.1. It accepts goodToken one
// time, and then the server ID and secret that it gave.
type testPrimary struct {
	t        *testing.T
	addr, fp string
	logs     *logBuffer
	hellos   chan Hello
	sessions chan *Session
}

func startPrimary(t *testing.T, cache http.Handler) *testPrimary {
	cert, fp, err := tlsutil.AgentCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &testPrimary{t: t, addr: ln.Addr().String(), fp: fp, logs: newLogBuffer(t), hellos: make(chan Hello, 16), sessions: make(chan *Session, 8)}
	var used atomic.Bool
	l := &Listener{
		Cert: cert,
		Authenticate: func(_ context.Context, h Hello) (string, string, error) {
			select {
			case p.hellos <- h:
			default:
			}
			switch {
			case h.Token == goodToken && used.CompareAndSwap(false, true):
				return testServer, testSecret, nil
			case h.Token != "":
				return "", "", errors.New("the install token is not valid")
			case h.ServerID == testServer && h.Secret == testSecret:
				return testServer, "", nil
			}
			return "", "", errors.New("unknown server")
		},
		OnSession:    func(s *Session) { p.sessions <- s },
		CacheHandler: cache,
		Logger:       p.logs.logger("primary"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return p
}

// agentJSON is the agent.json that install.sh writes.
func (p *testPrimary) agentJSON(token string) string {
	return fmt.Sprintf("{\n  \"primary\": %q,\n  \"fingerprint\": %q,\n  \"token\": %q\n}\n", p.addr, p.fp, token)
}

type testAgent struct {
	path   string
	cancel context.CancelFunc
	done   chan struct{}
	err    error // of Run, after done closes
}

func (p *testPrimary) startAgent(agentJSON string, node *fakeNode, proxy *CacheProxy) *testAgent {
	path := filepath.Join(p.t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(agentJSON), 0o600); err != nil {
		p.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &testAgent{path: path, cancel: cancel, done: make(chan struct{})}
	go func() {
		a.err = Run(ctx, Config{
			Path: path, Node: node, CacheProxy: proxy, Mirror: func() mirror.Stats { return testMirror }, Logger: p.logs.logger("agent"),
		})
		close(a.done)
	}()
	p.t.Cleanup(func() {
		cancel()
		waitClosed(p.t, a.done)
	})
	return a
}

// result waits for Run to return.
func (a *testAgent) result(t *testing.T) error {
	t.Helper()
	waitClosed(t, a.done)
	return a.err
}

// connect starts a primary and a registered agent.
func connect(t *testing.T) (*testAgent, *fakeNode, *Session) {
	t.Helper()
	p := startPrimary(t, nil)
	node := newFakeNode()
	a := p.startAgent(p.agentJSON(goodToken), node, nil)
	return a, node, recv(t, p.sessions)
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	panic("unreachable")
}

// waitClosed waits until ch closes. It drops the values that come before.
func waitClosed[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for the channel to close")
		}
	}
}

// readFull reads n bytes from r. A read error comes back as the text.
func readFull(t *testing.T, r io.Reader, n int) string {
	t.Helper()
	ch := make(chan string, 1)
	go func() {
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			ch <- "read error: " + err.Error()
			return
		}
		ch <- string(b)
	}()
	return recv(t, ch)
}

// logBuffer keeps the logs of a test and prints them when the test fails.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func newLogBuffer(t *testing.T) *logBuffer {
	l := &logBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("logs:\n%s", l.String())
		}
	})
	return l
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

func (l *logBuffer) logger(side string) *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug})).With("side", side)
}

func (l *logBuffer) waitFor(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(l.String(), text) {
		if time.Now().After(deadline) {
			t.Fatalf("no log line with %q", text)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var (
	testInfo = host.Info{
		Hostname: "agent-1", OS: "Ubuntu 24.04.3 LTS", OSID: "ubuntu", OSVersion: "24.04", Kernel: "6.8.0-45-generic", Arch: "arm64",
		Resources: host.Resources{VCPU: 16, MemoryMB: 65536, DiskBytes: 500e9, FreeDiskBytes: 320e9},
		DiskKind:  "NVMe", CgroupV2: true, NFTables: true, IPForward: true,
		DockerVersion: "29.1.0", DockerFirewall: "iptables", SysboxVersion: "0.7.1", SysboxRuntime: true,
		IPv4: "203.0.113.7", Provider: "Hetzner", Region: "fsn1",
	}
	testChecks = []host.Check{
		{ID: "kernel", Name: "Kernel", Status: host.StatusOK, Detail: "6.8.0-45-generic"},
		{ID: "sysbox", Name: "Sysbox", Status: host.StatusFail, Detail: "sysbox-runc is missing", Fix: "Install Sysbox 0.7.1"},
	}
	testProgress = []image.Progress{
		{Status: image.StatusPending},
		{Status: image.StatusPulling, Done: 50, Total: 100},
		{Status: image.StatusPulling, Done: 100, Total: 100},
		{Status: image.StatusReady},
	}
	testMirror   = mirror.Stats{Hits: 41, Misses: 3, HitBytes: 9 << 30, MissBytes: 700 << 20, SizeBytes: 12 << 30}
	fakeCapacity = sandbox.Capacity{
		Mode: sandbox.ModeContainer, Arch: "arm64", VCPU: 16, MemoryMB: 65536,
		DiskBytes: 500e9, FreeDiskBytes: 320e9, AllocVCPU: 4, AllocMemoryMB: 8192,
		ReserveVCPU: 1, ReserveMemoryMB: 2048, Overcommit: 1.5, MaxSandboxes: 10, Sandboxes: 2,
	}
)

// fakeNode is the node of a test agent and its driver. It keeps sandboxes
// in memory and records the calls that change something. The test writes
// the logs and drives the shells.
type fakeNode struct {
	events chan sandbox.Event
	stats  chan host.Stats
	logs   chan logStream
	shells chan *fakeTerminal

	mu        sync.Mutex
	sandboxes map[string]sandbox.Sandbox
	calls     [][]any
}

// logStream is one Logs call: the test writes the log to w, and done
// closes when the agent stops reading.
type logStream struct {
	w    *io.PipeWriter
	done <-chan struct{}
}

func newFakeNode() *fakeNode {
	return &fakeNode{
		events:    make(chan sandbox.Event, 16),
		stats:     make(chan host.Stats, 1),
		logs:      make(chan logStream, 4),
		shells:    make(chan *fakeTerminal, 4),
		sandboxes: map[string]sandbox.Sandbox{},
	}
}

// record adds a call, and returns nil as the result of the call.
func (n *fakeNode) record(call ...any) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, call)
	return nil
}

// takeCalls returns the calls since the last takeCalls.
func (n *fakeNode) takeCalls() [][]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	calls := n.calls
	n.calls = nil
	return calls
}

func (n *fakeNode) put(sb sandbox.Sandbox) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sandboxes[sb.ID] = sb
}

func (n *fakeNode) Driver() sandbox.Driver                       { return n }
func (n *fakeNode) Facts(context.Context) (host.Info, error)     { return testInfo, nil }
func (n *fakeNode) Doctor(context.Context) ([]host.Check, error) { return testChecks, nil }
func (n *fakeNode) Stats() <-chan host.Stats                     { return n.stats }

func (n *fakeNode) EnsureImage(_ context.Context, ref string, progress func(image.Progress)) (sandbox.ImageRef, error) {
	for _, p := range testProgress {
		progress(p)
	}
	if ref == "missing:latest" {
		return sandbox.ImageRef{}, errors.New("pull missing:latest: not found")
	}
	return sandbox.ImageRef{Ref: ref, Digest: "sha256:5f1c"}, nil
}

func (n *fakeNode) EnsureRunner(_ context.Context, version, arch string) (image.Bundle, error) {
	return image.Bundle{Version: version, Arch: arch, SHA256: "9e2f", Patched: true, Dir: "/var/lib/runplane/runners/" + version + "-" + arch}, nil
}

func (n *fakeNode) GCImages(_ context.Context, keep []string) error {
	return n.record("GCImages", keep)
}
func (n *fakeNode) PruneRunners(_ context.Context, keep []string) error {
	return n.record("PruneRunners", keep)
}
func (n *fakeNode) Configure(_ context.Context, s sandbox.Settings) error {
	return n.record("Configure", s)
}
func (n *fakeNode) Update(_ context.Context, version string) error {
	return n.record("Update", version)
}

func (n *fakeNode) Mode() sandbox.Mode { return sandbox.ModeContainer }

func (n *fakeNode) Start(_ context.Context, spec sandbox.Spec) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.sandboxes[spec.ID]; ok {
		return fmt.Errorf("sandbox %s exists", spec.ID)
	}
	n.calls = append(n.calls, []any{"Start", spec})
	stored := spec
	stored.JITConfig = ""
	n.sandboxes[spec.ID] = sandbox.Sandbox{ID: spec.ID, PoolID: spec.PoolID, State: sandbox.StateStarting, Spec: stored}
	return nil
}

func (n *fakeNode) Stop(_ context.Context, id string) error {
	n.mu.Lock()
	delete(n.sandboxes, id)
	n.mu.Unlock()
	return n.record("Stop", id)
}

func (n *fakeNode) List(context.Context) ([]sandbox.Sandbox, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.SortedFunc(maps.Values(n.sandboxes), func(a, b sandbox.Sandbox) int { return strings.Compare(a.ID, b.ID) }), nil
}

func (n *fakeNode) Capacity(context.Context) (sandbox.Capacity, error) { return fakeCapacity, nil }
func (n *fakeNode) Events() <-chan sandbox.Event                       { return n.events }

func (n *fakeNode) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	n.mu.Lock()
	_, ok := n.sandboxes[id]
	n.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no sandbox %s", id)
	}
	pr, pw := io.Pipe()
	context.AfterFunc(ctx, func() { pr.CloseWithError(ctx.Err()) })
	n.logs <- logStream{w: pw, done: ctx.Done()}
	return pr, n.record("Logs", id, follow)
}

// Shell opens a terminal that echoes its input. Its sizes channel gets the
// size at the open and at each resize.
func (n *fakeNode) Shell(_ context.Context, _ string, size sandbox.TermSize) (sandbox.Terminal, error) {
	pr, pw := io.Pipe()
	t := &fakeTerminal{PipeReader: pr, w: pw, sizes: make(chan sandbox.TermSize, 4), closed: make(chan struct{})}
	t.sizes <- size
	n.shells <- t
	return t, nil
}

func (n *fakeNode) Hold(_ context.Context, id string, until time.Time) error {
	return n.record("Hold", id, until)
}

func (n *fakeNode) Close() error { return nil }

type fakeTerminal struct {
	*io.PipeReader
	w      *io.PipeWriter
	sizes  chan sandbox.TermSize
	closed chan struct{}
	once   sync.Once
}

func (t *fakeTerminal) Write(p []byte) (int, error) { return t.w.Write(p) }

func (t *fakeTerminal) Resize(size sandbox.TermSize) error {
	t.sizes <- size
	return nil
}

func (t *fakeTerminal) Close() error {
	t.once.Do(func() {
		close(t.closed)
		t.w.Close()
	})
	return nil
}
