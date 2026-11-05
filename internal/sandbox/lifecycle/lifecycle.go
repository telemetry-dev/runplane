// Package lifecycle is the part of a sandbox driver that is the same in all
// modes: the record of each sandbox, its states, its time limit, its control
// channel, the events, the metrics, and the takeover after a restart of
// runplane. A driver gives a Backend for the parts that depend on how its
// sandboxes run.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
)

const (
	// limitGrace lets runplane-init end a sandbox at its time limit, also
	// after it waited 2 minutes for a debug hold, before the driver stops
	// the sandbox.
	limitGrace = 5 * time.Minute
	// stopGrace is the time for runplane-init to stop the runner and
	// Docker, send the last logs, and power off.
	stopGrace = 30 * time.Second
	// killWait is the time for a sandbox to end after a kill.
	killWait     = 10 * time.Second
	shutdownWait = 5 * time.Second
	metricsEvery = 5 * time.Second
	minMemoryMB  = 128
)

// A sandbox ID is also a folder, a container, a jailer, and a unit name.
var idPattern = regexp.MustCompile(`^[0-9a-z]{1,64}$`)

// ValidID reports if id can be the ID of a sandbox.
func ValidID(id string) bool { return idPattern.MatchString(id) }

var errClosed = errors.New("the driver is closed")

// Backend operates the sandboxes of one driver. T is the part of a sandbox
// that only the backend knows. Its exported fields are in the record of the
// sandbox, so they come back after a restart.
type Backend[T any] interface {
	// Create makes and starts the sandbox of b with spec, which has the JIT
	// config. It gives the control channel to Box.SetControl. It saves the
	// record (Box.Save) before it makes a part that only the exported fields
	// of Box.Host find again. After an error, no process of the sandbox
	// operates; Remove removes the parts.
	Create(ctx context.Context, b *Box[T], spec sandbox.Spec, s sandbox.Settings) error
	// Adopt connects to a sandbox from before a restart of runplane, with a
	// control channel that has no config. It returns false if the sandbox
	// does not operate.
	Adopt(ctx context.Context, b *Box[T]) (bool, error)
	// Shutdown asks the sandbox to power off when its guest does not answer
	// on the control channel.
	Shutdown(ctx context.Context, b *Box[T]) error
	// Kill ends the sandbox at once.
	Kill(ctx context.Context, b *Box[T]) error
	// Fault tells why the sandbox ended badly, or returns "". The process of
	// the sandbox has ended.
	Fault(ctx context.Context, b *Box[T]) string
	// Remove removes the parts of the sandbox. Its process ended or never
	// started. After an error, the sandbox folder stays, and the next start
	// of runplane tries again.
	Remove(ctx context.Context, b *Box[T]) error
	// Usage reads the CPU time and the memory of the sandbox.
	Usage(ctx context.Context, b *Box[T]) (Usage, error)
}

// Limits are the limits of a driver on its sandboxes.
type Limits struct {
	OverheadMB   int // host memory of each sandbox on top of its own
	MaxVCPU      int // 0 = no limit
	MaxSandboxes int // 0 = no limit
}

type Options[T any] struct {
	Mode    sandbox.Mode
	DataDir string // selects the data disk for Capacity
	// Dir is the sandboxes folder. It has a folder with the record for each
	// sandbox.
	Dir      string
	Limits   Limits
	Backend  Backend[T]
	Settings sandbox.Settings
	Logger   *slog.Logger
}

// Core is the part of a driver that is the same in all modes. A driver
// embeds it for most methods of sandbox.Driver.
type Core[T any] struct {
	mode    sandbox.Mode
	dataDir string
	dir     string
	limits  Limits
	backend Backend[T]
	log     *slog.Logger
	events  *queue

	ctx    context.Context // ends at Close
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	boxes    map[string]*Box[T]
	settings sandbox.Settings
	closed   bool
}

func New[T any](o Options[T]) *Core[T] {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Core[T]{
		mode: o.Mode, dataDir: o.DataDir, dir: o.Dir, limits: o.Limits, backend: o.Backend, log: o.Logger,
		events: newQueue(), ctx: ctx, cancel: cancel, boxes: map[string]*Box[T]{}, settings: o.Settings,
	}
	c.Go(c.sampleLoop)
	return c
}

func (c *Core[T]) Mode() sandbox.Mode { return c.mode }

func (c *Core[T]) Events() <-chan sandbox.Event { return c.events.out }

// Settings returns the settings for new sandboxes.
func (c *Core[T]) Settings() sandbox.Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings
}

