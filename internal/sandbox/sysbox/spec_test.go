package sysbox

import (
	"maps"
	"net/netip"
	"reflect"
	"slices"
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
	Env:           map[string]string{"FOO": "bar"},
}

func TestCreateOptionsContainerMode(t *testing.T) {
	d := &Driver{opts: Options{DataDir: "/var/lib/runplane", Profile: Container}, serviceHost: "10.213.0.1"}
	m := mounts{init: "/tmp/runplane-init", ctl: "/var/lib/runplane/sandboxes/x/ctl", bundle: "/var/lib/runplane/runners/2.330.0-x64", scratch: "/var/lib/runplane/sandboxes/x/scratch"}
	dns := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	got := d.createOptions(testSpec, m, dns)

	wantMounts := []mount.Mount{
		{Type: mount.TypeBind, Source: "/tmp/runplane-init", Target: "/.runplane/init", ReadOnly: true},
		{Type: mount.TypeBind, Source: "/var/lib/runplane/sandboxes/x/ctl", Target: "/run/runplane", ReadOnly: true},
		{Type: mount.TypeBind, Source: "/var/lib/runplane/runners/2.330.0-x64", Target: "/.runplane/runner", ReadOnly: true},
		{Type: mount.TypeBind, Source: "/var/lib/runplane/sandboxes/x/scratch", Target: "/mnt/scratch"},
		{Type: mount.TypeBind, Source: "/var/lib/runplane/sandboxes/x/scratch/docker", Target: "/var/lib/docker"},
		{Type: mount.TypeBind, Source: "/var/lib/runplane/sandboxes/x/scratch/tmp", Target: "/tmp"},
	}
	h := got.HostConfig
	if !reflect.DeepEqual(h.Mounts, wantMounts) {
		t.Errorf("mounts = %+v\nwant %+v", h.Mounts, wantMounts)
	}
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

	c := got.Config
	wantLabels := map[string]string{"dev.runplane.sandbox": "01jbz3x0aaaaaaaaaaaaaaaaaa", "dev.runplane.data": "/var/lib/runplane"}
	if !maps.Equal(c.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", c.Labels, wantLabels)
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

	wantMounts := []mount.Mount{
		{Type: mount.TypeBind, Source: "/i", Target: "/.runplane/init", ReadOnly: true},
		{Type: mount.TypeBind, Source: "/c", Target: "/run/runplane", ReadOnly: true},
		{Type: mount.TypeBind, Source: "/b", Target: "/.runplane/runner", ReadOnly: true},
		{Type: mount.TypeVolume, Source: "runplane-scratch-01jbz3x0aaaaaaaaaaaaaaaaaa", Target: "/mnt/scratch"},
	}
	if !reflect.DeepEqual(h.Mounts, wantMounts) {
		t.Errorf("mounts = %+v\nwant %+v", h.Mounts, wantMounts)
	}
	if h.Runtime != "runc" || !h.Privileged || h.CgroupParent != "" {
		t.Errorf("runtime %q, privileged %v, cgroup parent %q", h.Runtime, h.Privileged, h.CgroupParent)
	}
	if !slices.Equal(h.ExtraHosts, []string{"host.docker.internal:host-gateway"}) {
		t.Errorf("extra hosts = %v, want host.docker.internal on the Docker host", h.ExtraHosts)
	}
}
