package sysbox

import (
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"

	"github.com/runplane/runplane/internal/config"
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
	Labels:        map[string]string{"dev.runplane.pool": "pool1", sandbox.LabelSandbox: "not-this-id"},
}

func TestCreateOptionsContainerMode(t *testing.T) {
	o := Options{Mode: sandbox.ModeContainer, Runtime: "sysbox-runc", Scratch: ScratchLoop, ServiceHost: "10.213.0.1",
		Config: config.Config{DataDir: "/var/lib/runplane"}}
	m := mounts{init: "/tmp/runplane-init", ctl: "/var/lib/runplane/sandboxes/x/ctl", bundle: "/var/lib/runplane/runners/2.330.0-x64", scratch: "/var/lib/runplane/sandboxes/x/scratch"}
	dns := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	got := createOptions(o, testSpec, m, dns)

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
	wantLabels := map[string]string{"dev.runplane.pool": "pool1", sandbox.LabelSandbox: "01jbz3x0aaaaaaaaaaaaaaaaaa", "dev.runplane.data": "/var/lib/runplane"}
	if !maps.Equal(c.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", c.Labels, wantLabels)
	}
	if c.Image != "sha256:1111" || !slices.Equal(c.Entrypoint, []string{"/.runplane/init"}) || c.Hostname != "runplane-aaaaaaaa" {
		t.Errorf("image %q, entrypoint %v, hostname %q", c.Image, c.Entrypoint, c.Hostname)
	}
	if got.Name != "runplane-01jbz3x0aaaaaaaaaaaaaaaaaa" {
		t.Errorf("name = %q", got.Name)
	}
	if testSpec.Labels[sandbox.LabelSandbox] != "not-this-id" {
		t.Error("createOptions changed the labels of the spec")
	}
}

func TestCreateOptionsDevMode(t *testing.T) {
	o := Options{Mode: sandbox.ModeDev, Runtime: "runc", Privileged: true, Scratch: ScratchVolume, ServiceHost: "host.docker.internal"}
	m := mounts{init: "/i", ctl: "/c", bundle: "/b", scratch: "runplane-scratch-01jbz3x0aaaaaaaaaaaaaaaaaa"}
	h := createOptions(o, testSpec, m, nil).HostConfig

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

func TestForward(t *testing.T) {
	cases := []struct {
		cur, next sandbox.State
		want      bool
	}{
		{sandbox.StateStarting, sandbox.StateIdle, true},
		{sandbox.StateStarting, sandbox.StateBusy, true}, // the listening event can come late
		{sandbox.StateIdle, sandbox.StateBusy, true},
		{sandbox.StateBusy, sandbox.StateIdle, false},
		{sandbox.StateIdle, sandbox.StateIdle, false},
		{sandbox.StateStopping, sandbox.StateBusy, false},
		{sandbox.StateBusy, sandbox.StateStopping, true},
		{sandbox.StateStarting, sandbox.StateFailed, true},
		{sandbox.StateStopping, sandbox.StateGone, true},
		{sandbox.StateGone, sandbox.StateFailed, false},
		{sandbox.StateFailed, sandbox.StateStopping, false},
	}
	for _, c := range cases {
		if got := forward(c.cur, c.next); got != c.want {
			t.Errorf("forward(%s, %s) = %v, want %v", c.cur, c.next, got, c.want)
		}
	}
}

func TestPastLimit(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name           string
		limit          time.Duration
		busyAt, holdTo time.Duration // after t0; 0 means none
		at             time.Duration
		want           bool
	}{
		// A warm sandbox waits up to the limit, the idle grace (5m), and
		// the limit grace (5m) for a job.
		{"idle within the graces", time.Hour, 0, 0, time.Hour + 9*time.Minute, false},
		{"idle after the graces", time.Hour, 0, 0, time.Hour + 11*time.Minute, true},
		// The limit of a job counts from its start.
		{"job within the grace", time.Hour, 50 * time.Minute, 0, time.Hour + 54*time.Minute, false},
		{"job after the grace", time.Hour, 50 * time.Minute, 0, time.Hour + 56*time.Minute, true},
		{"during a debug hold", time.Hour, 50 * time.Minute, 3 * time.Hour, 2 * time.Hour, false},
		{"after the hold and the grace", time.Hour, 50 * time.Minute, 3 * time.Hour, 3*time.Hour + 6*time.Minute, true},
		{"no time limit", 0, 0, 0, 1000 * time.Hour, false},
	}
	for _, c := range cases {
		b := &box{created: t0, spec: sandbox.Spec{TimeLimit: c.limit}}
		if c.busyAt > 0 {
			b.busyAt = t0.Add(c.busyAt)
		}
		if c.holdTo > 0 {
			b.holdUntil = t0.Add(c.holdTo)
		}
		if got := pastLimit(b, t0.Add(c.at)); got != c.want {
			t.Errorf("%s: pastLimit = %v, want %v", c.name, got, c.want)
		}
	}
}
