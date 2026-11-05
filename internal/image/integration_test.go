//go:build integration

package image

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/version"
)

// These tests use the network, and the Docker tests use the Docker daemon
// from the environment: go test -tags integration ./internal/image/

func TestRealRunnerBundle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	b := NewBundles(BundleConfig{Mode: sandbox.ModeVM, Dir: t.TempDir(), Logger: testLogger(t)})
	rel, err := b.release(ctx, "/releases/latest")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("latest runner %s", rel.Version)
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
	assertExt4(t, bundle.Ext4Path)
	debugfs(t, bundle.Ext4Path, "ls -l /bin/", "Runner.Worker.dll")
}

func TestRealRootDisk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	m := vmManager(t, nil)
	img, err := m.Ensure(ctx, "docker.io/library/alpine:3.20", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("root disk %s, %d bytes", img.Path, img.Size)
	assertExt4(t, img.Path)
	debugfs(t, img.Path, "ls -l /sbin/", "runplane-init")
	debugfs(t, img.Path, "ls -l /bin/", "busybox")
}

func TestDockerBuildsBuiltinImage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	docker := dockerClient(t)
	noPublishedImages(t)
	m, err := New(Config{Mode: sandbox.ModeContainer, Docker: docker, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	var steps []Progress
	img, err := m.Ensure(ctx, "builtin:ubuntu-24.04", func(p Progress) { steps = append(steps, p) })
	if err != nil {
		t.Fatal(err)
	}
	if s := statuses(steps); s != "pending pulling building ready" {
		t.Errorf("statuses = %s", s)
	}
	if img.Ref != "runplane/runner-ubuntu-24.04:local" || !strings.HasPrefix(img.Digest, "sha256:") {
		t.Errorf("image = %+v", img)
	}
	info, err := docker.ImageInspect(ctx, img.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Config == nil || info.Config.Labels[labelManaged] != "true" {
		t.Errorf("image labels = %v, want %s=true", info.Config, labelManaged)
	}
}

func TestDockerBuildsBuiltinRootDisk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	noPublishedImages(t)
	m := vmManager(t, dockerClient(t))
	var steps []Progress
	img, err := m.Ensure(ctx, "builtin:ubuntu-24.04", func(p Progress) { steps = append(steps, p) })
	if err != nil {
		t.Fatal(err)
	}
	if s := statuses(steps); s != "pending pulling building converting ready" {
		t.Errorf("statuses = %s", s)
	}
	t.Logf("root disk %s, %d bytes", img.Path, img.Size)
	assertExt4(t, img.Path)
	debugfs(t, img.Path, "ls -l /sbin/", "runplane-init")
	debugfs(t, img.Path, "stat /.dockerenv", "File not found")
}

func vmManager(t *testing.T, docker *client.Client) *Manager {
	t.Helper()
	m, err := New(Config{
		Mode:      sandbox.ModeVM,
		ImagesDir: t.TempDir(),
		Docker:    docker,
		Init:      func() ([]byte, error) { return []byte("#!runplane-init"), nil },
		Logger:    testLogger(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// noPublishedImages makes the pull of each builtin image fail.
func noPublishedImages(t *testing.T) {
	repo := version.Repo
	version.Repo = "runplane/no-such-repo"
	t.Cleanup(func() { version.Repo = repo })
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
