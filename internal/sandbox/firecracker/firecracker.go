// Package firecracker is the VM mode driver: each sandbox is a Firecracker
// microVM in the jailer, with its own TAP device, scratch disk, and vsock
// control channel.
//
// Each VM operates in its own systemd scope, so VMs continue when runplane
// restarts. New finds them again and listens on their control sockets.
package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/netrules"
	"github.com/runplane/runplane/internal/sandbox/scratch"
)

// ErrNotFound means that the driver has no operating sandbox with this ID.
var ErrNotFound = errors.New("firecracker: no such sandbox")

// vmOverheadMB is the host memory of one VM on top of its guest memory:
// the Firecracker process, page tables, and device buffers.
const vmOverheadMB = 64

type Options struct {
	DataDir     string // default /var/lib/runplane
	Firecracker string // default /usr/local/bin/firecracker
	Jailer      string // default /usr/local/bin/jailer
	Kernel      string // default <DataDir>/kernel/vmlinux
	// JailerUser has the ID ranges of the VMs in /etc/subuid and
	// /etc/subgid. The default is runplane-vm.
	JailerUser  string
	ServicePort int // cache server and registry mirror on the gateway of each VM; default 8486

	// RootDisk returns the ext4 root disk of a ready image.
	RootDisk func(digest string) (string, error)
	// RunnerDrive returns the ext4 drive of a ready runner bundle.
	RunnerDrive func(ctx context.Context, version, arch string) (string, error)
	// CacheEnv returns the runner environment for the Actions cache at
	// serviceURL. Nil means no cache.
	CacheEnv func(serviceURL string) map[string]string

	Settings sandbox.Settings
	Logger   *slog.Logger
}

// Driver operates the microVMs of this server. It implements sandbox.Driver
// and sandbox.Configurable.
type Driver struct {
	opts       Options
	log        *slog.Logger
	ids        idRange
	unit       string // the systemd service of runplane, empty outside one
	arch       string
	sys        hostOps
	taps       *netrules.Allocator
	disks      *scratch.Disks
	events     *eventQueue
	cgroupRoot string

	mu       sync.Mutex
	vms      map[string]*vm
	settings sandbox.Settings
	closed   bool

	quit chan struct{} // closed by Close
	wg   sync.WaitGroup
}

var (
	_ sandbox.Driver       = (*Driver)(nil)
	_ sandbox.Configurable = (*Driver)(nil)
)

// New sets up the firewall for VMs, finds the VMs that operated before a
// restart, and connects to them again.
func New(opts Options) (*Driver, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("firecracker: %w: linux only", errors.ErrUnsupported)
	}
	opts = withDefaults(opts)
	if opts.RootDisk == nil || opts.RunnerDrive == nil {
		return nil, errors.New("firecracker: RootDisk and RunnerDrive are required")
	}
	// The jailer names the chroot folder after the real file name.
	if p, err := filepath.EvalSymlinks(opts.Firecracker); err == nil {
		opts.Firecracker = p
	}
	ids, err := readIDRange(opts.JailerUser, "/etc/subuid", "/etc/subgid")
	if err != nil {
		return nil, err
	}
	d, err := newDriver(opts, ids, systemHost{net: netrules.New(opts.Logger)})
	if err != nil {
		return nil, err
	}
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		d.unit = serviceUnit(b)
	}
	// The TAPs of VMs that still operate keep their addresses.
	err = d.taps.Recover()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err = d.sys.setupNet(ctx, d.netConfig(d.settings)); err != nil {
			err = fmt.Errorf("set up the VM firewall: %w", err)
		}
	}
	if err == nil {
		err = d.recover()
	}
	if err != nil {
		d.Close()
		return nil, err
	}
	d.spawn(d.sampleLoop)
	return d, nil
}

