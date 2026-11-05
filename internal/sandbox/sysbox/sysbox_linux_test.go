//go:build linux

package sysbox_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/netrules"
	"github.com/runplane/runplane/internal/sandbox/sysbox"
	"github.com/runplane/runplane/internal/sandbox/sysbox/sysboxtest"
)

// TestContainerMode runs the container mode path as root on a Linux Docker
// host, with RUNPLANE_TEST_DOCKER=1: loop scratch disks, Unix control
// sockets, the network rules, and runplane.slice. It uses sysbox-runc when
// Docker has it, else runc. RUNPLANE_TEST_IMAGE must name an image with
// bash, curl, a user "runner", and the label dev.runplane.managed=true.
func TestContainerMode(t *testing.T) {
	if os.Getenv("RUNPLANE_TEST_DOCKER") != "1" || os.Getuid() != 0 {
		t.Skip("set RUNPLANE_TEST_DOCKER=1 and run as root")
	}
	ref := os.Getenv("RUNPLANE_TEST_IMAGE")
	if ref == "" {
		t.Fatal("set RUNPLANE_TEST_IMAGE")
	}
	docker, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	info, err := docker.Info(ctx, client.InfoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	runtime := "runc"
	if _, ok := info.Info.Runtimes["sysbox-runc"]; ok {
		runtime = "sysbox-runc"
	}
	t.Logf("runtime %s", runtime)
	img, err := docker.ImageInspect(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	log := sysboxtest.Logger(t)
	bundle := sysboxtest.NewBundle(t, filepath.Join(work, "runner"))
	initPath := sysboxtest.Init(t, work, "")
	rules := netrules.New(log)
	defer rules.Teardown()
	cfg := config.Config{DataDir: filepath.Join(work, "data"), ServicePort: 8486}
	opts := sysbox.Options{
		Docker:   docker,
		Config:   cfg,
		Mode:     sandbox.ModeContainer,
		Runtime:  runtime,
		Scratch:  sysbox.ScratchLoop,
		Bundle:   bundle.Ensure,
		InitPath: func() (string, error) { return initPath, nil },
		NetRules: rules,
		Settings: sandbox.Settings{DNS: []string{"1.1.1.1"}, ActionsCache: true},
		Logger:   log,
	}
	drv, err := sysbox.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { drv.Close() }()
	ev := sysboxtest.Watch(drv.Events())
	spec := func(id string) sandbox.Spec {
		return sandbox.Spec{
			ID:            id,
			Image:         sandbox.ImageRef{Ref: ref, Digest: img.ID},
			RunnerVersion: "2.330.0",
			VCPU:          1,
			MemoryMB:      512,
			DiskGB:        2,
			JITConfig:     "jit-" + id,
		}
	}

	if out, err := exec.Command("nft", "list", "table", "inet", "runplane").CombinedOutput(); err != nil || !strings.Contains(string(out), `"runplane0"`) {
		t.Errorf("the firewall table has no rules for the runplane0 bridge: %v\n%s", err, out)
	}

	t.Run("job", func(t *testing.T) {
		id := ids.New()
		t.Cleanup(func() { docker.ContainerRemove(ctx, "runplane-"+id, client.ContainerRemoveOptions{Force: true}) })
		started := time.Now()
		if err := drv.Start(ctx, spec(id)); err != nil {
			t.Fatal(err)
		}
		logs, err := drv.Logs(ctx, id, true)
		if err != nil {
			t.Fatal(err)
		}
		logText := sysboxtest.ReadAll(logs)
		idle := ev.Wait(t, id, sandbox.StateIdle, 2*time.Minute)
		t.Logf("start to idle: %s", idle.Sub(started).Round(time.Millisecond))

		c, err := docker.ContainerInspect(ctx, "runplane-"+id, client.ContainerInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cgroup, _ := os.ReadFile("/proc/" + strconv.Itoa(c.Container.State.Pid) + "/cgroup")
		if !strings.Contains(string(cgroup), "runplane.slice") {
			t.Errorf("the sandbox cgroup is not under runplane.slice: %s", cgroup)
		}
		// The bridge port of the sandbox passes only its own addresses.
		ip := c.Container.NetworkSettings.Networks["runplane"].IPAddress.String()
		if out, err := exec.Command("nft", "list", "table", "bridge", "runplane").CombinedOutput(); err != nil || !strings.Contains(string(out), ip) {
			t.Errorf("the bridge table has no binding for %s: %v\n%s", ip, err, out)
		}

		bundle.Signal(t, "start", id)
		bundle.Signal(t, "end", id)
		ev.Wait(t, id, sandbox.StateGone, 2*time.Minute)
		if got, want := ev.States(id), []sandbox.State{"starting", "idle", "busy", "stopping", "gone"}; !slices.Equal(got, want) {
			t.Errorf("states = %v, want %v", got, want)
		}
		text := logText()
		sysboxtest.CheckLines(t, text,
			"runner jit=jit-"+id,
			"runner ACTIONS_RESULTS_URL=http://10.213.0.1:8486/",
			"runner mount /mnt/scratch ext4",
			"runner mount /var/lib/docker ext4",
			"runner mount /tmp ext4",
			"runner Job test completed with result: Succeeded",
		)
		// The writes to these image folders go to scratch. runc gives the
		// container root no right to mount.
		if runtime == "sysbox-runc" {
			sysboxtest.CheckLines(t, text, "runner mount /home overlay", "runner mount /opt overlay")
		}
		if t.Failed() {
			t.Logf("sandbox log:\n%s", text)
		}
		checkRemoved(t, docker, cfg, id)
		if out, _ := exec.Command("nft", "list", "table", "bridge", "runplane").CombinedOutput(); strings.Contains(string(out), ip+" ") || strings.Contains(string(out), ip+"\n") {
			t.Errorf("the bridge table keeps the binding of the removed sandbox:\n%s", out)
		}
	})

	t.Run("restart", func(t *testing.T) {
		id := ids.New()
		t.Cleanup(func() { docker.ContainerRemove(ctx, "runplane-"+id, client.ContainerRemoveOptions{Force: true}) })
		if err := drv.Start(ctx, spec(id)); err != nil {
			t.Fatal(err)
		}
		ev.Wait(t, id, sandbox.StateIdle, 2*time.Minute)
		drv.Close()
		drv, err = sysbox.New(opts)
		if err != nil {
			t.Fatal(err)
		}
		ev = sysboxtest.Watch(drv.Events())
		ev.WaitRunner(t, id, sandbox.RunnerResume, 30*time.Second)
		bundle.Signal(t, "start", id)
		bundle.Signal(t, "end", id)
		ev.Wait(t, id, sandbox.StateGone, 2*time.Minute)
		if got, want := ev.States(id), []sandbox.State{"busy", "stopping", "gone"}; !slices.Equal(got, want) {
			t.Errorf("states after the restart = %v, want %v", got, want)
		}
		checkRemoved(t, docker, cfg, id)
	})

	// runplane stops during a start: the sandbox goes away as gone, since
	// nothing failed.
	t.Run("canceled start", func(t *testing.T) {
		id := ids.New()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := drv.Start(canceled, spec(id)); !errors.Is(err, context.Canceled) {
			t.Fatalf("Start = %v, want context.Canceled", err)
		}
		end := ev.WaitFor(t, id, "the end", 30*time.Second, func(e sandbox.Event) bool { return e.State.Done() })
		if end.State != sandbox.StateGone {
			t.Errorf("the canceled start ended %s (%s), want gone", end.State, end.Reason)
		}
		checkRemoved(t, docker, cfg, id)
	})

	// The probe passes the firewall rules to the internet.
	t.Run("probe", func(t *testing.T) {
		if err := drv.Probe(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// checkRemoved checks that a removed sandbox left no container, mount,
// loop device, or folder.
func checkRemoved(t *testing.T, docker *client.Client, cfg config.Config, id string) {
	t.Helper()
	if _, err := docker.ContainerInspect(context.Background(), "runplane-"+id, client.ContainerInspectOptions{}); !cerrdefs.IsNotFound(err) {
		t.Errorf("the container is still there (inspect: %v)", err)
	}
	dir := cfg.SandboxDir(id)
	if mounts, _ := os.ReadFile("/proc/self/mounts"); strings.Contains(string(mounts), dir) {
		t.Errorf("the scratch disk is still mounted:\n%s", mounts)
	}
	backing, _ := filepath.Glob("/sys/block/loop*/loop/backing_file")
	for _, f := range backing {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), id) {
			t.Errorf("a loop device still holds the scratch disk: %s", f)
		}
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the sandbox folder is still there (stat: %v)", err)
	}
}
