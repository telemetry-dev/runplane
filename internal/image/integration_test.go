//go:build integration

package image

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/ext4/ext4test"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// These tests use the network, and the Docker tests use the Docker daemon
// from the environment: go test -tags integration ./internal/image/

func TestRealRootDisk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	m := vmManager(t, nil)
	img, err := m.Ensure(ctx, "docker.io/library/alpine:3.20", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("root disk %s, %d bytes", img.Path, img.Size)
	ext4test.Check(t, img.Path)
	ext4test.Debugfs(t, img.Path, "ls -l /sbin/", "runplane-init")
	ext4test.Debugfs(t, img.Path, "ls -l /bin/", "busybox")
}

func TestDockerBuildsBuiltinImage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	docker := dockerClient(t)
	m, err := New(Config{Mode: sandbox.ModeContainer, Docker: docker, Repo: noPublishedImages, Logger: testLogger(t)})
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
	if info.Config == nil || info.Config.Labels[guest.LabelManaged] != "true" {
		t.Errorf("image labels = %v, want %s=true", info.Config, guest.LabelManaged)
	}
}

func TestDockerBuildsBuiltinRootDisk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
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
	ext4test.Check(t, img.Path)
	ext4test.Debugfs(t, img.Path, "ls -l /sbin/", "runplane-init")
	ext4test.Debugfs(t, img.Path, "stat /.dockerenv", "File not found")
}

func vmManager(t *testing.T, docker *client.Client) *Manager {
	t.Helper()
	m, err := New(Config{
		Mode:      sandbox.ModeVM,
		ImagesDir: t.TempDir(),
		Docker:    docker,
		Repo:      noPublishedImages,
		Init:      func() ([]byte, error) { return []byte("#!runplane-init"), nil },
		Logger:    testLogger(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const noPublishedImages = "runplane/no-such-repo"

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
