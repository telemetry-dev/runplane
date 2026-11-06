package sysbox

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/scratch"
)

const (
	sweepEvery    = 30 * time.Second
	watchdogEvery = 10 * time.Second
	// diskEvery is how often the driver measures the writes of each
	// sandbox. Docker walks the writable layer of the container for it.
	diskEvery = time.Minute
)

// watchDocker follows the Docker events of sandbox containers. After each
// (re)connect it sweeps, so an exit while no stream was open is not lost.
func (d *Driver) watchDocker(ctx context.Context) {
	for ctx.Err() == nil {
		streamCtx, cancel := context.WithCancel(ctx)
		res := d.docker.Events(streamCtx, client.EventsListOptions{
			Filters: d.owned(client.Filters{}.
				Add("type", string(events.ContainerEventType)).
				Add("label", LabelSandbox).
				Add("event", string(events.ActionDie), string(events.ActionDestroy))),
		})
		d.sweep(streamCtx)
		d.readEvents(streamCtx, res)
		cancel()
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
		}
	}
}

func (d *Driver) readEvents(ctx context.Context, res client.EventsResult) {
	for {
		select {
		case msg := <-res.Messages:
			if b := d.byContainer(msg.Actor.ID); b != nil {
				b.MarkExited()
			}
		case err := <-res.Err:
			if err != nil && ctx.Err() == nil {
				d.log.Warn("Docker event stream ended, connecting again", "err", err)
			}
			return
		}
	}
}

// tick runs the sweep, the sysbox watchdog, and the disk check.
func (d *Driver) tick(ctx context.Context) {
	sweep := time.NewTicker(sweepEvery)
	dog := time.NewTicker(watchdogEvery)
	disks := time.NewTicker(diskEvery)
	defer sweep.Stop()
	defer dog.Stop()
	defer disks.Stop()
	for {
		select {
		case <-sweep.C:
			d.sweep(ctx)
		case <-dog.C:
			if d.dog != nil {
				d.checkSysboxFS()
			}
		case <-disks.C:
			d.checkDisks(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// sweep compares the sandboxes with the containers of Docker. It ends the
// sandboxes whose container stopped or went away, and removes the stopped
// containers that no sandbox owns.
func (d *Driver) sweep(ctx context.Context) {
	// Take the sandboxes before the list, so that a container that Start
	// makes during the list does not look gone.
	known := map[*box]string{}
	for _, b := range d.Boxes() {
		if id, _ := b.Host.get(); id != "" {
			known[b] = id
		}
	}
	res, err := d.docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: d.owned(client.Filters{}.Add("label", LabelSandbox)),
	})
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("list sandbox containers", "err", err)
		}
		return
	}
	seen := map[string]bool{}
	for _, c := range res.Items {
		seen[c.ID] = true
		if c.State != container.StateExited && c.State != container.StateDead {
			continue
		}
		if b := d.byContainer(c.ID); b != nil {
			b.MarkExited()
		} else {
			d.removeOrphan(ctx, c.ID)
		}
	}
	for b, id := range known {
		if !seen[id] {
			b.MarkExited()
		}
	}
}

// checkDisks stops each sandbox whose writes pass its disk size: the
// writable layer of its container, which is on the host disk, and its
// scratch disk or volume. runplane-init puts most writes on scratch; the
// rest, such as apt installs, go to the container root, which has no limit
// of its own.
func (d *Driver) checkDisks(ctx context.Context) {
	for _, b := range d.Boxes() {
		id, _ := b.Host.get()
		st, size := b.State(), b.Spec().DiskGB
		if id == "" || size <= 0 || (st != sandbox.StateIdle && st != sandbox.StateBusy) {
			continue
		}
		used, err := d.diskUse(ctx, b, id)
		if err != nil {
			if ctx.Err() == nil {
				b.Log().Warn("measure the disk use of a sandbox", "err", err)
			}
			continue
		}
		if used > int64(size)<<30 {
			b.Fail(fmt.Sprintf("the sandbox wrote more than its disk size of %d GB", size))
		}
	}
}

func (d *Driver) diskUse(ctx context.Context, b *box, containerID string) (int64, error) {
	res, err := d.docker.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{Size: true})
	if err != nil {
		return 0, err
	}
	var n int64
	if res.Container.SizeRw != nil {
		n = *res.Container.SizeRw
	}
	var scratchBytes int64
	if d.opts.Profile.loopScratch {
		scratchBytes, err = scratch.Used(filepath.Join(b.Dir, "scratch"))
	} else {
		var v client.VolumeInspectResult
		if v, err = d.docker.VolumeInspect(ctx, volumeName(b.ID), client.VolumeInspectOptions{}); err == nil {
			scratchBytes, err = scratch.Usage(v.Volume.Mountpoint)
		}
	}
	return n + scratchBytes, err
}
