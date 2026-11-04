package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

const (
	// stopGrace is the time for runplane-init to stop the runner and
	// Docker, send the last logs, and power off.
	stopGrace = 30 * time.Second
	// killWait is the time for the kernel to end the VM after a kill.
	killWait = 10 * time.Second
)

// scopePoll is how often the driver checks that a VM from before a restart
// still operates. It is not the parent of such a VM, so it cannot wait.
var scopePoll = time.Second

// Stop asks runplane-init to power off, kills the VM after a grace time,
// and removes it. It returns when the VM is gone. A cancelled ctx skips the
// grace time.
func (d *Driver) Stop(ctx context.Context, id string) error {
	d.mu.Lock()
	v := d.vms[id]
	d.mu.Unlock()
	if v == nil {
		return nil
	}
	select {
	case <-v.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	if isClosed(v.done) {
		return nil
	}
	if err := d.stop(ctx, v, "stopped"); err != nil {
		return fmt.Errorf("stop sandbox %s: %w", id, err)
	}
	return nil
}

func (d *Driver) stop(ctx context.Context, v *vm, reason string) error {
	v.mu.Lock()
	if v.stopReason == "" {
		v.stopReason = reason
	}
	v.mu.Unlock()
	v.setState(sandbox.StateStopping, reason)

	if d.shutdown(ctx, v) {
		grace := time.NewTimer(stopGrace)
		defer grace.Stop()
		select {
		case <-v.exited:
		case <-grace.C:
		case <-ctx.Done():
		}
	}
	if !isClosed(v.exited) {
		v.mu.Lock()
		v.killed = true
		v.mu.Unlock()
		if err := d.sys.killScope(unitName(v.id)); err != nil {
			d.log.Warn("kill the VM scope", "sandbox", v.id, "err", err)
		}
		select {
		case <-v.exited:
		case <-time.After(killWait):
			return errors.New("the VM does not exit")
		}
	}
	select {
	case <-v.done:
		return nil
	case <-d.quit:
		return errors.New("the driver is closed")
	}
}

// shutdown asks the guest to power off: through runplane-init, or with
// Ctrl+Alt+Del when init does not answer. Init turns Ctrl+Alt+Del into a
// shutdown. On arm64, Firecracker sends a power key instead, which init
// does not handle. It reports if the guest got the request.
func (d *Driver) shutdown(ctx context.Context, v *vm) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := v.ctl.Shutdown(ctx)
	if err != nil && runtime.GOARCH == "amd64" {
		err = v.api.ctrlAltDel(ctx)
	}
	if err != nil {
		d.log.Debug("ask the VM to power off", "sandbox", v.id, "err", err)
	}
	return err == nil
}

// watch removes a VM that this driver started when its process ends.
func (d *Driver) watch(v *vm) {
	select {
	case <-v.exited:
		d.finish(v)
	case <-d.quit:
	}
}

// watchScope removes a VM from before a restart when its scope is empty.
func (d *Driver) watchScope(v *vm) {
	t := time.NewTicker(scopePoll)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if !d.scopeAlive(v.id) {
				v.markExited()
				d.finish(v)
				return
			}
		case <-d.quit:
			return
		}
	}
}

// finish removes a VM whose process ended and sends its last state.
func (d *Driver) finish(v *vm) {
	// The last console lines can explain a failure.
	select {
	case <-v.tailDone:
	case <-time.After(2 * time.Second):
	}
	state, reason := v.outcome()
	v.mu.Lock()
	killed := v.killed
	v.mu.Unlock()
	log := d.log.Info
	if state == sandbox.StateFailed {
		log = d.log.Warn
	}
	log("VM ended", "sandbox", v.id, "state", state, "reason", reason, "killed", killed)
	if err := d.remove(v); err != nil {
		d.log.Warn("remove sandbox", "sandbox", v.id, "err", err)
	}
	d.emit(sandbox.Event{SandboxID: v.id, Type: sandbox.EventState, State: state, Reason: reason})
}

// remove deletes all parts of a VM whose process ended: its connections,
// TAP, firewall rules, chroot with the scratch disk, and sandbox folder.
func (d *Driver) remove(v *vm) error {
	v.detach()
	errs := []error{d.sys.dropTap(v.lease)}
	d.taps.Release(v.lease.Index)
	errs = append(errs, os.RemoveAll(d.jailDir(v.id)), os.RemoveAll(d.sandboxDir(v.id)))
	d.mu.Lock()
	if d.vms[v.id] == v {
		delete(d.vms, v.id)
	}
	d.mu.Unlock()
	close(v.done)
	return errors.Join(errs...)
}
