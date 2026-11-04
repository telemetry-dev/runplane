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
	metricsEvery  = 5 * time.Second
	sweepEvery    = 30 * time.Second
	watchdogEvery = 10 * time.Second
	// diskEvery is how often the driver measures the writes of each
	// sandbox. Docker walks the writable layer of the container for it.
	diskEvery = time.Minute
	// limitGrace is the time that runplane-init gets to end a sandbox at its
	// time limit before the driver removes it.
	limitGrace = 5 * time.Minute
)

// emit queues an event. The queue has no limit, so a slow reader of Events
// never blocks a sandbox.
func (d *Driver) emit(ev sandbox.Event) {
	d.qmu.Lock()
	d.queue = append(d.queue, ev)
	d.qmu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *Driver) emitState(id string, st sandbox.State, reason string) {
	d.emit(sandbox.Event{SandboxID: id, Type: sandbox.EventState, Time: time.Now().UTC(), State: st, Reason: reason})
}

// pump moves the queue to the Events channel until Close.
func (d *Driver) pump() {
	defer close(d.events)
	for {
		d.qmu.Lock()
		batch := d.queue
		d.queue = nil
		d.qmu.Unlock()
		for _, ev := range batch {
			select {
			case d.events <- ev:
			case <-d.done:
				return
			}
		}
		select {
		case <-d.wake:
		case <-d.done:
			return
		}
	}
}

// watchDocker follows the Docker events of sandbox containers. After each
// (re)connect it sweeps, so an exit while no stream was open is not lost.
func (d *Driver) watchDocker() {
	defer d.loops.Done()
	for d.ctx.Err() == nil {
		ctx, cancel := context.WithCancel(d.ctx)
		res := d.docker.Events(ctx, client.EventsListOptions{
			Filters: d.owned(client.Filters{}.
				Add("type", string(events.ContainerEventType)).
				Add("label", sandbox.LabelSandbox).
				Add("event", string(events.ActionDie), string(events.ActionDestroy))),
		})
		d.sweep(ctx)
		d.readEvents(ctx, res)
		cancel()
		select {
		case <-time.After(time.Second):
		case <-d.ctx.Done():
		}
	}
}

func (d *Driver) readEvents(ctx context.Context, res client.EventsResult) {
	for {
		select {
		case msg := <-res.Messages:
			b := d.get(msg.Actor.Attributes[sandbox.LabelSandbox])
			if b != nil && d.containerID(b) == msg.Actor.ID {
				go d.remove(b)
			}
		case err := <-res.Err:
			if err != nil && ctx.Err() == nil {
				d.log.Warn("Docker event stream ended, connecting again", "err", err)
			}
			return
		}
	}
}

// tick runs the periodic work: metrics of busy sandboxes, the sweep, and
// the sysbox watchdog.
func (d *Driver) tick() {
	defer d.loops.Done()
	metrics := time.NewTicker(metricsEvery)
	sweep := time.NewTicker(sweepEvery)
	dog := time.NewTicker(watchdogEvery)
	disks := time.NewTicker(diskEvery)
	defer metrics.Stop()
	defer sweep.Stop()
	defer dog.Stop()
	defer disks.Stop()
	for {
		select {
		case <-metrics.C:
			d.sampleMetrics(d.ctx)
		case <-sweep.C:
			d.sweep(d.ctx)
		case <-dog.C:
			if d.dog != nil {
				d.checkSysboxFS()
			}
		case <-disks.C:
			d.checkDisks(d.ctx)
		case <-d.ctx.Done():
			return
		}
	}
}

