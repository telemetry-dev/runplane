package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/sandbox"
)

// fakeDriver keeps sandboxes in memory. Tests write the logs and drive the
// shells of its sandboxes.
type fakeDriver struct {
	events chan sandbox.Event
	logs   chan logStream
	shells chan *fakeTerminal

	mu        sync.Mutex
	sandboxes map[string]sandbox.Sandbox
	specs     map[string]sandbox.Spec // as Start got them
	stopped   []string
	holds     map[string]time.Time
}

// logStream is one Logs call: the test writes the log to w, and done
// closes when the agent stops reading.
type logStream struct {
	w    *io.PipeWriter
	done <-chan struct{}
}

var fakeCapacity = sandbox.Capacity{
	Mode: sandbox.ModeContainer, Arch: "arm64", VCPU: 16, MemoryMB: 65536,
	DiskBytes: 500e9, FreeDiskBytes: 320e9, AllocVCPU: 4, AllocMemoryMB: 8192,
	ReserveVCPU: 1, ReserveMemoryMB: 2048, Overcommit: 1.5, MaxSandboxes: 10, Sandboxes: 2,
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{
		events:    make(chan sandbox.Event, 16),
		logs:      make(chan logStream, 4),
		shells:    make(chan *fakeTerminal, 4),
		sandboxes: map[string]sandbox.Sandbox{},
		specs:     map[string]sandbox.Spec{},
		holds:     map[string]time.Time{},
	}
}

func (d *fakeDriver) Mode() sandbox.Mode { return sandbox.ModeContainer }

func (d *fakeDriver) Start(_ context.Context, spec sandbox.Spec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.sandboxes[spec.ID]; ok {
		return fmt.Errorf("sandbox %s exists", spec.ID)
	}
	d.specs[spec.ID] = spec
	stored := spec
	stored.JITConfig = ""
	d.sandboxes[spec.ID] = sandbox.Sandbox{ID: spec.ID, PoolID: spec.PoolID, State: sandbox.StateStarting, Spec: stored}
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
	var list []sandbox.Sandbox
	for _, sb := range d.sandboxes {
		list = append(list, sb)
	}
	slices.SortFunc(list, func(a, b sandbox.Sandbox) int { return strings.Compare(a.ID, b.ID) })
	return list, nil
}

func (d *fakeDriver) Capacity(context.Context) (sandbox.Capacity, error) { return fakeCapacity, nil }

func (d *fakeDriver) Events() <-chan sandbox.Event { return d.events }

func (d *fakeDriver) Logs(ctx context.Context, id string, _ bool) (io.ReadCloser, error) {
	if !d.has(id) {
		return nil, fmt.Errorf("no sandbox %s", id)
	}
	pr, pw := io.Pipe()
	context.AfterFunc(ctx, func() { pr.CloseWithError(ctx.Err()) })
	d.logs <- logStream{w: pw, done: ctx.Done()}
	return pr, nil
}

func (d *fakeDriver) Shell(_ context.Context, id string, size sandbox.TermSize) (sandbox.Terminal, error) {
	if !d.has(id) {
		return nil, fmt.Errorf("no sandbox %s", id)
	}
	t := newFakeTerminal(size)
	d.shells <- t
	return t, nil
}

func (d *fakeDriver) Hold(_ context.Context, id string, until time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.holds[id] = until
	return nil
}

func (d *fakeDriver) Close() error { return nil }

func (d *fakeDriver) put(sb sandbox.Sandbox) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sandboxes[sb.ID] = sb
}

func (d *fakeDriver) has(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.sandboxes[id]
	return ok
}

func (d *fakeDriver) spec(id string) sandbox.Spec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.specs[id]
}

func (d *fakeDriver) stoppedIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.stopped)
}

func (d *fakeDriver) hold(id string) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.holds[id]
}

// fakeTerminal echoes what it gets and records the sizes.
type fakeTerminal struct {
	size   sandbox.TermSize // at the open
	sizes  chan sandbox.TermSize
	closed chan struct{}
	out    *io.PipeReader
	outW   *io.PipeWriter
	once   sync.Once
}

