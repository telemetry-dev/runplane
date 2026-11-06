//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/sandbox/guest"
)

// Jobs write caches and tools to the home folders and /opt. In container
// mode those folders are on the host disk, so init puts their writes on
// scratch and keeps what the image has there. The test mounts on /opt and
// /mnt/scratch of the machine, so it runs only in a throwaway container:
//
//	docker run --rm --privileged -e RUNPLANE_TEST_MOUNTS=1 ... ./init.test
func TestOverlayPutsWritesOnScratch(t *testing.T) {
	if os.Getenv("RUNPLANE_TEST_MOUNTS") != "1" || os.Geteuid() != 0 {
		t.Skip("set RUNPLANE_TEST_MOUNTS=1 and run as root in a throwaway container")
	}
	if err := mkdirs(guest.ScratchDir, "/opt/hostedtoolcache/node"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("tmpfs", guest.ScratchDir, "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Unmount("/opt", unix.MNT_DETACH); unix.Unmount(guest.ScratchDir, unix.MNT_DETACH) })
	if err := os.WriteFile("/opt/hostedtoolcache/node/image", []byte("from the image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod("/opt", 0o751); err != nil {
		t.Fatal(err)
	}

	if err := overlayOnScratch("/opt"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile("/opt/hostedtoolcache/node/image"); err != nil || string(b) != "from the image" {
		t.Errorf("image file through the overlay: %q, %v", b, err)
	}
	if err := os.WriteFile("/opt/hostedtoolcache/node/job", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(guest.ScratchDir, "root/opt/upper/hostedtoolcache/node/job")); err != nil {
		t.Errorf("a file that the job wrote is not on scratch: %v", err)
	}
	if st, err := os.Stat("/opt"); err != nil || st.Mode().Perm() != 0o751 {
		t.Errorf("/opt mode %v, %v; want 0751 as in the image", st.Mode().Perm(), err)
	}
	if err := overlayOnScratch("/no/such/folder"); err != nil {
		t.Errorf("overlay on a folder that the image does not have: %v, want nil", err)
	}
}
