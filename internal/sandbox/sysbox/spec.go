package sysbox

import (
	"maps"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/cache"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
)

// Paths in the sandbox.
const (
	guestInit      = "/.runplane/init"
	guestRunnerDir = "/.runplane/runner" // read-only runner bundle
	guestCtlDir    = "/run/runplane"
	guestScratch   = "/mnt/scratch"
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
	// scratch is the mountpoint of the scratch disk (ScratchLoop) or the
	// name of the scratch volume (ScratchVolume).
	scratch string
}

// createOptions is the Docker container of a sandbox. The host folders are
// read-only. The other mounts are on the scratch disk or volume, and
// runplane-init puts the writes to more folders there. The rest of the
// container root is on the host disk; the driver stops a sandbox whose
// writes pass its disk size (checkDisks).
func createOptions(o Options, spec sandbox.Spec, m mounts, dns []netip.Addr) client.ContainerCreateOptions {
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[sandbox.LabelSandbox] = spec.ID
	labels[labelData] = o.Config.DataDir

	binds := []mount.Mount{
		{Type: mount.TypeBind, Source: m.init, Target: guestInit, ReadOnly: true},
		{Type: mount.TypeBind, Source: m.ctl, Target: guestCtlDir, ReadOnly: true},
		{Type: mount.TypeBind, Source: m.bundle, Target: guestRunnerDir, ReadOnly: true},
	}
	switch o.Scratch {
	case ScratchLoop:
		// Sysbox mounts its own host folder at /var/lib/docker unless the
		// container has a mount there, so Docker data goes to the scratch
		// disk only with this mount. /tmp on scratch keeps large temporary
		// files off the host disk.
		binds = append(binds,
			mount.Mount{Type: mount.TypeBind, Source: m.scratch, Target: guestScratch},
			mount.Mount{Type: mount.TypeBind, Source: filepath.Join(m.scratch, "docker"), Target: "/var/lib/docker"},
			mount.Mount{Type: mount.TypeBind, Source: filepath.Join(m.scratch, "tmp"), Target: "/tmp"},
		)
	case ScratchVolume:
		binds = append(binds, mount.Mount{Type: mount.TypeVolume, Source: m.scratch, Target: guestScratch})
	}

	pids := int64(pidsLimit)
	memory := int64(spec.MemoryMB) << 20
	hostConfig := &container.HostConfig{
		NetworkMode: container.NetworkMode(networkName),
		Runtime:     o.Runtime,
		Privileged:  o.Privileged,
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
			CgroupParent: o.cgroupParent(),
		},
	}
	if _, err := netip.ParseAddr(o.ServiceHost); err != nil {
		// A name such as host.docker.internal (dev mode) points to the
		// Docker host.
		hostConfig.ExtraHosts = []string{o.ServiceHost + ":host-gateway"}
	}
	return client.ContainerCreateOptions{
		Name: containerName(spec.ID),
		Config: &container.Config{
			Image:      spec.Image.Digest,
			Hostname:   hostname(spec.ID),
			Entrypoint: []string{guestInit},
			Labels:     labels,
		},
		HostConfig: hostConfig,
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{networkName: {}},
		},
	}
}

// guestConfig is what runplane-init gets from the control channel.
func guestConfig(spec sandbox.Spec, s sandbox.Settings, bundle image.Bundle, serviceURL string) control.GuestConfig {
	env := map[string]string{}
	// An unpatched runner overwrites ACTIONS_RESULTS_URL with the GitHub
	// value, so the local cache works only with a patched bundle.
	if s.ActionsCache && bundle.Patched {
		maps.Copy(env, cache.RunnerEnv(serviceURL))
	}
	maps.Copy(env, spec.Env)
	var mirror string
	if s.RegistryMirror {
		mirror = serviceURL
	}
	return control.GuestConfig{
		SandboxID:        spec.ID,
		Hostname:         hostname(spec.ID),
		JITConfig:        spec.JITConfig,
		RunnerDir:        guestRunnerDir,
		Env:              env,
		DNS:              s.DNS,
		RegistryMirror:   mirror,
		TimeLimitSeconds: int(spec.TimeLimit / time.Second),
		Debug:            spec.Debug,
	}
}

func serviceURL(host string, port int) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

func containerName(id string) string { return "runplane-" + id }

// hostname uses the random end of the ULID, as the runner name does. The
// start is the time, which sandboxes of the same second share.
func hostname(id string) string { return "runplane-" + id[len(id)-8:] }

func volumeName(id string) string { return "runplane-scratch-" + id }