func newFakeTerminal(size sandbox.TermSize) *fakeTerminal {
	pr, pw := io.Pipe()
	return &fakeTerminal{size: size, sizes: make(chan sandbox.TermSize, 4), closed: make(chan struct{}), out: pr, outW: pw}
}

func (t *fakeTerminal) Read(p []byte) (int, error)  { return t.out.Read(p) }
func (t *fakeTerminal) Write(p []byte) (int, error) { return t.outW.Write(p) }

func (t *fakeTerminal) Resize(size sandbox.TermSize) error {
	t.sizes <- size
	return nil
}

func (t *fakeTerminal) Close() error {
	t.once.Do(func() {
		close(t.closed)
		t.outW.Close()
	})
	return nil
}

// fakeNode serves fixed host data and records the calls it gets.
type fakeNode struct {
	driver *fakeDriver
	stats  chan host.Stats

	mu         sync.Mutex
	settings   []sandbox.Settings
	updates    []string
	gcKeeps    [][]string
	pruneKeeps [][]string
}

var testInfo = host.Info{
	Hostname: "agent-1", OS: "Ubuntu 24.04.3 LTS", OSID: "ubuntu", OSVersion: "24.04", Kernel: "6.8.0-45-generic", Arch: "arm64",
	Resources: host.Resources{VCPU: 16, MemoryMB: 65536, DiskBytes: 500e9, FreeDiskBytes: 320e9},
	DiskKind:  "NVMe", CgroupV2: true, NFTables: true, IPForward: true,
	DockerVersion: "29.1.0", DockerFirewall: "iptables", SysboxVersion: "0.7.1", SysboxRuntime: true,
	IPv4: "203.0.113.7", Provider: "Hetzner", Region: "fsn1",
}

var testChecks = []host.Check{
	{ID: "kernel", Name: "Kernel", Status: host.StatusOK, Detail: "6.8.0-45-generic"},
	{ID: "sysbox", Name: "Sysbox", Status: host.StatusFail, Detail: "sysbox-runc is missing", Fix: "Install Sysbox 0.7.1"},
}

func newFakeNode() *fakeNode {
	return &fakeNode{driver: newFakeDriver(), stats: make(chan host.Stats, 1)}
}

func (n *fakeNode) Driver() sandbox.Driver                       { return n.driver }
func (n *fakeNode) Facts(context.Context) (host.Info, error)     { return testInfo, nil }
func (n *fakeNode) Doctor(context.Context) ([]host.Check, error) { return testChecks, nil }
func (n *fakeNode) Stats() <-chan host.Stats                     { return n.stats }

func (n *fakeNode) EnsureImage(_ context.Context, ref string, progress func(image.Progress)) (sandbox.ImageRef, error) {
	if ref == "missing:latest" {
		progress(image.Progress{Status: image.StatusFailed, Error: "not found"})
		return sandbox.ImageRef{}, errors.New("pull missing:latest: not found")
	}
	progress(image.Progress{Status: image.StatusPending})
	progress(image.Progress{Status: image.StatusPulling, Done: 50, Total: 100})
	progress(image.Progress{Status: image.StatusPulling, Done: 100, Total: 100})
	progress(image.Progress{Status: image.StatusReady})
	return sandbox.ImageRef{Ref: ref, Digest: "sha256:5f1c"}, nil
}

func (n *fakeNode) EnsureRunner(_ context.Context, version, arch string) (image.Bundle, error) {
	return image.Bundle{Version: version, Arch: arch, SHA256: "9e2f", Patched: true, Dir: "/var/lib/runplane/runners/" + version + "-" + arch}, nil
}

func (n *fakeNode) GCImages(_ context.Context, keep []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.gcKeeps = append(n.gcKeeps, keep)
	return nil
}

func (n *fakeNode) PruneRunners(_ context.Context, keep []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pruneKeeps = append(n.pruneKeeps, keep)
	return nil
}

var testMirror = mirror.Stats{Hits: 41, Misses: 3, HitBytes: 9 << 30, MissBytes: 700 << 20, SizeBytes: 12 << 30}

func (n *fakeNode) Configure(_ context.Context, s sandbox.Settings) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.settings = append(n.settings, s)
	return nil
}

func (n *fakeNode) Update(_ context.Context, version string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.updates = append(n.updates, version)
	return nil
}
