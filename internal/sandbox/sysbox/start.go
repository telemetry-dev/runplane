package sysbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
	"github.com/runplane/runplane/internal/sandbox/netrules"
	"github.com/runplane/runplane/internal/sandbox/scratch"
)

// Start makes the scratch disk, the control channel, and the container, and
// starts the container. It returns when the container operates; the state
// is then starting.
func (d *Driver) Start(ctx context.Context, spec sandbox.Spec) error {
	if err := d.check(spec); err != nil {
		return fmt.Errorf("start sandbox %s: %w", spec.ID, err)
	}
	if spec.Arch == "" {
		spec.Arch = host.Arch()
	}
	d.startMu.Lock()
	defer d.startMu.Unlock()

	b := &box{id: spec.ID, dir: d.opts.Config.SandboxDir(spec.ID), created: time.Now().UTC(), removed: make(chan struct{})}
	b.spec = spec
	b.spec.JITConfig = ""
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return fmt.Errorf("start sandbox %s: the driver is closed", spec.ID)
	}
	if _, ok := d.boxes[spec.ID]; ok {
		d.mu.Unlock()
		return fmt.Errorf("start sandbox %s: it exists", spec.ID)
	}
	settings, dns := d.settings, d.dns
	d.boxes[spec.ID] = b
	b.state = sandbox.StateStarting
	d.emitState(b.id, sandbox.StateStarting, "")
	d.mu.Unlock()

	if err := d.create(ctx, b, spec, settings, dns); err != nil {
		d.mu.Lock()
		if ctx.Err() != nil {
			// The caller gave up, for example because runplane stops.
			// Nothing failed, so the sandbox ends as gone.
			b.stopAsked = true
		} else {
			b.failure = err.Error()
		}
		d.mu.Unlock()
		d.remove(b)
		return fmt.Errorf("start sandbox %s: %w", spec.ID, err)
	}
	d.log.Info("sandbox started", "sandbox", spec.ID, "pool", spec.PoolID, "image", spec.Image.Ref)
	return nil
}

var idPattern = regexp.MustCompile(`^[0-9a-z]{26}$`)

func (d *Driver) check(spec sandbox.Spec) error {
	switch {
	case !idPattern.MatchString(spec.ID):
		return errors.New("the ID is not a lowercase ULID")
	case spec.Image.Digest == "":
		return errors.New("the image has no digest")
	case spec.VCPU < 1 || spec.MemoryMB < 64:
		return fmt.Errorf("size %d vCPU and %d MB is too small", spec.VCPU, spec.MemoryMB)
	case d.opts.Scratch == ScratchLoop && spec.DiskGB < 1:
		return fmt.Errorf("disk size %d GB is too small", spec.DiskGB)
	}
	return nil
}

func (d *Driver) create(ctx context.Context, b *box, spec sandbox.Spec, s sandbox.Settings, dns []netip.Addr) error {
	bundle, err := d.opts.Bundle(ctx, spec.RunnerVersion, spec.Arch)
	if err != nil {
		return fmt.Errorf("find runner bundle: %w", err)
	}
	initPath, err := d.opts.InitPath()
	if err != nil {
		return err
	}
	ctlDir := filepath.Join(b.dir, "ctl")
	if err := mkdir(b.dir, 0o700); err != nil {
		return err
	}
	// The ctl folder is the root of a mount in the sandbox, so its parent
	// does not protect it there. The guest root can be a mapped user that
	// needs the x bit.
	if err := mkdir(ctlDir, 0o755); err != nil {
		return err
	}
	m := mounts{init: initPath, ctl: ctlDir, bundle: bundle.Dir}
	switch d.opts.Scratch {
	case ScratchLoop:
		if m.scratch, err = d.makeScratchDisk(b.dir, spec.DiskGB); err != nil {
			return err
		}
	case ScratchVolume:
		m.scratch = volumeName(spec.ID)
		_, err := d.docker.VolumeCreate(ctx, client.VolumeCreateOptions{
			Name:   m.scratch,
			Labels: map[string]string{sandbox.LabelSandbox: spec.ID, labelManaged: "true", labelData: d.opts.Config.DataDir},
		})
		if err != nil {
			return fmt.Errorf("make scratch volume: %w", err)
		}
	}
	logs, err := control.OpenLogFile(b.dir)
	if err != nil {
		return err
	}
	d.mu.Lock()
	b.logs = logs
	d.mu.Unlock()
	cfg := guestConfig(spec, s, bundle, serviceURL(d.opts.ServiceHost, d.opts.Config.ServicePort))
	ctl, err := d.listen(b, logs, cfg)
	if err != nil {
		return err
	}
	d.mu.Lock()
	b.ctl = ctl
	d.mu.Unlock()

	res, err := d.docker.ContainerCreate(ctx, createOptions(d.opts, spec, m, dns))
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	d.mu.Lock()
	b.containerID = res.ID
	err = d.save(b)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	if _, err := d.docker.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	info, err := d.docker.ContainerInspect(ctx, res.ID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect container: %w", err)
	}
	if ns := info.Container.NetworkSettings; ns != nil && ns.Networks[networkName] != nil {
		d.mu.Lock()
		b.ip = ns.Networks[networkName].IPAddress.String()
		err = d.save(b)
		d.mu.Unlock()
	}
	if err != nil {
		return err
	}
	return d.bindPort(b, info.Container)
}

// bindPort lets the bridge port of the container pass only the frames with
// the MAC and IPv4 address that Docker gave the container
// (netrules.Manager.AddPort).
func (d *Driver) bindPort(b *box, c container.InspectResponse) error {
	if d.opts.NetRules == nil {
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
	d.mu.Lock()
	b.port = veth
	d.mu.Unlock()
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
	if err := mkdir(filepath.Join(mnt, "docker"), 0o710); err != nil {
		return "", err
	}
	return mnt, mkdir(filepath.Join(mnt, "tmp"), 0o777|os.ModeSticky)
}

func (d *Driver) listen(b *box, logs *control.LogFile, cfg control.GuestConfig) (*control.Server, error) {
	log := d.log.With("sandbox", b.id)
	h := handler{d: d, b: b, logs: logs, log: log}
	if d.tcp != nil {
		return d.tcp.Listen(filepath.Join(b.dir, "ctl"), cfg, h, log)
	}
	return control.Listen(filepath.Join(b.dir, "ctl", "control.sock"), cfg, h, log)
}
