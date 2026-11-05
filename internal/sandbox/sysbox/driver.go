// Package sysbox is the container mode driver. Each sandbox is one Docker
// container with the sysbox-runc runtime, on the Docker bridge network
// "runplane". With dev options (runc, privileged, a scratch volume, and the
// TCP control channel), the devdocker package uses the same driver.
package sysbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
	"github.com/runplane/runplane/internal/sandbox/netrules"
	"github.com/runplane/runplane/internal/sandbox/scratch"
)

// Scratch selects the scratch disk of a sandbox.
type Scratch string

const (
	// ScratchLoop is a sparse ext4 file on a loop device of the host,
	// bind-mounted at /mnt/scratch (container mode).
	ScratchLoop Scratch = "loop"
	// ScratchVolume is a Docker volume at /mnt/scratch (dev mode).
	ScratchVolume Scratch = "volume"
)

// labelManaged marks the Docker objects that runplane makes.
const labelManaged = "dev.runplane.managed"

// labelData marks the containers and volumes of a driver with its data
// folder. Two runplane processes on one Docker, such as `make dev` and the
// end-to-end test on a developer machine, then leave each other's sandboxes
// alone.
const labelData = "dev.runplane.data"

// stopTimeout is the time that runplane-init gets after SIGTERM to stop the
// runner and dockerd and to send its last log lines.
const stopTimeout = 30 * time.Second

// ErrNotFound means that the driver operates no sandbox with this ID.
var ErrNotFound = errors.New("sandbox not found")

var (
	_ sandbox.Driver       = (*Driver)(nil)
	_ sandbox.Configurable = (*Driver)(nil)
)

type Options struct {
	Docker *client.Client
	Config config.Config // data folders and the service port
	Mode   sandbox.Mode  // sandbox.ModeContainer or sandbox.ModeDev
	// Runtime is the OCI runtime of the containers: "sysbox-runc" in
	// container mode, "runc" in dev mode.
	Runtime string
	// Privileged lets dockerd and the overlay mounts operate under runc.
	// Dev mode only.
	Privileged bool
	Scratch    Scratch
	// Bundle returns a runner bundle that is ready on this server, for
	// example (*image.Bundles).Ensure.
	Bundle func(ctx context.Context, version, arch string) (image.Bundle, error)
	// InitPath returns the runplane-init binary, for example initbin.Path.
	// The driver asks at each start, because a temp folder cleaner can
	// remove the file.
	InitPath func() (string, error)
	// NetRules applies the firewall rules to the runplane bridge. Nil means
	// no rules (dev mode).
	NetRules *netrules.Manager
	// ServiceHost is the address of the cache server and the registry
	// mirror for the sandboxes. Empty means the gateway of the runplane
	// network. A name, such as host.docker.internal, points to the Docker
	// host.
	ServiceHost string
	// ControlListen is the TCP address of the control channel in dev mode,
	// for example ":8487". Empty means a Unix socket for each sandbox.
	ControlListen string
	// CgroupParent of the containers. Empty means runplane.slice in
	// container mode and the Docker default in dev mode.
	CgroupParent string
	Settings     sandbox.Settings
	Logger       *slog.Logger
}

// owned limits a Docker filter to the objects of this driver.
func (d *Driver) owned(f client.Filters) client.Filters {
	return f.Add("label", labelData+"="+d.opts.Config.DataDir)
}

// Driver operates the sandbox containers of one server.
type Driver struct {
	opts   Options
	docker *client.Client
	log    *slog.Logger
	disks  *scratch.Disks
	tcp    *control.TCPListener // nil without ControlListen
	dog    *watchdog            // nil without sysbox-runc

	startMu sync.Mutex // one start at a time

	mu       sync.Mutex
	settings sandbox.Settings
	dns      []netip.Addr
	boxes    map[string]*box
	closed   bool

	events chan sandbox.Event
	qmu    sync.Mutex
	queue  []sandbox.Event
	wake   chan struct{}

	ctx       context.Context // ends at Close
	cancel    context.CancelFunc
	loops     sync.WaitGroup
	done      chan struct{}
	closeOnce sync.Once
}

// box is one sandbox that the driver operates. Driver.mu guards the fields
// after the first group.
type box struct {
	id      string
	dir     string
	created time.Time
	spec    sandbox.Spec  // without the JIT config
	removed chan struct{} // closed when remove is done
	once    sync.Once

	logs        *control.LogFile
	ctl         *control.Server
	containerID string
	ip          string
	port        string // host end of the veth pair on the bridge
	state       sandbox.State
	holdUntil   time.Time
	busyAt      time.Time // when the job started; zero before
	stopAsked   bool      // runplane stops it: the end is gone, not failed
	failure     string    // why it failed: a fault that runplane found, or the runner error from init
	cpu         cpuSample
}

