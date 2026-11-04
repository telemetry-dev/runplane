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
	"github.com/runplane/runplane/internal/image"
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
	o := Options{Runtime: "sysbox-runc", Scratch: ScratchLoop, CgroupParent: "runplane.slice", ServiceHost: "10.213.0.1",
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
	o := Options{Runtime: "runc", Privileged: true, Scratch: ScratchVolume, ServiceHost: "host.docker.internal"}
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

func TestGuestConfig(t *testing.T) {
	const url = "http://10.213.0.1:8486"
	cases := []struct {
		name       string
		settings   sandbox.Settings
		patched    bool
		env        map[string]string
		wantEnv    map[string]string
		wantMirror string
	}{
		{
			name:       "cache and mirror",
			settings:   sandbox.Settings{ActionsCache: true, RegistryMirror: true},
			patched:    true,
			env:        map[string]string{"FOO": "bar"},
			wantEnv:    map[string]string{"FOO": "bar", "ACTIONS_RESULTS_URL": "http://10.213.0.1:8486/", "ACTIONS_CACHE_SERVICE_V2": "true"},
			wantMirror: url,
		},
		{
			name:     "unpatched runner keeps the GitHub cache",
			settings: sandbox.Settings{ActionsCache: true},
			env:      map[string]string{"FOO": "bar"},
			wantEnv:  map[string]string{"FOO": "bar"},
		},
		{
			name:     "cache off",
			settings: sandbox.Settings{},
			patched:  true,
			wantEnv:  map[string]string{},
		},
		{
			name:     "the spec env wins",
			settings: sandbox.Settings{ActionsCache: true},
			patched:  true,
			env:      map[string]string{"ACTIONS_RESULTS_URL": "http://other/"},
			wantEnv:  map[string]string{"ACTIONS_RESULTS_URL": "http://other/", "ACTIONS_CACHE_SERVICE_V2": "true"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := testSpec
			spec.Env = c.env
			spec.Debug = true
			got := guestConfig(spec, c.settings, image.Bundle{Patched: c.patched}, url)
			if !maps.Equal(got.Env, c.wantEnv) {
				t.Errorf("env = %v, want %v", got.Env, c.wantEnv)
			}
			if got.RegistryMirror != c.wantMirror {
				t.Errorf("registry mirror = %q, want %q", got.RegistryMirror, c.wantMirror)
			}
			if got.JITConfig != "jit" || got.TimeLimitSeconds != 3600 || !got.Debug || got.SandboxID != spec.ID || got.Hostname != "runplane-aaaaaaaa" {
				t.Errorf("config = %+v", got)
			}
		})
	}
}

// The guest finds the bundle at RunnerDir, so it must be where the
// container mounts it.
func TestRunnerDirIsTheBundleMount(t *testing.T) {
	cfg := guestConfig(testSpec, sandbox.Settings{}, image.Bundle{}, "")
	h := createOptions(Options{Scratch: ScratchLoop}, testSpec, mounts{bundle: "/bundle"}, nil).HostConfig
	i := slices.IndexFunc(h.Mounts, func(m mount.Mount) bool { return m.Source == "/bundle" })
	if i < 0 || h.Mounts[i].Target != cfg.RunnerDir {
		t.Fatalf("RunnerDir %q, bundle mounts %+v", cfg.RunnerDir, h.Mounts)
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
	// A warm sandbox waits up to the limit and the idle grace for a job.
	b := &box{created: t0, spec: sandbox.Spec{TimeLimit: time.Hour}}
	if pastLimit(b, t0.Add(time.Hour+9*time.Minute)) {
		t.Error("an idle sandbox is past the limit within the idle and the limit grace")
	}
	if !pastLimit(b, t0.Add(time.Hour+11*time.Minute)) {
		t.Error("an idle sandbox is not past the limit after the idle and the limit grace")
	}
	// The limit of a job counts from its start, not from the sandbox start.
	b.busyAt = t0.Add(50 * time.Minute)
	if pastLimit(b, t0.Add(time.Hour+54*time.Minute)) {
		t.Error("past the limit within the grace time after the job start")
	}
	if !pastLimit(b, t0.Add(time.Hour+56*time.Minute)) {
		t.Error("not past the limit after the grace time")
	}
	b.holdUntil = t0.Add(3 * time.Hour)
	if pastLimit(b, t0.Add(2*time.Hour)) {
		t.Error("past the limit during a debug hold")
	}
	if !pastLimit(b, t0.Add(3*time.Hour+6*time.Minute)) {
		t.Error("not past the limit after the hold and the grace time")
	}
	if pastLimit(&box{created: t0}, t0.Add(1000*time.Hour)) {
		t.Error("a sandbox without a time limit is past its limit")
	}
}

// The first ULID characters hold the time, so sandboxes that start in the
// same second share them.
func TestHostnameDiffersWithinASecond(t *testing.T) {
	a, b := hostname("01m6jwd94yx8aj3sdamaedysaw"), hostname("01m6jwd94z4kq7c1v9b3rt2xnm")
	if a != "runplane-maedysaw" || b != "runplane-b3rt2xnm" {
		t.Errorf("host names %q and %q, want runplane-maedysaw and runplane-b3rt2xnm", a, b)
	}
}
