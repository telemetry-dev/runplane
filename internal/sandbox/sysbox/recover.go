package sysbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox/lifecycle"
)

// Adopt connects to a sandbox container that still operates after a runplane
// restart. Its guest connects again within a second.
func (d backend) Adopt(ctx context.Context, b *box) (bool, error) {
	res, err := d.docker.ContainerInspect(ctx, containerName(b.ID), client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect container: %w", err)
	}
	info := res.Container
	if info.State == nil || !info.State.Running {
		return false, nil
	}
	b.Host.mu.Lock()
	b.Host.id = info.ID
	b.Host.mu.Unlock()
	ctl, err := d.listen(b, nil)
	if err != nil {
		return true, err
	}
	b.SetControl(ctl)
	// The bridge table usually has the binding of the port already.
	if err := d.bindPort(b, info); err != nil {
		b.Log().Warn("bind the bridge port of a sandbox", "err", err)
	}
	return true, nil
}

// recover takes over the sandboxes that still operate after a runplane
// restart. It removes the containers and volumes of sandboxes that have no
// folder, and the probe containers.
func (d *Driver) recover(ctx context.Context) error {
	if _, err := d.Recover(ctx); err != nil {
		return err
	}
	// The folder of a sandbox of another mode stays for its driver.
	hasFolder := func(id string) bool {
		if !lifecycle.ValidID(id) {
			return false
		}
		_, err := os.Stat(filepath.Join(d.opts.Sandboxes, id))
		return err == nil
	}
	res, err := d.docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: d.owned(client.Filters{}.Add("label", LabelSandbox)),
	})
	if err != nil {
		return fmt.Errorf("list sandbox containers: %w", err)
	}
	for _, c := range res.Items {
		if !hasFolder(c.Labels[LabelSandbox]) {
			d.removeOrphan(ctx, c.ID)
		}
	}
	if !d.opts.Profile.loopScratch {
		vols, err := d.docker.VolumeList(ctx, client.VolumeListOptions{
			Filters: d.owned(client.Filters{}.Add("label", LabelSandbox)),
		})
		if err != nil {
			return fmt.Errorf("list scratch volumes: %w", err)
		}
		for _, v := range vols.Items {
			if hasFolder(v.Labels[LabelSandbox]) {
				continue
			}
			if _, err := d.docker.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true}); err != nil {
				d.log.Warn("remove old scratch volume", "volume", v.Name, "err", err)
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
		d.removeOrphan(ctx, c.ID)
	}
	return nil
}

// removeOrphan removes a container of this driver that no sandbox owns.
func (d *Driver) removeOrphan(ctx context.Context, containerID string) {
	_, err := d.docker.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		d.log.Warn("remove old sandbox container", "container", containerID, "err", err)
		return
	}
	d.log.Info("removed old sandbox container", "container", containerID)
}
