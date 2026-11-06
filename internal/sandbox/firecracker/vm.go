package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/sandbox/control"
	"github.com/runplane/runplane/internal/sandbox/guest"
	"github.com/runplane/runplane/internal/sandbox/lifecycle"
	"github.com/runplane/runplane/internal/sandbox/netrules"
)

// consoleFIFO in the sandbox folder takes the stdout and stderr of
// Firecracker: the serial console of the guest and the Firecracker log. A
// FIFO, unlike a file, does not grow when a job writes to the console
// without end, and Firecracker only gets write errors while runplane
// restarts.
const consoleFIFO = "console.fifo"

// scopePoll is how often the driver checks that a VM from before a restart
// still operates. It is not the parent of such a VM, so it cannot wait.
var scopePoll = time.Second

// vm is the VM part of a sandbox. Lease is in the record of the sandbox.
type vm struct {
	Lease netrules.Lease `json:"lease"`

	api      *apiClient
	console  *os.File      // read end of the console FIFO
	tailDone chan struct{} // closed when the console tail ends
}

type box = lifecycle.Box[vm]

type backend struct{ *Driver }

func (d backend) Shutdown(ctx context.Context, b *box) error {
	// Init turns Ctrl+Alt+Del into a shutdown. On arm64, Firecracker sends
	// a power key instead, which init does not handle.
	if runtime.GOARCH != "amd64" {
		return errors.New("firecracker sends no Ctrl+Alt+Del on " + runtime.GOARCH)
	}
	return b.Host.api.ctrlAltDel(ctx)
}

func (d backend) Kill(_ context.Context, b *box) error { return d.sys.killScope(unitName(b.ID)) }

// Fault waits for the last console lines, which can explain a failure.
func (d backend) Fault(_ context.Context, b *box) string {
	if done := b.Host.tailDone; done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	return ""
}

func (d backend) Remove(_ context.Context, b *box) error {
	v := b.Host
	v.closeConsole()
	var errs []error
	if d.scopeAlive(b.ID) {
		errs = append(errs, d.sys.killScope(unitName(b.ID)))
	}
	if v.Lease.Tap != "" {
		errs = append(errs, d.sys.closeTap(v.Lease))
	}
	return errors.Join(append(errs, os.RemoveAll(d.jailDir(b.ID)))...)
}

func (d backend) Usage(_ context.Context, b *box) (lifecycle.Usage, error) {
	u, err := readUsage(d.scopeDir(b.ID))
	u.At = time.Now()
	return u, err
}

// Adopt connects to a VM from before the restart whose scope still has a
// process.
func (d backend) Adopt(_ context.Context, b *box) (bool, error) {
	if !d.scopeAlive(b.ID) {
		return false, nil
	}
	v := b.Host
	if err := d.sys.keepTap(v.Lease); err != nil {
		return true, err
	}
	if console, err := openConsole(filepath.Join(b.Dir, consoleFIFO)); err != nil {
		b.Log().Warn("no console for a VM from before the restart", "err", err)
	} else {
		v.console, v.tailDone = console, make(chan struct{})
		go v.tailConsole(b)
	}
	chroot := d.chroot(b.ID)
	srv, err := control.Listen(filepath.Join(chroot, controlSocket), nil, b, b.Log())
	if err != nil {
		return true, err
	}
	b.SetControl(srv)
	v.api = newAPIClient(filepath.Join(chroot, apiSocket))
	d.Go(func(ctx context.Context) { d.watchScope(ctx, b) })
	return true, nil
}

// watchScope marks a VM from before a restart as ended when its scope is
// empty.
func (d *Driver) watchScope(ctx context.Context, b *box) {
	t := time.NewTicker(scopePoll)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if !d.scopeAlive(b.ID) {
				b.MarkExited()
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// recover takes over the VMs from before a restart, and removes the scopes
// and chroots of the VMs that have no sandbox.
func (d *Driver) recover(ctx context.Context) error {
	taken, err := d.Recover(ctx)
	if err != nil {
		return err
	}
	// They come from a start that runplane did not complete.
	for _, id := range d.scopeIDs() {
		if !taken[id] && d.scopeAlive(id) {
			if err := d.sys.killScope(unitName(id)); err != nil {
				d.log.Warn("kill the scope of a lost VM", "sandbox", id, "err", err)
			}
		}
	}
	jails, err := os.ReadDir(d.jails())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		d.log.Warn("read the chroots", "err", err)
	}
	for _, e := range jails {
		if !taken[e.Name()] {
			if err := os.RemoveAll(d.jailDir(e.Name())); err != nil {
				d.log.Warn("remove the chroot of a lost VM", "sandbox", e.Name(), "err", err)
			}
		}
	}
	return nil
}

// tailConsole copies the console lines to the sandbox until Firecracker
// closes its end.
func (v *vm) tailConsole(b *box) {
	defer close(v.tailDone)
	guest.ReadLines(v.console, b.Console)
}

func (v *vm) closeConsole() {
	if v.console != nil {
		v.console.Close()
	}
}

func makeConsole(path string) (r, w *os.File, err error) {
	os.Remove(path)
	if err := unix.Mkfifo(path, 0o600); err != nil {
		return nil, nil, fmt.Errorf("make console FIFO: %w", err)
	}
	// The read end opens first, so the write end does not wait.
	if r, err = openConsole(path); err != nil {
		return nil, nil, err
	}
	if w, err = os.OpenFile(path, os.O_WRONLY, 0); err != nil {
		r.Close()
		return nil, nil, fmt.Errorf("open console FIFO: %w", err)
	}
	return r, w, nil
}

// openConsole opens the read end of the console FIFO, also again after a
// restart.
func openConsole(path string) (*os.File, error) {
	r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open console FIFO: %w", err)
	}
	return r, nil
}

var _ lifecycle.Backend[vm] = backend{}
