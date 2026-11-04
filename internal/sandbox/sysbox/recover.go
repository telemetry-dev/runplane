package sysbox

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
)

// recover takes over the sandbox containers that still operate after a
// runplane restart: it opens their control channels again, and the guests
// connect again within a second. It removes stopped containers and the
// folders, volumes, and probe containers that no sandbox uses.
func (d *Driver) recover(ctx context.Context) error {
	res, err := d.docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: d.owned(client.Filters{}.Add("label", sandbox.LabelSandbox)),
	})
	if err != nil {
		return fmt.Errorf("list sandbox containers: %w", err)
	}
	for _, c := range res.Items {
		id := c.Labels[sandbox.LabelSandbox]
		if c.State == container.StateRunning && idPattern.MatchString(id) {
			err := d.adopt(id, c.ID)
			if err == nil {
				d.bindAdopted(ctx, id, c.ID)
				continue
			}
			d.log.Warn("cannot take over sandbox, removing it", "sandbox", id, "err", err)
		}
		d.removeOrphan(ctx, c.ID, id)
	}

	entries, err := os.ReadDir(d.opts.Config.SandboxesDir())
	if err != nil {
		return fmt.Errorf("read sandboxes folder: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() && idPattern.MatchString(e.Name()) && d.get(e.Name()) == nil {
			d.removeFiles(ctx, e.Name())
		}
	}

	if d.opts.Scratch == ScratchVolume {
		vols, err := d.docker.VolumeList(ctx, client.VolumeListOptions{
			Filters: d.owned(client.Filters{}.Add("label", sandbox.LabelSandbox)),
		})
		if err != nil {
			return fmt.Errorf("list scratch volumes: %w", err)
		}
		for _, v := range vols.Items {
			if d.get(v.Labels[sandbox.LabelSandbox]) == nil {
				d.docker.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true})
			}
		}
	}

	probes, err := d.docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: d.owned(client.Filters{}.Add("label", labelProbe)),
	})
	if err != nil {
		return fmt.Errorf("list probe containers: %w", err)
	}
	for _, c := range probes.Items {
		d.docker.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true})
	}
	return nil
}

// bindAdopted binds the bridge port of a sandbox that operates since before
// the restart. The bridge table usually has the binding already; a runplane
// from before the bridge table has none, and its sandboxes then have no
// network until the binding.
func (d *Driver) bindAdopted(ctx context.Context, id, containerID string) {
	b := d.get(id)
	info, err := d.docker.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err == nil {
		err = d.bindPort(b, info.Container)
	}
	if err != nil {
		d.log.Warn("bind the bridge port of a sandbox", "sandbox", id, "err", err)
	}
}

// adopt takes over one running sandbox container.
func (d *Driver) adopt(id, containerID string) error {
	dir := d.opts.Config.SandboxDir(id)
	m, err := readMeta(dir)
	if err != nil {
		return err
	}
	if m.ContainerID != containerID {
		return errors.New("the sandbox folder belongs to a different container")
	}
	b := &box{
		id:          id,
		dir:         dir,
		created:     m.CreatedAt,
		spec:        m.Spec,
		removed:     make(chan struct{}),
		containerID: containerID,
		ip:          m.IP,
		state:       m.State,
		holdUntil:   m.HoldUntil,
		busyAt:      m.BusyAt,
		// A running sandbox in the stopping state was in a Stop call.
		stopAsked: m.State == sandbox.StateStopping,
	}
	if b.logs, err = control.OpenLogFile(dir); err != nil {
		return err
	}
	// The guest asked for its config before; it asks only one time.
	if b.ctl, err = d.listen(b, b.logs, control.GuestConfig{SandboxID: id}); err != nil {
		b.logs.Close()
		return err
	}
	d.mu.Lock()
	d.boxes[id] = b
	d.mu.Unlock()
	d.log.Info("took over sandbox", "sandbox", id, "state", b.state)
	if b.state == sandbox.StateStopping {
		go d.stop(context.Background(), b)
	}
	return nil
}