func withDefaults(opts Options) Options {
	set := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	set(&opts.DataDir, "/var/lib/runplane")
	set(&opts.Firecracker, "/usr/local/bin/firecracker")
	set(&opts.Jailer, "/usr/local/bin/jailer")
	set(&opts.Kernel, filepath.Join(opts.DataDir, "kernel", "vmlinux"))
	set(&opts.JailerUser, "runplane-vm")
	if opts.ServicePort == 0 {
		opts.ServicePort = 8486
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	opts.Settings = settingsDefaults(opts.Settings)
	return opts
}

func settingsDefaults(s sandbox.Settings) sandbox.Settings {
	if len(s.DNS) == 0 {
		s.DNS = []string{"1.1.1.1", "8.8.8.8"}
	}
	return s
}

// newDriver makes a driver that has not touched the host yet.
func newDriver(opts Options, ids idRange, sys hostOps) (*Driver, error) {
	taps, err := netrules.NewAllocator(netrules.DefaultTapSubnet)
	if err != nil {
		return nil, err
	}
	return &Driver{
		opts:       opts,
		log:        opts.Logger,
		ids:        ids,
		arch:       host.Arch(),
		sys:        sys,
		taps:       taps,
		disks:      scratch.New(filepath.Join(opts.DataDir, "scratch-templates")),
		events:     newEventQueue(),
		cgroupRoot: "/sys/fs/cgroup",
		vms:        map[string]*vm{},
		settings:   opts.Settings,
		quit:       make(chan struct{}),
	}, nil
}

func (d *Driver) Mode() sandbox.Mode { return sandbox.ModeVM }

func (d *Driver) Events() <-chan sandbox.Event { return d.events.out }

func (d *Driver) List(context.Context) ([]sandbox.Sandbox, error) {
	list := []sandbox.Sandbox{}
	for _, v := range d.all() {
		list = append(list, v.sandbox())
	}
	return list, nil
}

// Capacity counts each VM with its memory overhead.
func (d *Driver) Capacity(context.Context) (sandbox.Capacity, error) {
	res, err := host.ReadResources(d.opts.DataDir)
	if err != nil {
		return sandbox.Capacity{}, fmt.Errorf("read host resources: %w", err)
	}
	d.mu.Lock()
	s := d.settings
	d.mu.Unlock()
	c := sandbox.Capacity{
		Mode:            sandbox.ModeVM,
		Arch:            d.arch,
		VCPU:            res.VCPU,
		MemoryMB:        res.MemoryMB,
		DiskBytes:       res.DiskBytes,
		FreeDiskBytes:   res.FreeDiskBytes,
		ReserveVCPU:     s.ReserveVCPU,
		ReserveMemoryMB: s.ReserveMemoryMB,
		Overcommit:      s.Overcommit,
	}
	for _, v := range d.all() {
		c.AllocVCPU += v.spec.VCPU
		c.AllocMemoryMB += v.spec.MemoryMB + vmOverheadMB
		c.Sandboxes++
	}
	return c, nil
}

func (d *Driver) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	v, err := d.ready(id)
	if err != nil {
		return nil, err
	}
	return v.log.Reader(ctx, follow), nil
}

func (d *Driver) Shell(ctx context.Context, id string, size sandbox.TermSize) (sandbox.Terminal, error) {
	v, err := d.ready(id)
	if err != nil {
		return nil, err
	}
	t, err := v.ctl.Shell(ctx, size)
	if err != nil {
		return nil, fmt.Errorf("open shell in sandbox %s: %w", id, err)
	}
	return t, nil
}

// Hold tells runplane-init to wait until this time after the runner exits.
// The time limit of the VM moves to the end of the hold.
func (d *Driver) Hold(ctx context.Context, id string, until time.Time) error {
	v, err := d.ready(id)
	if err != nil {
		return err
	}
	if err := v.ctl.Hold(ctx, until); err != nil {
		return fmt.Errorf("hold sandbox %s: %w", id, err)
	}
	v.hold(until)
	return nil
}

