package sysbox

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"

	"github.com/runplane/runplane/internal/sandbox"
)

var testSpec = sandbox.Spec{
	ID:            "01jbz3x0aaaaaaaaaaaaaaaaaa",
	PoolID:        "pool1",
	Image:         sandbox.ImageRef{Ref: "ghcr.io/runplane/runner-ubuntu-24.04:latest", Digest: "sha256:1111"},
	RunnerVersion: "2.330.0",
	Arch:          "x64",
	VCPU:          2,
	MemoryMB:      4096,
	DiskGB:        64,
	TimeLimit:     time.Hour,
	JITConfig:     "jit",
}

func TestCreateOptionsContainerMode(t *testing.T) {
	d := &Driver{opts: Options{DataDir: "/var/lib/runplane", Profile: Container}, serviceHost: "10.213.0.1"}
	m := mounts{init: "/tmp/runplane-init", ctl: "/var/lib/runplane/sandboxes/x/ctl", bundle: "/var/lib/runplane/runners/2.330.0-x64", scratch: "/var/lib/runplane/sandboxes/x/scratch"}
	dns := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	got := d.createOptions(testSpec, m, dns)

	h := got.HostConfig
	checkMounts(t, h.Mounts, m, []mount.Mount{
		{Type: mount.TypeBind, Source: "/var/lib/runplane/sandboxes/x/scratch", Target: "/mnt/scratch"},
		// Without a mount at /var/lib/docker, Sysbox mounts a host folder
		// there. The writes of a job go to the scratch disk.
		{Type: mount.TypeBind, Source: "/var/lib/runplane/sandboxes/x/scratch/docker", Target: "/var/lib/docker"},
		{Type: mount.TypeBind, Source: "/var/lib/runplane/sandboxes/x/scratch/tmp", Target: "/tmp"},
	})
	if h.Runtime != "sysbox-runc" || h.Privileged {
		t.Errorf("runtime %q, privileged %v; want sysbox-runc without privileges", h.Runtime, h.Privileged)
	}
	r := h.Resources
	if r.NanoCPUs != 2_000_000_000 || r.Memory != 4096<<20 || r.MemorySwap != 4096<<20 || r.PidsLimit == nil || *r.PidsLimit <= 0 {
		t.Errorf("resources: cpus %d, memory %d, swap %d, pids %v; want 2 CPUs, 4 GiB, no swap, a pids limit",
			r.NanoCPUs, r.Memory, r.MemorySwap, r.PidsLimit)
	}
	if r.CgroupParent != "runplane.slice" || r.CPUShares == 0 || r.CPUShares >= 1024 {
		t.Errorf("cgroup parent %q, CPU shares %d; want runplane.slice and a weight below the default", r.CgroupParent, r.CPUShares)
	}
	if !slices.Equal(h.DNS, dns) || h.NetworkMode != "runplane" || len(h.ExtraHosts) != 0 {
		t.Errorf("DNS %v, network %q, extra hosts %v", h.DNS, h.NetworkMode, h.ExtraHosts)
	}
	if _, ok := got.NetworkingConfig.EndpointsConfig["runplane"]; !ok {
		t.Errorf("endpoints = %v, want the runplane network", got.NetworkingConfig.EndpointsConfig)
	}

	// The driver finds its sandboxes after a restart by these labels.
	c := got.Config
	if c.Labels["dev.runplane.sandbox"] != "01jbz3x0aaaaaaaaaaaaaaaaaa" || c.Labels["dev.runplane.data"] != "/var/lib/runplane" {
		t.Errorf("labels = %v, want the sandbox ID and the data folder", c.Labels)
	}
	if c.Image != "sha256:1111" || !slices.Equal(c.Entrypoint, []string{"/.runplane/init"}) || c.Hostname != "runplane-aaaaaaaa" {
		t.Errorf("image %q, entrypoint %v, hostname %q", c.Image, c.Entrypoint, c.Hostname)
	}
	if got.Name != "runplane-01jbz3x0aaaaaaaaaaaaaaaaaa" {
		t.Errorf("name = %q", got.Name)
	}
}

func TestCreateOptionsDevMode(t *testing.T) {
	d := &Driver{opts: Options{Profile: Dev}, serviceHost: "host.docker.internal"}
	m := mounts{init: "/i", ctl: "/c", bundle: "/b", scratch: "runplane-scratch-01jbz3x0aaaaaaaaaaaaaaaaaa"}
	h := d.createOptions(testSpec, m, nil).HostConfig

	checkMounts(t, h.Mounts, m, []mount.Mount{
		{Type: mount.TypeVolume, Source: "runplane-scratch-01jbz3x0aaaaaaaaaaaaaaaaaa", Target: "/mnt/scratch"},
	})
	if h.Runtime != "runc" || !h.Privileged || h.CgroupParent != "" {
		t.Errorf("runtime %q, privileged %v, cgroup parent %q", h.Runtime, h.Privileged, h.CgroupParent)
	}
	if !slices.Equal(h.ExtraHosts, []string{"host.docker.internal:host-gateway"}) {
		t.Errorf("extra hosts = %v, want host.docker.internal on the Docker host", h.ExtraHosts)
	}
}

// checkMounts checks that a sandbox has runplane-init, the folder of the
// control socket, and the runner bundle from the host, read-only, at the
// paths where runplane-init and the guest config look for them. It also
// checks the scratch mounts, and that no other mount lets a job write to the
// host.
func checkMounts(t *testing.T, got []mount.Mount, m mounts, scratch []mount.Mount) {
	t.Helper()
	want := append([]mount.Mount{
		{Type: mount.TypeBind, Source: m.init, Target: "/.runplane/init", ReadOnly: true},
		{Type: mount.TypeBind, Source: m.ctl, Target: "/run/runplane", ReadOnly: true},
		{Type: mount.TypeBind, Source: m.bundle, Target: "/.runplane/runner", ReadOnly: true},
	}, scratch...)
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("no mount %+v in %+v", w, got)
		}
	}
	for _, g := range got {
		if g.Type == mount.TypeBind && !g.ReadOnly && !strings.HasPrefix(g.Source, m.scratch) {
			t.Errorf("a job can write to the host folder %s at %s", g.Source, g.Target)
		}
	}
}
