package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
)

// recover finds the VMs from before a restart. It connects again to the VMs
// whose scope still has a process, and removes the parts of all others:
// records, chroots, TAPs, and scopes.
func (d *Driver) recover() error {
	dir := filepath.Join(d.opts.DataDir, "sandboxes")
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read the sandbox folder: %w", err)
	}
	live := map[string]bool{}
	for _, e := range entries {
		id := e.Name()
		rec, err := readRecord(filepath.Join(dir, id))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue // not a VM
		case err != nil:
			d.log.Warn("remove a VM with a bad record", "sandbox", id, "err", err)
			d.kill(id)
		case !d.scopeAlive(id):
			d.log.Info("a VM stopped while runplane did not operate", "sandbox", id)
			d.freeTap(rec)
		default:
			if err := d.resume(rec); err != nil {
				d.log.Warn("remove a VM from before the restart", "sandbox", id, "err", err)
				d.kill(id)
				d.freeTap(rec)
				break
			}
			live[id] = true
			d.log.Info("took over sandbox", "sandbox", id, "state", rec.State)
			continue
		}
		os.RemoveAll(d.jailDir(id))
		os.RemoveAll(d.sandboxDir(id))
	}

	// Scopes and chroots without a record come from a start that runplane
	// did not complete.
	for _, id := range d.scopeIDs() {
		if !live[id] {
			d.kill(id)
		}
	}
	jails, _ := os.ReadDir(d.jails())
	for _, e := range jails {
		if !live[e.Name()] {
			os.RemoveAll(d.jailDir(e.Name()))
		}
	}
	return nil
}

// resume connects to a VM that operated before the restart.
func (d *Driver) resume(rec record) error {
	id := rec.Spec.ID
	v := newVM(d, rec.Spec, d.taps.Lease(rec.Tap), rec.CreatedAt)
	v.record = rec
	v.heard.Store(rec.State != sandbox.StateStarting)
	if err := d.sys.keepTap(v.lease); err != nil {
		return err
	}
	dir := d.sandboxDir(id)
	var err error
	if v.log, err = control.OpenLogFile(dir); err != nil {
		return err
	}
	if v.console, err = openConsole(filepath.Join(dir, consoleFIFO)); err != nil {
		d.log.Warn("no console for a VM from before the restart", "sandbox", id, "err", err)
		close(v.tailDone)
	} else {
		go v.tailConsole(v.console)
	}
	chroot := d.chroot(id)
	if v.ctl, err = control.Listen(filepath.Join(chroot, controlSocket), nil, v, d.log.With("sandbox", id)); err != nil {
		v.detach()
		return err
	}
	v.api = newAPIClient(filepath.Join(chroot, apiSocket))
	close(v.ready)
	d.mu.Lock()
	d.vms[id] = v
	d.mu.Unlock()
	d.spawn(func() { d.watchScope(v) })
	if v.State != sandbox.StateStopping {
		v.armTimer()
		return nil
	}
	// The restart came during a stop.
	go func() {
		if err := d.stop(context.Background(), v, "stopped"); err != nil {
			d.log.Warn("stop sandbox", "sandbox", id, "err", err)
		}
	}()
	return nil
}

func (d *Driver) kill(id string) {
	if !d.scopeAlive(id) {
		return
	}
	if err := d.sys.killScope(unitName(id)); err != nil {
		d.log.Warn("kill the scope of a lost VM", "sandbox", id, "err", err)
	}
}

func (d *Driver) freeTap(rec record) {
	if err := d.sys.dropTap(d.taps.Lease(rec.Tap)); err != nil {
		d.log.Warn("remove the TAP of a lost VM", "sandbox", rec.Spec.ID, "err", err)
	}
	d.taps.Release(rec.Tap)
}