// New sets up the runplane network and its firewall rules, takes over the
// sandboxes that still operate, and removes the remains of the others.
func New(opts Options) (*Driver, error) {
	if opts.Docker == nil || opts.Bundle == nil || opts.InitPath == nil {
		return nil, errors.New("sysbox: Docker, Bundle, and InitPath are necessary")
	}
	if opts.Scratch != ScratchLoop && opts.Scratch != ScratchVolume {
		return nil, fmt.Errorf("sysbox: unknown scratch kind %q", opts.Scratch)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.CgroupParent == "" && opts.Mode == sandbox.ModeContainer {
		opts.CgroupParent = "runplane.slice"
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Driver{
		opts:   opts,
		docker: opts.Docker,
		log:    opts.Logger,
		disks:  scratch.New(opts.Config.ScratchTemplatesDir()),
		boxes:  map[string]*box{},
		events: make(chan sandbox.Event),
		wake:   make(chan struct{}, 1),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	if err := d.setup(); err != nil {
		d.Close()
		return nil, err
	}
	go d.pump()
	d.loops.Add(2)
	go d.watchDocker()
	go d.tick()
	return d, nil
}

func (d *Driver) setup() error {
	ctx, cancel := context.WithTimeout(d.ctx, 2*time.Minute)
	defer cancel()
	if err := os.MkdirAll(d.opts.Config.SandboxesDir(), 0o700); err != nil {
		return fmt.Errorf("make sandboxes folder: %w", err)
	}
	net, err := d.ensureNetwork(ctx)
	if err != nil {
		return err
	}
	if d.opts.ServiceHost == "" {
		d.opts.ServiceHost = net.gateway.String()
	}
	if err := d.Configure(d.opts.Settings); err != nil {
		return err
	}
	if d.opts.NetRules != nil {
		if err := d.opts.NetRules.Bridge(net.bridge, net.subnet, net.gateway); err != nil {
			return fmt.Errorf("apply network rules: %w", err)
		}
	}
	if d.opts.ControlListen != "" {
		if d.tcp, err = control.ListenTCP(d.opts.ControlListen, d.opts.ServiceHost, d.log); err != nil {
			return err
		}
	}
	if d.opts.Runtime == "sysbox-runc" {
		d.dog = newWatchdog()
	}
	return d.recover(ctx)
}

func (d *Driver) Mode() sandbox.Mode { return d.opts.Mode }

// Configure takes new settings. New sandboxes get the DNS servers and the
// cache and mirror settings; the firewall policy changes at once.
func (d *Driver) Configure(s sandbox.Settings) error {
	var dns []netip.Addr
	for _, v := range s.DNS {
		a, err := netip.ParseAddr(v)
		if err != nil || !a.Is4() {
			d.log.Warn("skip DNS server; use an IPv4 address", "dns", v)
			continue
		}
		dns = append(dns, a)
	}
	d.mu.Lock()
	d.settings, d.dns = s, dns
	d.mu.Unlock()
	if d.opts.NetRules == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := d.opts.NetRules.Setup(ctx, netrules.Config{
		ServicePort: d.opts.Config.ServicePort,
		DNS:         dns,
		Allowlist:   s.EgressAllowlist,
	})
	if err != nil {
		return fmt.Errorf("apply network rules: %w", err)
	}
	return nil
}

func (d *Driver) List(ctx context.Context) ([]sandbox.Sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	list := make([]sandbox.Sandbox, 0, len(d.boxes))
	for _, b := range d.boxes {
		list = append(list, sandbox.Sandbox{
			ID:        b.id,
			PoolID:    b.spec.PoolID,
			State:     b.state,
			IP:        b.ip,
			CreatedAt: b.created,
			Spec:      b.spec,
		})
	}
	slices.SortFunc(list, func(a, b sandbox.Sandbox) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return list, nil
}

// Capacity is the size of this server, the reserve from the settings, and
// the sum of the sandboxes that operate.
func (d *Driver) Capacity(ctx context.Context) (sandbox.Capacity, error) {
	res, err := host.ReadResources(d.opts.Config.DataDir)
	if err != nil {
		return sandbox.Capacity{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c := sandbox.Capacity{
		Mode:            d.opts.Mode,
		Arch:            host.Arch(),
		VCPU:            res.VCPU,
		MemoryMB:        res.MemoryMB,
		DiskBytes:       res.DiskBytes,
		FreeDiskBytes:   res.FreeDiskBytes,
		ReserveVCPU:     d.settings.ReserveVCPU,
		ReserveMemoryMB: d.settings.ReserveMemoryMB,
		Overcommit:      d.settings.Overcommit,
		Sandboxes:       len(d.boxes),
	}
	for _, b := range d.boxes {
		c.AllocVCPU += b.spec.VCPU
		c.AllocMemoryMB += b.spec.MemoryMB
	}
	return c, nil
}

func (d *Driver) Events() <-chan sandbox.Event { return d.events }

// Logs reads the console log: runner, diag, init, and dockerd lines.
func (d *Driver) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	_, logs, _ := d.channel(id)
	if logs == nil {
		return nil, fmt.Errorf("logs of sandbox %s: %w", id, ErrNotFound)
	}
	return logs.Reader(ctx, follow), nil
}

func (d *Driver) Shell(ctx context.Context, id string, size sandbox.TermSize) (sandbox.Terminal, error) {
	_, _, ctl := d.channel(id)
	if ctl == nil {
		return nil, fmt.Errorf("shell in sandbox %s: %w", id, ErrNotFound)
	}
	return ctl.Shell(ctx, size)
}

// Hold keeps a debug sandbox after its job until the given time. The time
// limit of the sandbox moves to the hold time.
func (d *Driver) Hold(ctx context.Context, id string, until time.Time) error {
	b, _, ctl := d.channel(id)
	if ctl == nil {
		return fmt.Errorf("hold sandbox %s: %w", id, ErrNotFound)
	}
	if err := ctl.Hold(ctx, until); err != nil {
		return fmt.Errorf("hold sandbox %s: %w", id, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	b.holdUntil = until.UTC()
	return d.save(b)
}

// Close stops the loops and the control channels. The containers keep
// operating, and a new driver takes them over.
func (d *Driver) Close() error {
	d.closeOnce.Do(func() {
		d.cancel()
		d.loops.Wait()
		d.mu.Lock()
		d.closed = true
		for _, b := range d.boxes {
			b.closeChannel()
		}
		d.mu.Unlock()
		if d.tcp != nil {
			d.tcp.Close()
		}
		close(d.done)
	})
	return nil
}

func (d *Driver) get(id string) *box {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.boxes[id]
}

// channel returns a sandbox with its log and control channel, or nils.
func (d *Driver) channel(id string) (*box, *control.LogFile, *control.Server) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.boxes[id]
	if b == nil {
		return nil, nil, nil
	}
	return b, b.logs, b.ctl
}

func (d *Driver) containerID(b *box) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return b.containerID
}

// setState moves a sandbox forward to st and sends the change. A state
// never moves back, so a late event cannot undo a newer one.
func (d *Driver) setState(b *box, st sandbox.State) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !forward(b.state, st) {
		return
	}
	b.state = st
	if st == sandbox.StateBusy {
		b.busyAt = time.Now().UTC()
	}
	if err := d.save(b); err != nil {
		d.log.Warn("save sandbox state", "sandbox", b.id, "err", err)
	}
	d.emitState(b.id, st, "")
}

var stateOrder = []sandbox.State{sandbox.StateStarting, sandbox.StateIdle, sandbox.StateBusy, sandbox.StateStopping}

// forward reports if a sandbox can go from cur to next.
func forward(cur, next sandbox.State) bool {
	if cur.Done() {
		return false
	}
	return slices.Index(stateOrder, next) > slices.Index(stateOrder, cur) || next.Done()
}

// handler takes what runplane-init sends.
type handler struct {
	d    *Driver
	b    *box
	logs *control.LogFile
	log  *slog.Logger
}

func (h handler) OnEvent(ev sandbox.RunnerEvent) {
	d, b := h.d, h.b
	d.emit(sandbox.Event{SandboxID: b.id, Type: sandbox.EventRunner, Time: time.Now().UTC(), Runner: &ev})
	switch ev.Kind {
	case sandbox.RunnerListening:
		d.setState(b, sandbox.StateIdle)
	case sandbox.RunnerJobStarted:
		d.setState(b, sandbox.StateBusy)
	case sandbox.RunnerExited:
		d.mu.Lock()
		if b.failure == "" {
			b.failure = ev.Error
		}
		d.mu.Unlock()
	}
}

func (h handler) OnLostConfig() {
	h.log.Warn("runplane restarted before the sandbox got its config; stopping it")
	h.d.Stop(context.Background(), h.b.id)
}

func (h handler) OnLog(line control.LogLine) {
	if err := h.logs.Write(line); err != nil && !errors.Is(err, os.ErrClosed) {
		h.log.Warn("write sandbox log", "err", err)
	}
}

func mkdir(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return fmt.Errorf("make folder: %w", err)
	}
	// MkdirAll applies the umask.
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("set folder mode: %w", err)
	}
	return nil
}
