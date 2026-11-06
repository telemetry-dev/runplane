package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// runnerDir is where runplane-init mounts the runner drive in the guest.
const runnerDir = "/opt/runplane/runner"

// maxSocketPath is the longest Unix socket path that Linux takes.
const maxSocketPath = 107

// Create returns when the VM boots; runplane-init then connects to the
// control channel.
func (d backend) Create(ctx context.Context, b *box, spec sandbox.Spec, s sandbox.Settings) error {
	chroot := d.chroot(b.ID)
	if p := filepath.Join(chroot, controlSocket); len(p) > maxSocketPath {
		return fmt.Errorf("the socket path %s is longer than %d bytes; use a shorter data folder", p, maxSocketPath)
	}
	root, err := d.opts.RootDisk(spec.Image.Digest)
	if err != nil {
		return err
	}
	runner, patched, err := d.opts.RunnerDrive(ctx, spec.RunnerVersion, spec.Arch)
	if err != nil {
		return err
	}
	v := b.Host
	if v.Lease, err = d.sys.acquireTap(); err != nil {
		return err
	}
	// The record has the lease before the TAP exists, so a restart in the
	// middle finds and removes the TAP.
	if err := b.SetIP(v.Lease.Guest.String()); err != nil {
		return err
	}
	uid, gid, err := d.ids.of(v.Lease.Index)
	if err != nil {
		return err
	}
	if err := d.sys.openTap(v.Lease, uid, gid); err != nil {
		return err
	}
	if err := d.prepareChroot(chroot, uid, gid, spec.DiskGB, root, runner); err != nil {
		return err
	}
	console, w, err := makeConsole(filepath.Join(b.Dir, consoleFIFO))
	if err != nil {
		return err
	}
	v.console = console
	defer w.Close()
	// The services listen on the host end of the TAP, the gateway of the guest.
	service := fmt.Sprintf("http://%s:%d", v.Lease.Host, d.opts.ServicePort)
	cfg := guest.NewConfig(spec, s, service, runnerDir, patched)
	srv, err := control.Listen(filepath.Join(chroot, controlSocket), &cfg, b, b.Log())
	if err != nil {
		return err
	}
	b.SetControl(srv)

	cmd := d.jailerCommand(b.ID, uid, gid)
	cmd.Stdout, cmd.Stderr = w, w
	// Its own session keeps the VM out of the signals of a terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the jailer: %w", err)
	}
	go func() {
		cmd.Wait()
		b.MarkExited()
	}()
	// The FIFO end is shared with Firecracker. Nonblocking, a full FIFO
	// drops console output instead of stopping the VM.
	unix.SetNonblock(int(w.Fd()), true)
	v.tailDone = make(chan struct{})
	go v.tailConsole(b)

	v.api = newAPIClient(filepath.Join(chroot, apiSocket))
	err = waitForSocket(ctx, filepath.Join(chroot, apiSocket), b.Exited())
	if err == nil {
		bootCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = v.api.boot(bootCtx, spec, v.Lease, s.BandwidthMbps)
		cancel()
	}
	if err != nil {
		d.abort(b)
	}
	return err
}

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

// abort ends the jailer of a VM that did not start, and waits for the end of
// its console, which holds the reason, for example a jailer error.
func (d *Driver) abort(b *box) {
	if !isClosed(b.Exited()) {
		if err := d.sys.killScope(unitName(b.ID)); err != nil {
			b.Log().Warn("kill the scope of a failed VM", "err", err)
		}
	}
	select {
	case <-b.Exited():
	case <-time.After(10 * time.Second):
		b.Log().Warn("the failed VM does not exit")
	}
	select {
	case <-b.Host.tailDone:
	case <-time.After(2 * time.Second):
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
