package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
	"github.com/runplane/runplane/internal/sandbox/netrules"
)

// Files in the chroot. Firecracker sees them at its root.
const (
	kernelFile    = "vmlinux"
	rootFile      = "rootfs.ext4"
	runnerFile    = "runner.ext4"
	scratchFile   = "scratch.ext4"
	apiSocket     = "api.sock"
	vsockFile     = "v.sock"
	controlSocket = "v.sock_1024" // Firecracker forwards guest vsock port 1024 here
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
	runner, err := d.opts.RunnerDrive(ctx, spec.RunnerVersion, spec.Arch)
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

	cmd, err := d.launch(ctx, v, spec, settings, root, runner)
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
	case spec.Arch != d.arch:
		return fmt.Errorf("firecracker: sandbox arch %q on a %s server", spec.Arch, d.arch)
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
func (d *Driver) launch(ctx context.Context, v *vm, spec sandbox.Spec, s sandbox.Settings, root, runner string) (*exec.Cmd, error) {
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
	cfg := d.guestConfig(spec, v.lease, s)
	if v.ctl, err = control.Listen(filepath.Join(chroot, controlSocket), cfg, v, d.log.With("sandbox", v.id)); err != nil {
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
	m := machine{
		vcpu:          spec.VCPU,
		memoryMB:      spec.MemoryMB,
		tap:           v.lease.Tap,
		guest:         v.lease.Guest,
		host:          v.lease.Host,
		bandwidthMbps: s.BandwidthMbps,
	}
	if err := configure(ctx, v.api, m); err != nil {
		return cmd, err
	}
	return cmd, nil
}

// prepareChroot puts the files of the VM in the chroot. The read-only
// drives are hard links, so all VMs share one page cache for them. Only the
// VM with the IDs uid and gid can write the scratch disk.
func (d *Driver) prepareChroot(chroot string, uid, gid, diskGB int, root, runner string) error {
	if err := os.MkdirAll(chroot, 0o700); err != nil {
		return fmt.Errorf("create chroot: %w", err)
	}
	for _, f := range []struct{ name, src string }{
		{kernelFile, d.opts.Kernel},
		{rootFile, root},
		{runnerFile, runner},
	} {
		if err := linkFile(f.src, filepath.Join(chroot, f.name)); err != nil {
			return fmt.Errorf("put %s in the chroot: %w", f.name, err)
		}
	}
	disk := filepath.Join(chroot, scratchFile)
	if err := d.disks.Create(disk, diskGB); err != nil {
		return err
	}
	if err := os.Chown(disk, uid, gid); err != nil {
		return fmt.Errorf("give the scratch disk to the VM: %w", err)
	}
	return nil
}

// linkFile makes a hard link, or a copy when dst is on another file system.
func linkFile(src, dst string) error {
	err := os.Link(src, dst)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// guestConfig is what runplane-init gets on the control channel.
func (d *Driver) guestConfig(spec sandbox.Spec, lease netrules.Lease, s sandbox.Settings) control.GuestConfig {
	service := fmt.Sprintf("http://%s:%d", lease.Host, d.opts.ServicePort)
	env := map[string]string{}
	if s.ActionsCache && d.opts.CacheEnv != nil {
		maps.Copy(env, d.opts.CacheEnv(service))
	}
	maps.Copy(env, spec.Env)
	var mirror string
	if s.RegistryMirror {
		mirror = service
	}
	var dns []string
	for _, a := range ipv4DNS(s) {
		dns = append(dns, a.String())
	}
	return control.GuestConfig{
		SandboxID:        spec.ID,
		Hostname:         hostname(spec.ID),
		JITConfig:        spec.JITConfig,
		RunnerDir:        runnerDir,
		Env:              env,
		DNS:              dns,
		RegistryMirror:   mirror,
		TimeLimitSeconds: int(spec.TimeLimit / time.Second),
		Debug:            spec.Debug,
	}
}

// hostname uses the random end of the ULID, as the runner name and the
// container mode host name do. The start is the time, which sandboxes of
// the same second share.
func hostname(id string) string { return "runplane-" + id[max(len(id)-8, 0):] }

func unitName(id string) string { return "runplane-vm-" + id }

// jailerCommand starts the jailer in the systemd scope of the VM, so that
// the VM continues when runplane stops. Firecracker runs as uid and gid.
func (d *Driver) jailerCommand(id string, uid, gid int) *exec.Cmd {
	unit := unitName(id)
	args := []string{
		"--scope", "--quiet", "--collect",
		"--slice=" + slice,
		"--unit=" + unit,
		"--description=runplane sandbox " + id,
		// The metrics read memory.current of the scope.
		"--property=MemoryAccounting=yes",
	}
	if d.unit != "" {
		// When systemd stops the two, as at a host shutdown, it stops
		// runplane first. Close then powers the VM off cleanly, before
		// systemd kills what is left in the scope.
		args = append(args, "--property=Before="+d.unit)
	}
	args = append(args, "--",
		d.opts.Jailer,
		"--id", id,
		"--exec-file", d.opts.Firecracker,
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(gid),
		"--chroot-base-dir", d.chrootBase(),
		// With cgroup v2 and no --cgroup values, the jailer only moves
		// itself to this cgroup: the scope that it is in already.
		"--cgroup-version", "2",
		"--parent-cgroup", slice+"/"+unit+".scope",
		"--",
		"--api-sock", "/"+apiSocket,
	)
	return exec.Command("systemd-run", args...)
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

// withConsole adds the end of the console to a start error, because the
// sandbox folder is gone afterwards.
func (v *vm) withConsole(err error) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if end := v.consoleEnd(); end != "" {
		return fmt.Errorf("%w%s", err, end)
	}
	return err
}
