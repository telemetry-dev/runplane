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
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/lifecycle"
	"github.com/runplane/runplane/internal/sandbox/netrules"
	"github.com/runplane/runplane/internal/sandbox/scratch"
)

const (
	// vmOverheadMB is the host memory of one VM on top of its guest memory:
	// the Firecracker process, page tables, and device buffers.
	vmOverheadMB = 64
	// maxVCPU is the most vCPUs that Firecracker gives one VM.
	maxVCPU = 32
)

type Options struct {
	DataDir          string // the disk of the VMs, for the capacity
	Sandboxes        string // the sandboxes folder
	ScratchTemplates string
	JailDir          string // the chroots of the jailer
	Kernel           string
	ServicePort      int // cache server and registry mirror on the gateway of each VM
	// RootDisk returns the ext4 root disk of a ready image.
	RootDisk func(digest string) (string, error)
	// RunnerDrive returns the ext4 drive of a ready runner bundle, and if
	// the bundle has the runner patch (guest.Patch).
	RunnerDrive func(ctx context.Context, version, arch string) (drive string, patched bool, err error)
	// NetRules is the firewall of the host, with the TAP range of VM mode.
	NetRules *netrules.Manager
	Settings sandbox.Settings
	Logger   *slog.Logger
}

// Driver operates the microVMs of this server.
type Driver struct {
	*lifecycle.Core[vm]
	opts        Options
	log         *slog.Logger
	ids         idRange
	unit        string // the systemd service of runplane, empty outside one
	firecracker string
	jailer      string
	cgroupRoot  string
	sys         hostOps
	disks       *scratch.Disks
	closeOnce   sync.Once
}

var _ sandbox.Driver = (*Driver)(nil)

// New sets up the firewall for VMs, finds the VMs that operated before a
// restart, and connects to them again.
func New(opts Options) (*Driver, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("firecracker: %w: linux only", errors.ErrUnsupported)
	}
	ids, err := readIDRange("runplane-vm", "/etc/subuid", "/etc/subgid")
	if err != nil {
		return nil, err
	}
	d := newDriver(opts, ids, systemHost{net: opts.NetRules})
	// The jailer names the chroot folder after the real file name.
	if p, err := filepath.EvalSymlinks(d.firecracker); err == nil {
		d.firecracker = p
	}
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		d.unit = serviceUnit(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = d.sys.setupNet(ctx, d.netConfig(d.Settings())); err != nil {
		err = fmt.Errorf("set up the VM firewall: %w", err)
	} else {
		err = d.recover(ctx)
	}
	if err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// newDriver makes a driver that has not touched the host yet.
func newDriver(opts Options, ids idRange, sys hostOps) *Driver {
	d := &Driver{
		opts:        opts,
		log:         opts.Logger,
		ids:         ids,
		firecracker: "/usr/local/bin/firecracker",
		jailer:      "/usr/local/bin/jailer",
		cgroupRoot:  "/sys/fs/cgroup",
		sys:         sys,
		disks:       scratch.New(opts.ScratchTemplates),
	}
	d.Core = lifecycle.New(lifecycle.Options[vm]{
		Mode: sandbox.ModeVM, DataDir: opts.DataDir, Dir: opts.Sandboxes,
		Limits:  lifecycle.Limits{OverheadMB: vmOverheadMB, MaxVCPU: maxVCPU, MaxSandboxes: min(ids.count, sys.tapLeases())},
		Backend: backend{d}, Settings: opts.Settings, Logger: opts.Logger,
	})
	return d
}

// Configure applies new dashboard settings. Firewall changes and the
// bandwidth limit also apply to the VMs that operate.
func (d *Driver) Configure(ctx context.Context, s sandbox.Settings) error {
	old := d.SetSettings(s)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var err error
	if !slices.Equal(old.DNS, s.DNS) || !slices.Equal(old.EgressAllowlist, s.EgressAllowlist) {
		if err = d.sys.setupNet(ctx, d.netConfig(s)); err != nil {
			err = fmt.Errorf("apply the VM firewall: %w", err)
		}
	}
	if old.BandwidthMbps != s.BandwidthMbps {
		for _, b := range d.Boxes() {
			if !b.Ready() {
				continue
			}
			if e := b.Host.api.setBandwidth(ctx, s.BandwidthMbps); e != nil {
				b.Log().Warn("change the VM bandwidth limit", "err", e)
			}
		}
	}
	return err
}

// Probe does the sandbox internet check of the doctor. It does not boot a
// VM: it puts a network namespace behind a free TAP address, with the
// firewall rules of a VM, and fetches the GitHub API from there through the
// DNS server of the settings, as container mode does. Thus it checks the
// host part of the VM network: forwarding, NAT, the runplane rules, and
// other firewalls of the host.
func (d *Driver) Probe(ctx context.Context) error {
	dns := d.Settings().IPv4DNS()
	if len(dns) == 0 {
		return errors.New("the dns setting has no IPv4 server, and VMs have IPv4 only")
	}
	lease, err := d.sys.acquireTap()
	if err != nil {
		return err
	}
	defer func() {
		if err := d.sys.closeTap(lease); err != nil {
			d.log.Warn("remove the probe interface", "err", err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return d.sys.probe(ctx, lease, dns[0], "https://api.github.com")
}

// Close disconnects from the VMs and leaves them running. A new driver
// connects to them again. At a host shutdown, systemd stops the VM scopes
// after runplane (jailerCommand). Close then first tells these VMs to power
// off, so that each runner cancels its job. A kill of the scope would let
// the job fail.
func (d *Driver) Close() error {
	var err error
	d.closeOnce.Do(func() {
		d.stopForShutdown()
		err = d.Core.Close()
		for _, b := range d.Boxes() {
			b.Host.closeConsole()
		}
	})
	return err
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
	for _, b := range d.Boxes() {
		if !units[unitName(b.ID)+".scope"] || !b.Ready() {
			continue
		}
		b.Log().Info("systemd stops the VM")
		wg.Go(func() {
			if err := b.Stop(context.Background(), "the server shuts down"); err != nil {
				b.Log().Warn("stop sandbox", "err", err)
			}
		})
	}
	wg.Wait()
}

func (d *Driver) netConfig(s sandbox.Settings) netrules.Config {
	return netrules.Config{ServicePort: d.opts.ServicePort, DNS: s.IPv4DNS(), Allowlist: s.EgressAllowlist}
}
