package devdocker_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
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
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/devdocker"
	"github.com/runplane/runplane/internal/sandbox/sysbox/sysboxtest"
)

func TestRefusesWithoutDevInsecure(t *testing.T) {
	_, err := devdocker.New(devdocker.Options{Config: config.Config{DataDir: t.TempDir(), ServicePort: 8486}})
	if err == nil {
		t.Fatal("New made a privileged driver without RUNPLANE_DEV_INSECURE")
	}
}

// TestSandbox starts real sandboxes on Docker with RUNPLANE_TEST_DOCKER=1.
// It uses the image in RUNPLANE_TEST_IMAGE, by default the local build of
// the base image: docker build -t runplane/runner-ubuntu-24.04:local images/ubuntu-24.04
func TestSandbox(t *testing.T) {
	if os.Getenv("RUNPLANE_TEST_DOCKER") != "1" {
		t.Skip("set RUNPLANE_TEST_DOCKER=1 to start sandboxes on Docker")
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
	ref := os.Getenv("RUNPLANE_TEST_IMAGE")
	if ref == "" {
		ref = "runplane/runner-ubuntu-24.04:local"
	}
	img, err := docker.ImageInspect(ctx, ref)
	if err != nil {
		t.Fatalf("inspect %s: %v; build it with: docker build -t %[1]s images/ubuntu-24.04", ref, err)
	}

	work := t.TempDir()
	driverLog := &sysboxtest.Log{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("driver log:\n%s", driverLog)
		}
	})
	goarch := map[string]string{"aarch64": "arm64", "x86_64": "amd64"}[info.Info.Architecture]
	initPath := sysboxtest.Init(t, work, goarch)
	bundle := sysboxtest.NewBundle(t, filepath.Join(work, "runner"))
	cfg := config.Config{DataDir: filepath.Join(work, "data"), ServicePort: freePort(t) - 1, DevInsecure: true}
	opts := devdocker.Options{
		Docker: docker,
		Config: cfg,
		Bundle: func(_ context.Context, version, arch string) (image.Bundle, error) {
			return image.Bundle{Version: version, Arch: arch, Dir: bundle.Dir, Patched: true}, nil
		},
		InitPath: func() (string, error) { return initPath, nil },
		Settings: sandbox.Settings{DNS: []string{"1.1.1.1"}, ActionsCache: true, ReserveVCPU: 1, ReserveMemoryMB: 1024, Overcommit: 1},
		Logger:   slog.New(slog.NewTextHandler(driverLog, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	drv, err := devdocker.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { drv.Close() }()
	ev := sysboxtest.Watch(drv.Events())
	spec := func(id string) sandbox.Spec {
		return sandbox.Spec{
			ID:            id,
			PoolID:        "pool-test",
			Image:         sandbox.ImageRef{Ref: ref, Digest: img.ID},
			RunnerVersion: "2.330.0",
			VCPU:          2,
			MemoryMB:      2048,
			DiskGB:        8,
			TimeLimit:     10 * time.Minute,
			JITConfig:     "jit-" + id,
			Env:           map[string]string{"RUNPLANE_TEST": "1"},
		}
	}

	t.Run("job", func(t *testing.T) {
		id := ids.New()
		t.Cleanup(func() { removeLeftovers(docker, id) })
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
		list, _ := drv.List(ctx)
		if len(list) != 1 || list[0].ID != id || list[0].State != sandbox.StateIdle || list[0].IP == "" || list[0].Spec.JITConfig != "" {
			t.Errorf("List = %+v, want the sandbox idle with an IP and no JIT config", list)
		}
		capacity, err := drv.Capacity(ctx)
		if err != nil || capacity.AllocVCPU != 2 || capacity.AllocMemoryMB != 2048 || capacity.Sandboxes != 1 {
			t.Errorf("Capacity = %+v, %v; want the allocation of one sandbox", capacity, err)
		}

		bundle.Signal(t, "start", id)
		ev.Wait(t, id, sandbox.StateBusy, time.Minute)
		// The second sample gives the first metrics, about 10 s after busy.
		m := ev.WaitFor(t, id, "metrics", 30*time.Second, func(ev sandbox.Event) bool { return ev.Type == sandbox.EventMetrics }).Metrics
		t.Logf("metrics: %+v", *m)
		if m.CPUPercent < 20 || m.CPUPercent > 100 || m.MemoryMB <= 0 {
			t.Errorf("metrics = %+v, want about 50%% CPU (one busy core of 2 vCPU) and some memory", *m)
		}
		bundle.Signal(t, "end", id)
		ev.Wait(t, id, sandbox.StateGone, 2*time.Minute)
		if got, want := ev.States(id), []sandbox.State{"starting", "idle", "busy", "stopping", "gone"}; !slices.Equal(got, want) {
			t.Errorf("states = %v, want %v", got, want)
		}
		text := logText()
		sysboxtest.CheckLines(t, text,
			"runner jit=jit-"+id,
			"runner ACTIONS_RESULTS_URL=http://host.docker.internal:"+strconv.Itoa(cfg.ServicePort)+"/",
			"runner RUNPLANE_TEST=1",
			"runner mount /mnt/scratch ",
			"runner Listening for Jobs",
			"runner Running job: test",
			"runner docker storage=overlay2",
			"runner Job test completed with result: Succeeded",
		)
		if t.Failed() {
			t.Logf("sandbox log:\n%s", text)
		}
		checkRemoved(t, docker, cfg, id)
	})

	t.Run("stop", func(t *testing.T) {
		id := ids.New()
		t.Cleanup(func() { removeLeftovers(docker, id) })
		if err := drv.Start(ctx, spec(id)); err != nil {
			t.Fatal(err)
		}
		ev.Wait(t, id, sandbox.StateIdle, 2*time.Minute)

		term, err := drv.Shell(ctx, id, sandbox.TermSize{Rows: 24, Cols: 80})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(term, "echo shell-$((6*7)) as $(whoami)\n"); err != nil {
			t.Fatal(err)
		}
		if out := sysboxtest.ReadUntil(term, "shell-42 as runner", 10*time.Second); !strings.Contains(out, "shell-42 as runner") {
			t.Errorf("the shell did not run the command as runner; output: %q", out)
		}
		term.Close()
		if err := drv.Hold(ctx, id, time.Now().Add(time.Hour)); err != nil {
			t.Errorf("Hold = %v", err)
		}

		stopped := time.Now()
		if err := drv.Stop(ctx, id); err != nil {
			t.Fatal(err)
		}
		t.Logf("stop of an idle sandbox: %s", time.Since(stopped).Round(time.Millisecond))
		if got, want := ev.States(id), []sandbox.State{"starting", "idle", "stopping", "gone"}; !slices.Equal(got, want) {
			t.Errorf("states = %v, want %v", got, want)
		}
		checkRemoved(t, docker, cfg, id)
		if err := drv.Stop(ctx, id); err != nil {
			t.Errorf("a second Stop = %v, want nil", err)
		}
	})

	t.Run("restart", func(t *testing.T) {
		id := ids.New()
		t.Cleanup(func() { removeLeftovers(docker, id) })
		if err := drv.Start(ctx, spec(id)); err != nil {
			t.Fatal(err)
		}
		ev.Wait(t, id, sandbox.StateIdle, 2*time.Minute)

		// runplane restarts. The sandbox keeps its runner, and the new
		// driver takes it over with a new control token.
		drv.Close()
		drv, err = devdocker.New(opts)
		if err != nil {
			t.Fatal(err)
		}
		ev = sysboxtest.Watch(drv.Events())
		list, _ := drv.List(ctx)
		if len(list) != 1 || list[0].ID != id || list[0].State != sandbox.StateIdle {
			t.Fatalf("List after the restart = %+v, want the idle sandbox", list)
		}
		ev.WaitRunner(t, id, sandbox.RunnerResume, 30*time.Second)
		bundle.Signal(t, "start", id)
		bundle.Signal(t, "end", id)
		ev.Wait(t, id, sandbox.StateGone, 2*time.Minute)
		if got, want := ev.States(id), []sandbox.State{"busy", "stopping", "gone"}; !slices.Equal(got, want) {
			t.Errorf("states after the restart = %v, want %v", got, want)
		}
		checkRemoved(t, docker, cfg, id)
	})

	t.Run("second driver", func(t *testing.T) {
		id := ids.New()
		t.Cleanup(func() { removeLeftovers(docker, id) })
		if err := drv.Start(ctx, spec(id)); err != nil {
			t.Fatal(err)
		}
		ev.Wait(t, id, sandbox.StateIdle, 2*time.Minute)

		// A second runplane on the same Docker, with its own data folder,
		// must not take over or remove the sandbox of the first.
		other := opts
		other.Config.DataDir = filepath.Join(work, "other")
		other.Config.ServicePort = freePort(t) - 1
		drv2, err := devdocker.New(other)
		if err != nil {
			t.Fatal(err)
		}
		list, _ := drv2.List(ctx)
		drv2.Close()
		if len(list) != 0 {
			t.Errorf("the second driver lists %+v, want no sandboxes", list)
		}
		res, err := docker.ContainerInspect(ctx, "runplane-"+id, client.ContainerInspectOptions{})
		if err != nil || !res.Container.State.Running {
			t.Fatalf("the sandbox container after the second driver started: %+v, %v; want it running", res.Container.State, err)
		}
		if err := drv.Stop(ctx, id); err != nil {
			t.Fatal(err)
		}
		ev.Wait(t, id, sandbox.StateGone, time.Minute)
		if got, want := ev.States(id), []sandbox.State{"starting", "idle", "stopping", "gone"}; !slices.Equal(got, want) {
			t.Errorf("states = %v, want %v", got, want)
		}
		checkRemoved(t, docker, cfg, id)
	})

	t.Run("probe", func(t *testing.T) {
		if err := drv.Probe(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// checkRemoved checks that a removed sandbox left no container, volume, or folder.
func checkRemoved(t *testing.T, docker *client.Client, cfg config.Config, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := docker.ContainerInspect(ctx, "runplane-"+id, client.ContainerInspectOptions{}); !cerrdefs.IsNotFound(err) {
		t.Errorf("the container is still there (inspect: %v)", err)
	}
	if _, err := docker.VolumeInspect(ctx, "runplane-scratch-"+id, client.VolumeInspectOptions{}); !cerrdefs.IsNotFound(err) {
		t.Errorf("the scratch volume is still there (inspect: %v)", err)
	}
	if _, err := os.Stat(cfg.SandboxDir(id)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the sandbox folder is still there (stat: %v)", err)
	}
}

func removeLeftovers(docker *client.Client, id string) {
	ctx := context.Background()
	docker.ContainerRemove(ctx, "runplane-"+id, client.ContainerRemoveOptions{Force: true})
	docker.VolumeRemove(ctx, "runplane-scratch-"+id, client.VolumeRemoveOptions{Force: true})
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
