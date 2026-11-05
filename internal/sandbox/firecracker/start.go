package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// runnerDir is where runplane-init mounts the runner drive in the guest.
const runnerDir = "/opt/runplane/runner"

// maxSocketPath is the longest Unix socket path that Linux takes.
const maxSocketPath = 107

// The jailer takes IDs of letters, digits, and hyphens.
var idRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// Start prepares the network, the scratch disk, and the chroot of a VM,
// starts Firecracker in the jailer, and boots the VM. It returns when the VM
// boots; runplane-init then connects to the control channel.
func (d *Driver) Start(ctx context.Context, spec sandbox.Spec) error {
	if err := d.check(spec); err != nil {
		return err
	}
	root, err := d.opts.RootDisk(spec.Image.Digest)
	if err != nil {
		return fmt.Errorf("start sandbox %s: %w", spec.ID, err)
	}
	runner, patched, err := d.opts.RunnerDrive(ctx, spec.RunnerVersion, spec.Arch)
	if err != nil {
		return fmt.Errorf("start sandbox %s: %w", spec.ID, err)
	}
	lease, err := d.taps.Acquire()
	if err != nil {
		return err
	}
	v := newVM(d, spec, lease, time.Now().UTC())
	d.mu.Lock()
	switch {
	case d.closed:
		err = errors.New("firecracker: the driver is closed")
	case d.vms[spec.ID] != nil:
		err = fmt.Errorf("firecracker: sandbox %s exists", spec.ID)
	default:
		d.vms[spec.ID] = v
	}
	settings := d.settings
	d.mu.Unlock()
	if err != nil {
		d.taps.Release(lease.Index)
		return err
	}

	cmd, err := d.launch(ctx, v, spec, settings, root, runner, patched)
	if err != nil {
		d.abort(v, cmd)
		return fmt.Errorf("start sandbox %s: %w", spec.ID, v.withConsole(err))
	}
	close(v.ready)
	if !d.spawn(func() { d.watch(v) }) {
		// The VM operates on; the next driver finds it.
		v.detach()
		return nil
	}
	v.armTimer()
	d.emit(sandbox.Event{SandboxID: v.id, Type: sandbox.EventState, State: sandbox.StateStarting})
	return nil
}

func (d *Driver) check(spec sandbox.Spec) error {
	switch {
	case !idRE.MatchString(spec.ID):
		return fmt.Errorf("firecracker: sandbox ID %q is not valid", spec.ID)
	case spec.Arch != host.Arch():
		return fmt.Errorf("firecracker: sandbox arch %q on a %s server", spec.Arch, host.Arch())
	case spec.VCPU < 1 || spec.VCPU > 32:
		return fmt.Errorf("firecracker: %d vCPUs; a VM has 1 to 32", spec.VCPU)
	case spec.MemoryMB < 128:
		return fmt.Errorf("firecracker: %d MB memory; a VM needs at least 128 MB", spec.MemoryMB)
	case spec.DiskGB < 1:
		return fmt.Errorf("firecracker: %d GB disk; a VM needs at least 1 GB", spec.DiskGB)
	}
	if p := filepath.Join(d.chroot(spec.ID), controlSocket); len(p) > maxSocketPath {
		return fmt.Errorf("firecracker: the socket path %s is longer than %d bytes; use a shorter data folder", p, maxSocketPath)
	}
	return nil
}

// launch does the work of Start. It returns the jailer command once that
// started, also with an error.
func (d *Driver) launch(ctx context.Context, v *vm, spec sandbox.Spec, s sandbox.Settings, root, runner string, patched bool) (*exec.Cmd, error) {
	dir := d.sandboxDir(v.id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create sandbox folder: %w", err)
	}
	uid, gid, err := d.ids.of(v.lease.Index)
	if err != nil {
		return nil, err
	}
	// The record comes first, so a restart in the middle finds and
	// removes the parts below.
	v.save()
	if err := d.sys.addTap(v.lease, uid, gid); err != nil {
		return nil, err
	}
	chroot := d.chroot(v.id)
	if err := d.prepareChroot(chroot, uid, gid, spec.DiskGB, root, runner); err != nil {
		return nil, err
	}
	if v.log, err = control.OpenLogFile(dir); err != nil {
		return nil, err
	}
	console, w, err := makeConsole(filepath.Join(dir, consoleFIFO))
	if err != nil {
		return nil, err
	}
	v.console = console
	defer w.Close()
	// The services listen on the host end of the TAP, the gateway of the guest.
	service := fmt.Sprintf("http://%s:%d", v.lease.Host, d.opts.ServicePort)
	cfg := guest.NewConfig(spec, s, service, runnerDir, patched)
	if v.ctl, err = control.Listen(filepath.Join(chroot, controlSocket), &cfg, v, d.log.With("sandbox", v.id)); err != nil {
		return nil, err
	}

	cmd := d.jailerCommand(v.id, uid, gid)
	cmd.Stdout, cmd.Stderr = w, w
	// Its own session keeps the VM out of the signals of a terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the jailer: %w", err)
	}
	go func() {
		cmd.Wait()
		v.markExited()
	}()
	// The FIFO end is shared with Firecracker. Nonblocking, a full FIFO
	// drops console output instead of stopping the VM.
	unix.SetNonblock(int(w.Fd()), true)
	go v.tailConsole(console)

	v.api = newAPIClient(filepath.Join(chroot, apiSocket))
	if err := waitForSocket(ctx, filepath.Join(chroot, apiSocket), v.exited); err != nil {
		return cmd, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return cmd, v.api.boot(ctx, spec, v.lease, s.BandwidthMbps)
}

// waitForSocket waits until the Firecracker API socket takes connections.
func waitForSocket(ctx context.Context, path string, exited <-chan struct{}) error {
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-exited:
			return errors.New("firecracker exited before its API was ready")
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return errors.New("the Firecracker API did not start in 10 seconds")
		case <-tick.C:
		}
	}
}

// abort removes a VM that did not start.
func (d *Driver) abort(v *vm, cmd *exec.Cmd) {
	if cmd != nil {
		if !isClosed(v.exited) {
			if err := d.sys.killScope(unitName(v.id)); err != nil {
				d.log.Warn("kill the scope of a failed VM", "sandbox", v.id, "err", err)
			}
		}
		select {
		case <-v.exited:
		case <-time.After(10 * time.Second):
			d.log.Warn("the failed VM does not exit", "sandbox", v.id)
		}
		// The console holds the reason, for example a jailer error.
		select {
		case <-v.tailDone:
		case <-time.After(2 * time.Second):
		}
	}
	d.remove(v)
	close(v.ready)
}
