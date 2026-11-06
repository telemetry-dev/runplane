package sysbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
	"github.com/runplane/runplane/internal/sandbox/guest"
	"github.com/runplane/runplane/internal/sandbox/lifecycle"
	"github.com/runplane/runplane/internal/sandbox/netrules"
	"github.com/runplane/runplane/internal/sandbox/scratch"
)

// box is a sandbox of this driver.
type box = lifecycle.Box[instance]

// backend is the lifecycle.Backend of the driver.
type backend struct{ *Driver }

var _ lifecycle.Backend[instance] = backend{}

// Create makes the scratch disk, the control channel, and the container, and
// starts the container.
func (d backend) Create(ctx context.Context, b *box, spec sandbox.Spec, s sandbox.Settings) error {
	runnerDir, patched, err := d.opts.Runner(ctx, spec.RunnerVersion, spec.Arch)
	if err != nil {
		return fmt.Errorf("find runner bundle: %w", err)
	}
	initPath, err := d.opts.InitPath()
	if err != nil {
		return err
	}
	ctlDir := filepath.Join(b.Dir, "ctl")
	// The ctl folder is the root of a mount in the sandbox, so its parent
	// does not protect it there. The guest root can be a mapped user that
	// needs the x bit.
	if err := mkdir(ctlDir, 0o755); err != nil {
		return err
	}
	m := mounts{init: initPath, ctl: ctlDir, bundle: runnerDir}
	if d.opts.Profile.loopScratch {
		if m.scratch, err = d.makeScratchDisk(b.Dir, spec.DiskGB); err != nil {
			return err
		}
	} else {
		m.scratch = volumeName(b.ID)
		_, err := d.docker.VolumeCreate(ctx, client.VolumeCreateOptions{
			Name:   m.scratch,
			Labels: map[string]string{LabelSandbox: b.ID, guest.LabelManaged: "true", labelData: d.opts.DataDir},
		})
		if err != nil {
			return fmt.Errorf("make scratch volume: %w", err)
		}
	}
	cfg := guest.NewConfig(spec, s, serviceURL(d.serviceHost, d.opts.ServicePort), guestRunnerDir, patched)
	ctl, err := d.listen(b, &cfg)
	if err != nil {
		return err
	}
	b.SetControl(ctl)

	res, err := d.docker.ContainerCreate(ctx, d.createOptions(spec, m, s.IPv4DNS()))
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	c := b.Host
	c.mu.Lock()
	c.id = res.ID
	c.mu.Unlock()
	if _, err := d.docker.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	info, err := d.docker.ContainerInspect(ctx, res.ID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect container: %w", err)
	}
	if ns := info.Container.NetworkSettings; ns != nil && ns.Networks[networkName] != nil {
		if err := b.SetIP(ns.Networks[networkName].IPAddress.String()); err != nil {
			return err
		}
	}
	return d.bindPort(b, info.Container)
}

// bindPort lets the bridge port of the container pass only the frames with
// the MAC and IPv4 address that Docker gave the container
// (netrules.Manager.AddPort).
func (d *Driver) bindPort(b *box, c container.InspectResponse) error {
	if !d.opts.Profile.firewall {
		return nil
	}
	var ep *network.EndpointSettings
	if c.NetworkSettings != nil {
		ep = c.NetworkSettings.Networks[networkName]
	}
	if ep == nil || c.State == nil {
		return fmt.Errorf("the container has no address on the network %s", networkName)
	}
	nsPath := c.NetworkSettings.SandboxKey
	if nsPath == "" {
		nsPath = fmt.Sprintf("/proc/%d/ns/net", c.State.Pid)
	}
	veth, err := netrules.HostPeer(nsPath, "eth0")
	if err != nil {
		return fmt.Errorf("find the bridge port of the container: %w", err)
	}
	if err := d.opts.NetRules.AddPort(veth, net.HardwareAddr(ep.MacAddress), ep.IPAddress); err != nil {
		return fmt.Errorf("bind the bridge port of the container: %w", err)
	}
	b.Host.mu.Lock()
	b.Host.port = veth
	b.Host.mu.Unlock()
	return nil
}

// makeScratchDisk makes the scratch disk in dir and mounts it at dir/scratch.
func (d *Driver) makeScratchDisk(dir string, sizeGB int) (string, error) {
	img, mnt := filepath.Join(dir, "scratch.img"), filepath.Join(dir, "scratch")
	if err := d.disks.Create(img, sizeGB); err != nil {
		return "", err
	}
	if err := scratch.Attach(img, mnt); err != nil {
		return "", err
	}
	if err := mkdir(filepath.Join(mnt, guest.ScratchDocker), 0o710); err != nil {
		return "", err
	}
	return mnt, mkdir(filepath.Join(mnt, guest.ScratchTmp), 0o777|os.ModeSticky)
}

// listen serves the control channel in the ctl folder, which the sandbox has
// at guest.CtlDir. cfg is nil for a sandbox from before a runplane restart.
func (d *Driver) listen(b *box, cfg *guest.Config) (*control.Server, error) {
	ctlDir := filepath.Join(b.Dir, "ctl")
	if d.tcp != nil {
		return d.tcp.Listen(ctlDir, b.ID, cfg, b, b.Log())
	}
	return control.Listen(filepath.Join(ctlDir, guest.SocketFile), cfg, b, b.Log())
}
