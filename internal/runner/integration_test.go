//go:build integration

package runner

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/ext4/ext4test"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// These tests use the network, and the Docker test uses the Docker daemon
// from the environment: go test -tags integration ./internal/runner/

func TestRealRunnerBundle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rel, err := Releases{}.Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("latest runner %s", rel.Version)
	b := NewBundles(BundleConfig{Mode: sandbox.ModeVM, Dir: t.TempDir(), Logger: testLogger(t)})
	bundle, err := b.Ensure(ctx, rel.Version, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Patched {
		t.Fatalf("runner %s is not patched", rel.Version)
	}
	dll, err := os.ReadFile(filepath.Join(bundle.Dir, "bin", "Runner.Worker.dll"))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(dll, utf16Bytes("ACTIONS_RESULTS_URL")); n != 0 {
		t.Errorf("%d matches are left", n)
	}
	t.Logf("Runner.Worker.dll: %d bytes, %d patches", len(dll), bytes.Count(dll, utf16Bytes("ACTIONS_RESULTS_ORL")))
	t.Run("ext4", func(t *testing.T) {
		ext4test.Check(t, bundle.Ext4Path)
		ext4test.Debugfs(t, bundle.Ext4Path, "ls -l /bin/", "Runner.Worker.dll")
	})
}

// baseImage is the base image that "make runner-smoke" tests. Build it with:
//
//	docker build -t runplane/runner-ubuntu-24.04:local --build-arg UBUNTU=24.04 --build-arg IMAGE_OS=ubuntu24 images
const baseImage = "runplane/runner-ubuntu-24.04:local"

// TestRealRunnerInBaseImage starts the real runner of this arch (linux-arm64
// on an arm64 host) in the base image. Runner.Listener is a .NET program: it
// starts only if the image has its libraries (ICU, OpenSSL, Kerberos,
// LTTng). A JIT config that is not valid base64 must make run.sh exit, so
// that runplane-init reports "exited" and the sandbox ends. The runner
// exits with code 0 also after this error ("exit with terminated error, no
// retry needed"), so the code does not tell the error from a normal end.
func TestRealRunnerInBaseImage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	docker := dockerClient(t)
	if _, err := docker.ImageInspect(ctx, baseImage); err != nil {
		t.Skipf("needs the local image %s; the comment of baseImage gives the docker build command: %v", baseImage, err)
	}
	rel, err := Releases{}.Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	version := rel.Version
	b := NewBundles(BundleConfig{Mode: sandbox.ModeContainer, Dir: t.TempDir(), Logger: testLogger(t)})
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	bundle, err := b.Ensure(ctx, version, arch)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("runner %s for linux-%s in %s", version, arch, bundle.Dir)

	out, code := runRunner(t, docker, bundle.Dir, "./bin/Runner.Listener", "--version")
	if code != 0 || strings.TrimSpace(out) != version {
		t.Fatalf("Runner.Listener --version: exit code %d, output:\n%s\nwant %s: the image lacks a library of the runner", code, out, version)
	}

	start := time.Now()
	out, code = runRunner(t, docker, bundle.Dir, "./run.sh", "--jitconfig", "not-base64!")
	t.Logf("run.sh exited with code %d after %s:\n%s", code, time.Since(start).Round(time.Millisecond), out)
	if strings.Contains(out, "Listening for Jobs") {
		t.Errorf("the runner listens for jobs with a bad JIT config")
	}
}

// runRunner runs args as the user runner in the runner folder of a new
// container of baseImage, and returns the output and the exit code. The
// bundle is mounted read-only, and its copy is the runner folder, as
// runplane-init does when it cannot mount an overlay: the runner writes
// files next to bin/. A command that runs for more than two minutes fails
// the test: in a sandbox, runplane-init would wait until the time limit.
func runRunner(t *testing.T, docker *client.Client, bundle string, args ...string) (string, int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	const ro = "/opt/runner-bundle"
	copyAndRun := []string{"sh", "-c", "cp -a " + ro + "/. . && exec \"$@\"", "sh"}
	res, err := docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: baseImage, User: guest.RunnerUser, WorkingDir: guest.RunnerHome, Entrypoint: append(copyAndRun, args...),
			Labels: map[string]string{guest.LabelManaged: "true"},
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{Type: mount.TypeBind, Source: bundle, Target: ro, ReadOnly: true}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer docker.ContainerRemove(context.WithoutCancel(ctx), res.ID, client.ContainerRemoveOptions{Force: true})
	wait := docker.ContainerWait(ctx, res.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := docker.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	var code int64
	select {
	case r := <-wait.Result:
		code = r.StatusCode
	case err := <-wait.Error:
		t.Fatalf("%s did not exit within two minutes: %v", strings.Join(args, " "), err)
	}
	logs, err := docker.ContainerLogs(context.WithoutCancel(ctx), res.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	var out strings.Builder
	if _, err := stdcopy.StdCopy(&out, &out, logs); err != nil {
		t.Fatal(err)
	}
	return out.String(), code
}

func dockerClient(t *testing.T) *client.Client {
	t.Helper()
	docker, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { docker.Close() })
	return docker
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), nil))
}