// SetSettings keeps s for new sandboxes and returns the settings before.
func (c *Core[T]) SetSettings(s sandbox.Settings) sandbox.Settings {
	if len(s.IPv4DNS()) < len(s.DNS) {
		c.log.Warn("sandboxes use only the IPv4 DNS servers", "servers", s.DNS)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.settings
	c.settings = s
	return old
}

// Start makes the record and the folder of the sandbox and calls
// Backend.Create. Events of the sandbox wait until Start returns, so that a
// failed Start sends none.
func (c *Core[T]) Start(ctx context.Context, spec sandbox.Spec) error {
	if err := c.check(&spec); err != nil {
		return fmt.Errorf("start sandbox %s: %w", spec.ID, err)
	}
	b := c.newBox(spec.ID, spec, time.Now().UTC())
	c.mu.Lock()
	var err error
	switch {
	case c.closed:
		err = errClosed
	case c.boxes[spec.ID] != nil:
		err = errors.New("it exists")
	default:
		c.boxes[spec.ID] = b
	}
	settings := c.settings
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("start sandbox %s: %w", spec.ID, err)
	}
	if err := c.create(ctx, b, spec, settings); err != nil {
		end := b.consoleEnd()
		c.abort(b)
		return fmt.Errorf("start sandbox %s: %w%s", spec.ID, err, end)
	}
	b.mu.Lock()
	b.live = true
	b.emit(sandbox.Event{Type: sandbox.EventState, State: sandbox.StateStarting})
	for _, ev := range b.pending {
		c.events.push(ev)
	}
	b.pending = nil
	b.arm()
	b.mu.Unlock()
	if !c.Go(func(ctx context.Context) { c.watch(ctx, b) }) {
		// The sandbox operates on; the next driver takes it over.
		b.detach()
	}
	close(b.started)
	b.log.Info("sandbox started", "pool", spec.PoolID, "image", spec.Image.Ref)
	return nil
}

func (c *Core[T]) check(spec *sandbox.Spec) error {
	arch := host.Arch()
	if spec.Arch == "" {
		spec.Arch = arch
	}
	switch {
	case !ValidID(spec.ID):
		return fmt.Errorf("the ID %q is not 1 to 64 lowercase letters and digits", spec.ID)
	case spec.Arch != arch:
		return fmt.Errorf("arch %q on a %s server", spec.Arch, arch)
	case spec.Image.Digest == "":
		return errors.New("the image has no digest")
	case spec.VCPU < 1:
		return fmt.Errorf("%d vCPUs; a sandbox needs at least 1", spec.VCPU)
	case c.limits.MaxVCPU > 0 && spec.VCPU > c.limits.MaxVCPU:
		return fmt.Errorf("%d vCPUs; a sandbox in %s mode has at most %d", spec.VCPU, c.mode, c.limits.MaxVCPU)
	case spec.MemoryMB < minMemoryMB:
		return fmt.Errorf("%d MB memory; a sandbox needs at least %d MB", spec.MemoryMB, minMemoryMB)
	case spec.DiskGB < 1:
		return fmt.Errorf("%d GB disk; a sandbox needs at least 1 GB", spec.DiskGB)
	}
	return nil
}

func (c *Core[T]) create(ctx context.Context, b *Box[T], spec sandbox.Spec, s sandbox.Settings) error {
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return fmt.Errorf("make the sandbox folder: %w", err)
	}
	if err := b.Save(); err != nil {
		return err
	}
	logs, err := control.OpenLogFile(b.Dir)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.logs = logs
	b.mu.Unlock()
	return c.backend.Create(ctx, b, spec, s)
}

// abort removes a sandbox whose start failed.
func (c *Core[T]) abort(b *Box[T]) {
	b.detach()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c.removeParts(ctx, b)
	c.drop(b)
	close(b.removed)
	close(b.started)
}

// Stop stops the sandbox: runplane-init powers off, or the backend kills the
// sandbox after stopGrace.
func (c *Core[T]) Stop(ctx context.Context, id string) error {
	b := c.Box(id)
	if b == nil {
		return nil
	}
	if err := b.Stop(ctx, "stopped by runplane"); err != nil {
		return fmt.Errorf("stop sandbox %s: %w", id, err)
	}
	return nil
}

