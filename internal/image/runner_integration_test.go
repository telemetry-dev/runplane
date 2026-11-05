//go:build integration

package image

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// baseImage is the base image that "make runner-smoke" tests. Build it with:
//
//	docker build -t runplane/runner-ubuntu-24.04:local images/ubuntu-24.04
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
		t.Skipf("no image %s: %v", baseImage, err)
	}
	b := NewBundles(BundleConfig{Mode: sandbox.ModeContainer, Dir: t.TempDir(), Logger: testLogger(t)})
	version, _, err := b.release(ctx, "/releases/latest")
	if err != nil {
		t.Fatal(err)
	}
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
