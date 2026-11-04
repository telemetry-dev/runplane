//go:build linux

package image

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/runplane/runplane/internal/sandbox"
)

// TestRootDiskMounts mounts a root disk with the kernel, as the guest does.
// It needs root and loop devices, so it runs only with RUNPLANE_TEST_MOUNT=1.
func TestRootDiskMounts(t *testing.T) {
	if os.Getenv("RUNPLANE_TEST_MOUNT") != "1" {
		t.Skip("set RUNPLANE_TEST_MOUNT=1 to mount a root disk (needs root and loop devices)")
	}
	ref := pushImage(t, testImage(t, runtime.GOARCH))
	init := []byte("#!runplane-init")
	m, err := New(Config{Mode: sandbox.ModeVM, ImagesDir: t.TempDir(), Init: func() ([]byte, error) { return init, nil }})
	if err != nil {
		t.Fatal(err)
	}
	img, err := m.Ensure(context.Background(), ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	mnt := t.TempDir()
	if out, err := exec.Command("mount", "-t", "ext4", "-o", "loop,ro", img.Path, mnt).CombinedOutput(); err != nil {
		t.Fatalf("mount %s: %v\n%s", img.Path, err, out)
	}
	t.Cleanup(func() { exec.Command("umount", mnt).Run() })

	got, err := os.ReadFile(filepath.Join(mnt, "sbin", "runplane-init"))
	if err != nil || string(got) != string(init) {
		t.Errorf("/sbin/runplane-init = %q, %v; want %q", got, err, init)
	}
	if st, err := os.Stat(filepath.Join(mnt, "sbin", "runplane-init")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("/sbin/runplane-init mode: %v, %v", st.Mode(), err)
	}
	for _, p := range []string{"dev", "proc", "sys", "run", "tmp", "mnt/scratch"} {
		if st, err := os.Stat(filepath.Join(mnt, p)); err != nil || !st.IsDir() {
			t.Errorf("mount point /%s: %v", p, err)
		}
	}
}