func (c *Core[T]) List(context.Context) ([]sandbox.Sandbox, error) {
	boxes := c.Boxes()
	list := make([]sandbox.Sandbox, 0, len(boxes))
	for _, b := range boxes {
		list = append(list, b.sandbox())
	}
	slices.SortFunc(list, func(a, b sandbox.Sandbox) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return list, nil
}

// Capacity is the size of this server, the sum of its sandboxes, and the
// limits of the driver.
func (c *Core[T]) Capacity(context.Context) (sandbox.Capacity, error) {
	res, err := host.ReadResources(c.dataDir)
	if err != nil {
		return sandbox.Capacity{}, fmt.Errorf("read host resources: %w", err)
	}
	cp := sandbox.Capacity{
		Mode: c.mode, Arch: host.Arch(), VCPU: res.VCPU, MemoryMB: res.MemoryMB,
		DiskBytes: res.DiskBytes, FreeDiskBytes: res.FreeDiskBytes,
		OverheadMB: c.limits.OverheadMB, MaxVCPU: c.limits.MaxVCPU, MaxSandboxes: c.limits.MaxSandboxes,
	}
	for _, b := range c.Boxes() {
		cp.AllocVCPU += b.spec.VCPU
		cp.AllocMemoryMB += b.spec.MemoryMB + c.limits.OverheadMB
		cp.Sandboxes++
	}
	return cp, nil
}

// Logs reads the console log: runner, diag, init, dockerd, and console lines.
func (c *Core[T]) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	if b := c.Box(id); b != nil {
		b.mu.Lock()
		logs := b.logs
		b.mu.Unlock()
		if logs != nil {
			return logs.Reader(ctx, follow), nil
		}
	}
	return nil, fmt.Errorf("logs of sandbox %s: %w", id, sandbox.ErrNotFound)
}

func (c *Core[T]) Shell(ctx context.Context, id string, size sandbox.TermSize) (sandbox.Terminal, error) {
	ctl, err := c.control(id)
	if err != nil {
		return nil, fmt.Errorf("shell in sandbox %s: %w", id, err)
	}
	t, err := ctl.Shell(ctx, size)
	if err != nil {
		return nil, fmt.Errorf("open shell in sandbox %s: %w", id, err)
	}
	return t, nil
}

// Hold tells runplane-init to wait until this time after the runner exits.
// The time limit of the sandbox moves to the end of the hold.
func (c *Core[T]) Hold(ctx context.Context, id string, until time.Time) error {
	ctl, err := c.control(id)
	if err == nil {
		err = ctl.Hold(ctx, until)
	}
	if err != nil {
		return fmt.Errorf("hold sandbox %s: %w", id, err)
	}
	if b := c.Box(id); b != nil {
		b.mu.Lock()
		b.holdUntil = until.UTC()
		b.saveLocked()
		b.arm()
		b.mu.Unlock()
	}
	return nil
}

// control returns the control channel of a sandbox that has started.
func (c *Core[T]) control(id string) (*control.Server, error) {
	b := c.Box(id)
	if b == nil || !isClosed(b.started) || isClosed(b.removed) {
		return nil, sandbox.ErrNotFound
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctl == nil {
		return nil, sandbox.ErrNotFound
	}
	return b.ctl, nil
}

// Box returns the sandbox id, or nil.
func (c *Core[T]) Box(id string) *Box[T] {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.boxes[id]
}

func (c *Core[T]) Boxes() []*Box[T] {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Collect(maps.Values(c.boxes))
}

// Go runs fn in a goroutine that Close waits for. ctx ends at Close. After
// Close, Go does not run fn and returns false.
func (c *Core[T]) Go(fn func(ctx context.Context)) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.wg.Go(func() { fn(c.ctx) })
	return true
}

// Recover takes over the sandboxes of this mode from before a restart of
// runplane: the records in the sandboxes folder. It removes the sandboxes
// that no longer operate, and sends no events for them. The records of other
// modes stay for their driver. Recover returns the IDs that it took over.
func (c *Core[T]) Recover(ctx context.Context) (map[string]bool, error) {
	taken := map[string]bool{}
	entries, err := os.ReadDir(c.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return taken, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the sandbox folder: %w", err)
	}
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || !ValidID(id) {
			continue
		}
		rec, err := readRecord[T](filepath.Join(c.dir, id))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err == nil && rec.Spec.ID != id:
			err = fmt.Errorf("the record is of sandbox %q", rec.Spec.ID)
		}
		if err != nil {
			c.log.Warn("skip a sandbox folder with a bad record", "sandbox", id, "err", err)
			continue
		}
		if rec.Mode != c.mode {
			continue
		}
		if c.adopt(ctx, c.fromRecord(id, rec)) {
			taken[id] = true
		}
	}
	return taken, nil
}

