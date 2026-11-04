package sysbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/scratch"
)

// Stop stops the container and removes the sandbox. It is idempotent.
func (d *Driver) Stop(ctx context.Context, id string) error {
	b := d.get(id)
	if b == nil {
		return nil
	}
	d.mu.Lock()
	b.stopAsked = true
	d.mu.Unlock()
	d.stop(ctx, b)
	return ctx.Err()
}

// stop asks the container to stop with SIGTERM, which runplane-init takes as
// a shutdown, and removes the sandbox. It waits for the removal or ctx.
func (d *Driver) stop(ctx context.Context, b *box) {
	d.setState(b, sandbox.StateStopping)
	go func() {
		id := d.containerID(b)
		if id == "" {
			// Start still makes this sandbox: wait for it.
			d.startMu.Lock()
			d.startMu.Unlock()
			id = d.containerID(b)
		}
		if id != "" {
			timeout := int(stopTimeout / time.Second)
			stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout+30*time.Second)
			_, err := d.docker.ContainerStop(stopCtx, id, client.ContainerStopOptions{Timeout: &timeout})
			cancel()
			if err != nil && !cerrdefs.IsNotFound(err) {
				d.log.Warn("stop container", "sandbox", b.id, "err", err)
			}
		}
		d.remove(b)
	}()
	select {
	case <-b.removed:
	case <-ctx.Done():
	}
}

// remove takes a sandbox down: the container, the control channel, the
// scratch disk or volume, and the sandbox folder. Then it sends the last
// state. It runs one time for each sandbox; other calls wait for it.
func (d *Driver) remove(b *box) {
	b.once.Do(func() {
		d.setState(b, sandbox.StateStopping)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		state, reason := d.outcome(ctx, b)
		d.cleanup(ctx, b)
		d.mu.Lock()
		delete(d.boxes, b.id)
		b.state = state
		d.emitState(b.id, state, reason)
		d.mu.Unlock()
		if state == sandbox.StateFailed {
			d.log.Warn("sandbox failed", "sandbox", b.id, "reason", reason)
		} else {
			d.log.Info("sandbox removed", "sandbox", b.id)
		}
		close(b.removed)
	})
	<-b.removed
}

// outcome tells if the sandbox ended well (gone) or not (failed), and why.
func (d *Driver) outcome(ctx context.Context, b *box) (sandbox.State, string) {
	d.mu.Lock()
	failure, stopAsked, id := b.failure, b.stopAsked, b.containerID
	d.mu.Unlock()
	switch {
	case failure != "":
		return sandbox.StateFailed, failure
	case stopAsked:
		return sandbox.StateGone, "stopped by runplane"
	case id == "":
		return sandbox.StateGone, ""
	}
	res, err := d.docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return sandbox.StateFailed, "the container was removed outside of runplane"
	}
	if err != nil {
		return sandbox.StateFailed, fmt.Sprintf("inspect container: %v", err)
	}
	st := res.Container.State
	switch {
	case st == nil:
		return sandbox.StateFailed, "the container has no state"
	case st.OOMKilled:
		return sandbox.StateFailed, "out of memory"
	case st.Running:
		return sandbox.StateFailed, "the container still operated"
	case st.ExitCode != 0:
		return sandbox.StateFailed, fmt.Sprintf("runplane-init exited with code %d", st.ExitCode)
	}
	return sandbox.StateGone, ""
}

func (d *Driver) cleanup(ctx context.Context, b *box) {
	if id := d.containerID(b); id != "" {
		_, err := d.docker.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		if err != nil && !cerrdefs.IsNotFound(err) {
			d.log.Warn("remove container", "sandbox", b.id, "err", err)
		}
	}
	d.mu.Lock()
	b.closeChannel()
	port := b.port
	d.mu.Unlock()
	if port != "" {
		if err := d.opts.NetRules.RemovePort(port); err != nil {
			d.log.Warn("remove bridge port binding", "sandbox", b.id, "port", port, "err", err)
		}
	}
	d.removeFiles(ctx, b.id)
}

// closeChannel closes the control channel and the log. Call it with
// Driver.mu held.
func (b *box) closeChannel() {
	if b.ctl != nil {
		b.ctl.Close()
	}
	if b.logs != nil {
		b.logs.Close()
	}
}

// removeFiles removes the scratch disk or volume and the folder of a sandbox.
func (d *Driver) removeFiles(ctx context.Context, id string) {
	dir := d.opts.Config.SandboxDir(id)
	switch d.opts.Scratch {
	case ScratchLoop:
		// With the disk still mounted, RemoveAll would erase the disk file
		// by file. The next sweep or start tries again.
		if err := scratch.Detach(filepath.Join(dir, "scratch")); err != nil {
			d.log.Warn("detach scratch disk", "sandbox", id, "err", err)
			return
		}
	case ScratchVolume:
		_, err := d.docker.VolumeRemove(ctx, volumeName(id), client.VolumeRemoveOptions{Force: true})
		if err != nil && !cerrdefs.IsNotFound(err) {
			d.log.Warn("remove scratch volume", "sandbox", id, "err", err)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		d.log.Warn("remove sandbox folder", "sandbox", id, "err", err)
	}
}
