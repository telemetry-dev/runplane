package sysbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox/scratch"
)

// Shutdown sends SIGTERM, which runplane-init takes as a shutdown.
func (d backend) Shutdown(ctx context.Context, b *box) error {
	_, err := d.docker.ContainerKill(ctx, containerName(b.ID), client.ContainerKillOptions{Signal: "SIGTERM"})
	return err
}

func (d backend) Kill(ctx context.Context, b *box) error {
	_, err := d.docker.ContainerKill(ctx, containerName(b.ID), client.ContainerKillOptions{Signal: "SIGKILL"})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// Fault reads why the container ended.
func (d backend) Fault(ctx context.Context, b *box) string {
	res, err := d.docker.ContainerInspect(ctx, containerName(b.ID), client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return "the container was removed outside of runplane"
	}
	if err != nil {
		return fmt.Sprintf("inspect container: %v", err)
	}
	st := res.Container.State
	switch {
	case st == nil:
		return "the container has no state"
	case st.OOMKilled:
		return "out of memory"
	case st.Running:
		return "the container still operated"
	case st.ExitCode != 0:
		return fmt.Sprintf("runplane-init exited with code %d", st.ExitCode)
	}
	return ""
}

// Remove removes the container, the binding of its bridge port, and the
// scratch disk or volume.
func (d backend) Remove(ctx context.Context, b *box) error {
	var errs []error
	_, err := d.docker.ContainerRemove(ctx, containerName(b.ID), client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("remove container: %w", err))
	}
	if _, port := b.Host.get(); port != "" {
		if err := d.opts.NetRules.RemovePort(port); err != nil {
			errs = append(errs, fmt.Errorf("remove the binding of bridge port %s: %w", port, err))
		}
	}
	if d.opts.Profile.loopScratch {
		// With the disk still mounted, a removal of the sandbox folder
		// would erase the disk file by file.
		if err := scratch.Detach(filepath.Join(b.Dir, "scratch")); err != nil {
			errs = append(errs, fmt.Errorf("detach scratch disk: %w", err))
		}
	} else {
		_, err := d.docker.VolumeRemove(ctx, volumeName(b.ID), client.VolumeRemoveOptions{Force: true})
		if err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove scratch volume: %w", err))
		}
	}
	return errors.Join(errs...)
}