func (c *Core[T]) adopt(ctx context.Context, b *Box[T]) bool {
	logs, err := control.OpenLogFile(b.Dir)
	alive := false
	if err == nil {
		b.logs = logs
		alive, err = c.backend.Adopt(ctx, b)
	}
	if !alive || err != nil {
		if err != nil {
			b.log.Warn("remove a sandbox from before the restart", "err", err)
		} else {
			b.log.Info("a sandbox stopped while runplane did not operate")
		}
		b.detach()
		c.removeParts(ctx, b)
		return false
	}
	c.mu.Lock()
	c.boxes[b.ID] = b
	c.mu.Unlock()
	c.Go(func(ctx context.Context) { c.watch(ctx, b) })
	b.mu.Lock()
	state, stopping := b.state, b.stopReason != "" || b.failure != ""
	if !stopping {
		b.arm()
	}
	b.mu.Unlock()
	b.log.Info("took over sandbox", "state", state)
	if stopping {
		// The restart came during a stop.
		b.halt("", "")
	}
	return true
}

func (c *Core[T]) watch(ctx context.Context, b *Box[T]) {
	select {
	case <-b.exited:
		c.finish(b)
	case <-ctx.Done():
	}
}

// finish removes a sandbox whose process ended and sends its last state.
func (c *Core[T]) finish(b *Box[T]) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fault := c.backend.Fault(ctx, b)
	b.mu.Lock()
	state, reason := b.outcome(fault)
	killed := b.killed
	b.mu.Unlock()
	log := b.log.Info
	if state == sandbox.StateFailed {
		log = b.log.Warn
	}
	log("sandbox ended", "state", state, "reason", reason, "killed", killed)
	b.detach()
	c.removeParts(ctx, b)
	c.drop(b)
	b.mu.Lock()
	b.state = state
	b.emit(sandbox.Event{Type: sandbox.EventState, State: state, Reason: reason})
	b.mu.Unlock()
	close(b.removed)
}

func (c *Core[T]) removeParts(ctx context.Context, b *Box[T]) {
	if err := c.backend.Remove(ctx, b); err != nil {
		b.log.Warn("remove sandbox", "err", err)
		return
	}
	if err := os.RemoveAll(b.Dir); err != nil {
		b.log.Warn("remove the sandbox folder", "err", err)
	}
}

func (c *Core[T]) drop(b *Box[T]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.boxes[b.ID] == b {
		delete(c.boxes, b.ID)
	}
}

// halt does a stop of b: a shutdown request, stopGrace, then a kill. It
// returns when b is gone.
func (c *Core[T]) halt(ctx context.Context, b *Box[T]) error {
	select {
	case <-b.started:
	case <-ctx.Done():
		return errClosed
	}
	if isClosed(b.removed) {
		return nil
	}
	if c.shutdown(b) {
		grace := time.NewTimer(stopGrace)
		defer grace.Stop()
		select {
		case <-b.exited:
		case <-grace.C:
		case <-ctx.Done():
			return errClosed
		}
	}
	if !isClosed(b.exited) {
		if err := c.kill(ctx, b); err != nil {
			return err
		}
	}
	select {
	case <-b.removed:
		return nil
	case <-ctx.Done():
		return errClosed
	}
}

// shutdown asks the sandbox to power off, and reports if it got the request.
func (c *Core[T]) shutdown(b *Box[T]) bool {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()
	err := control.ErrNotConnected
	b.mu.Lock()
	ctl := b.ctl
	b.mu.Unlock()
	if ctl != nil {
		err = ctl.Shutdown(ctx)
	}
	if err != nil {
		err = c.backend.Shutdown(ctx, b)
	}
	if err != nil {
		b.log.Debug("ask the sandbox to power off", "err", err)
	}
	return err == nil
}

func (c *Core[T]) kill(ctx context.Context, b *Box[T]) error {
	b.mu.Lock()
	b.killed = true
	b.mu.Unlock()
	killCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.backend.Kill(killCtx, b); err != nil {
		b.log.Warn("kill sandbox", "err", err)
	}
	wait := time.NewTimer(killWait)
	defer wait.Stop()
	select {
	case <-b.exited:
		return nil
	case <-wait.C:
		return errors.New("the sandbox does not exit")
	case <-ctx.Done():
		return errClosed
	}
}

// Close disconnects from the sandboxes and leaves them running. A new
// driver takes them over.
func (c *Core[T]) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	boxes := slices.Collect(maps.Values(c.boxes))
	c.mu.Unlock()
	c.cancel()
	for _, b := range boxes {
		b.detach()
	}
	c.wg.Wait()
	c.events.close()
	return nil
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
