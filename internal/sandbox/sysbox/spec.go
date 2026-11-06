package sysbox

import (
	"net/netip"
	"path/filepath"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// Paths in the sandbox.
const (
	guestInit      = "/.runplane/init"
	guestRunnerDir = "/.runplane/runner" // read-only runner bundle
)

const (
	// pidsLimit stops a fork bomb long before the host runs out of PIDs.
	pidsLimit = 16384
	// cpuShares 256 is cpu.weight 10 on cgroup v2, a tenth of the weight of
	// other services, so CI does not slow down the apps on the host.
	cpuShares = 256
)

// mounts are the host sources of the sandbox mounts.
type mounts struct {
	init   string // runplane-init binary
	ctl    string // ctl folder with the control socket
	bundle string // runner bundle folder
	// scratch is the mountpoint of the scratch disk (Profile.loopScratch) or
	// the name of the scratch volume.
	scratch string
}

// createOptions is the Docker container of a sandbox. The host folders are
// read-only. The other mounts are on the scratch disk or volume, and
// runplane-init puts the writes to more folders there. The rest of the
// container root is on the host disk; the driver stops a sandbox whose
// writes pass its disk size (checkDisks).
func (d *Driver) createOptions(spec sandbox.Spec, m mounts, dns []netip.Addr) client.ContainerCreateOptions {
	p := d.opts.Profile
	binds := []mount.Mount{
		{Type: mount.TypeBind, Source: m.init, Target: guestInit, ReadOnly: true},
		{Type: mount.TypeBind, Source: m.ctl, Target: guest.CtlDir, ReadOnly: true},
		{Type: mount.TypeBind, Source: m.bundle, Target: guestRunnerDir, ReadOnly: true},
	}
	if p.loopScratch {
		// Sysbox mounts its own host folder at /var/lib/docker unless the
		// container has a mount there, so Docker data goes to the scratch
		// disk only with this mount. /tmp on scratch keeps large temporary
		// files off the host disk.
		binds = append(binds,
			mount.Mount{Type: mount.TypeBind, Source: m.scratch, Target: guest.ScratchDir},
			mount.Mount{Type: mount.TypeBind, Source: filepath.Join(m.scratch, guest.ScratchDocker), Target: guest.DockerDir},
			mount.Mount{Type: mount.TypeBind, Source: filepath.Join(m.scratch, guest.ScratchTmp), Target: "/tmp"},
		)
	} else {
		binds = append(binds, mount.Mount{Type: mount.TypeVolume, Source: m.scratch, Target: guest.ScratchDir})
	}

	pids := int64(pidsLimit)
	memory := int64(spec.MemoryMB) << 20
	hostConfig := &container.HostConfig{
		NetworkMode: container.NetworkMode(networkName),
		Runtime:     p.runtime,
		Privileged:  p.privileged,
		DNS:         dns,
		Mounts:      binds,
		// Jobs that run browsers need more than the 64 MB default.
		ShmSize: memory / 2,
		Resources: container.Resources{
			NanoCPUs:     int64(spec.VCPU) * 1e9,
			Memory:       memory,
			MemorySwap:   memory, // no swap
			PidsLimit:    &pids,
			CPUShares:    cpuShares,
			CgroupParent: p.cgroupParent,
		},
	}
	if _, err := netip.ParseAddr(d.serviceHost); err != nil {
		// A name such as host.docker.internal points to the Docker host.
		hostConfig.ExtraHosts = []string{d.serviceHost + ":host-gateway"}
	}
	return client.ContainerCreateOptions{
		Name: containerName(spec.ID),
		Config: &container.Config{
			Image:      spec.Image.Digest,
			Hostname:   guest.Hostname(spec.ID),
			Entrypoint: []string{guestInit},
			Labels:     map[string]string{LabelSandbox: spec.ID, labelData: d.opts.DataDir},
		},
		HostConfig: hostConfig,
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{networkName: {}},
		},
	}
}

func containerName(id string) string { return "runplane-" + id }

func volumeName(id string) string { return "runplane-scratch-" + id }