// sweep compares the sandboxes with the containers of Docker. It removes
// sandboxes whose container stopped or went away, the containers of
// sandboxes that runplane no longer knows, and sandboxes past their time
// limit.
func (d *Driver) sweep(ctx context.Context) {
	// Take the sandboxes before the list, so that a container that Start
	// makes during the list does not look gone.
	d.mu.Lock()
	known := map[*box]string{}
	for _, b := range d.boxes {
		if b.containerID != "" {
			known[b] = b.containerID
		}
	}
	d.mu.Unlock()
	res, err := d.docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: d.owned(client.Filters{}.Add("label", sandbox.LabelSandbox)),
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
		id := c.Labels[sandbox.LabelSandbox]
		if b := d.get(id); b != nil && d.containerID(b) == c.ID {
			go d.remove(b)
		} else {
			d.removeOrphan(ctx, c.ID, id)
		}
	}
	now := time.Now()
	for b, id := range known {
		if !seen[id] {
			go d.remove(b)
			continue
		}
		d.mu.Lock()
		late := pastLimit(b, now) && b.failure == ""
		if late {
			b.failure = "the sandbox passed its time limit"
		}
		d.mu.Unlock()
		if late {
			go d.stop(context.Background(), b)
		}
	}
}

// checkDisks stops each sandbox whose writes pass its disk size: the
// writable layer of its container, which is on the host disk, and its
// scratch disk or volume. runplane-init puts most writes on scratch; the
// rest, such as apt installs, go to the container root, which has no limit
// of its own.
func (d *Driver) checkDisks(ctx context.Context) {
	d.mu.Lock()
	var list []*box
	for _, b := range d.boxes {
		if b.containerID != "" && b.spec.DiskGB > 0 && (b.state == sandbox.StateIdle || b.state == sandbox.StateBusy) {
			list = append(list, b)
		}
	}
	d.mu.Unlock()
	for _, b := range list {
		used, err := d.diskUse(ctx, b)
		if err != nil {
			if ctx.Err() == nil {
				d.log.Warn("measure the disk use of a sandbox", "sandbox", b.id, "err", err)
			}
			continue
		}
		if !overDisk(used, b.spec.DiskGB) {
			continue
		}
		d.mu.Lock()
		if b.failure == "" {
			b.failure = fmt.Sprintf("the sandbox wrote more than its disk size of %d GB", b.spec.DiskGB)
		}
		d.mu.Unlock()
		d.log.Warn("sandbox passed its disk size", "sandbox", b.id, "bytes", used, "disk_gb", b.spec.DiskGB)
		go d.stop(context.Background(), b)
	}
}

// overDisk tells if used bytes pass a disk size of diskGB.
func overDisk(used int64, diskGB int) bool { return used > int64(diskGB)<<30 }

// diskUse returns the bytes that a sandbox wrote: the writable layer of its
// container and its scratch disk or volume.
func (d *Driver) diskUse(ctx context.Context, b *box) (int64, error) {
	res, err := d.docker.ContainerInspect(ctx, d.containerID(b), client.ContainerInspectOptions{Size: true})
	if err != nil {
		return 0, err
	}
	var n int64
	if res.Container.SizeRw != nil {
		n = *res.Container.SizeRw
	}
	var scratchBytes int64
	switch d.opts.Scratch {
	case ScratchLoop:
		scratchBytes, err = scratch.Used(filepath.Join(b.dir, "scratch"))
	case ScratchVolume:
		var v client.VolumeInspectResult
		if v, err = d.docker.VolumeInspect(ctx, volumeName(b.id), client.VolumeInspectOptions{}); err == nil {
			scratchBytes, err = scratch.Usage(v.Volume.Mountpoint)
		}
	}
	return n + scratchBytes, err
}

// pastLimit reports if a sandbox operates longer than its time limit, or
// than its debug hold. runplane-init ends the job at the limit; this is the
// fallback when init does not.
func pastLimit(b *box, now time.Time) bool {
	if b.spec.TimeLimit <= 0 {
		return false
	}
	end := sandbox.Deadline(b.created, b.busyAt, b.spec.TimeLimit)
	if b.holdUntil.After(end) {
		end = b.holdUntil
	}
	return now.After(end.Add(limitGrace))
}

// removeOrphan removes a stopped container with the sandbox label that no
// sandbox of this driver owns, and the files of its sandbox.
func (d *Driver) removeOrphan(ctx context.Context, containerID, id string) {
	_, err := d.docker.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil {
		d.log.Warn("remove old sandbox container", "container", containerID, "err", err)
		return
	}
	d.log.Info("removed old sandbox container", "sandbox", id, "container", containerID)
	if idPattern.MatchString(id) && d.get(id) == nil {
		d.removeFiles(ctx, id)
	}
}