// Configure applies new dashboard settings. Firewall changes and the
// bandwidth limit also apply to the VMs that operate.
func (d *Driver) Configure(s sandbox.Settings) error {
	s = settingsDefaults(s)
	d.mu.Lock()
	old := d.settings
	d.settings = s
	d.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	if !slices.Equal(old.DNS, s.DNS) || !slices.Equal(old.EgressAllowlist, s.EgressAllowlist) {
		if err = d.sys.setupNet(ctx, d.netConfig(s)); err != nil {
			err = fmt.Errorf("apply the VM firewall: %w", err)
		}
	}
	if old.BandwidthMbps != s.BandwidthMbps {
		for _, v := range d.all() {
			if !v.isReady() {
				continue
			}
			if e := v.api.setBandwidth(ctx, s.BandwidthMbps); e != nil {
				d.log.Warn("change the VM bandwidth limit", "sandbox", v.id, "err", e)
			}
		}
	}
	return err
}

// Close disconnects from the VMs and leaves them running. A new driver
// connects to them again. At a host shutdown, systemd stops the VM scopes
// after runplane (jailerCommand). Close then first tells these VMs to power
// off, so that each runner cancels its job. A kill of the scope would let
// the job fail.
func (d *Driver) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.mu.Unlock()
	d.stopForShutdown()
	close(d.quit)
	for _, v := range d.all() {
		v.detach()
	}
	d.wg.Wait()
	d.events.close()
	return d.sys.close()
}

// stopForShutdown stops the VMs whose scopes systemd is about to stop, all at
// the same time. It returns when they are gone.
func (d *Driver) stopForShutdown() {
	units, err := d.sys.stopJobs()
	if err != nil {
		d.log.Warn("read the systemd jobs", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, v := range d.all() {
		if !units[unitName(v.id)+".scope"] || !v.isReady() {
			continue
		}
		d.log.Info("systemd stops the VM", "sandbox", v.id)
		wg.Go(func() {
			if err := d.stop(context.Background(), v, "the server shuts down"); err != nil {
				d.log.Warn("stop sandbox", "sandbox", v.id, "err", err)
			}
		})
	}
	wg.Wait()
}

// netConfig is the firewall policy for settings s.
func (d *Driver) netConfig(s sandbox.Settings) netrules.Config {
	dns := ipv4DNS(s)
	if len(dns) < len(s.DNS) {
		d.log.Warn("sandboxes use only the IPv4 DNS servers", "servers", s.DNS)
	}
	return netrules.Config{
		TapSubnet:   netrules.DefaultTapSubnet,
		ServicePort: d.opts.ServicePort,
		DNS:         dns,
		Allowlist:   s.EgressAllowlist,
	}
}

// ipv4DNS returns the IPv4 DNS servers of the settings. Sandboxes have no
// IPv6.
func ipv4DNS(s sandbox.Settings) []netip.Addr {
	var list []netip.Addr
	for _, a := range s.DNS {
		if ip, err := netip.ParseAddr(a); err == nil && ip.Is4() {
			list = append(list, ip)
		}
	}
	return list
}

// spawn runs fn in a goroutine that Close waits for. After Close, it does
// not run fn and returns false.
func (d *Driver) spawn(fn func()) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		fn()
	}()
	return true
}

func (d *Driver) emit(ev sandbox.Event) {
	ev.Time = time.Now().UTC()
	d.events.push(ev)
}

func (d *Driver) all() []*vm {
	d.mu.Lock()
	defer d.mu.Unlock()
	list := make([]*vm, 0, len(d.vms))
	for _, v := range d.vms {
		list = append(list, v)
	}
	return list
}

// ready returns a VM that has started.
func (d *Driver) ready(id string) (*vm, error) {
	d.mu.Lock()
	v := d.vms[id]
	d.mu.Unlock()
	if v == nil || !v.isReady() {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return v, nil
}

func (d *Driver) sandboxDir(id string) string {
	return filepath.Join(d.opts.DataDir, "sandboxes", id)
}

func (d *Driver) chrootBase() string { return filepath.Join(d.opts.DataDir, "jail") }

// jails holds the folder that the jailer makes for each VM.
func (d *Driver) jails() string {
	return filepath.Join(d.chrootBase(), filepath.Base(d.opts.Firecracker))
}

// jailDir is the folder of one VM. Its root folder is the chroot.
func (d *Driver) jailDir(id string) string { return filepath.Join(d.jails(), id) }

func (d *Driver) chroot(id string) string { return filepath.Join(d.jailDir(id), "root") }
