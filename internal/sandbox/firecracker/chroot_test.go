package firecracker

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestPrepareChroot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("scratch disks need Linux")
	}
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("no mkfs.ext4")
	}
	root := t.TempDir()
	d := testDriver(t, root, &fakeHost{})
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid, gid = 1234, 1235
	}
	kernel := filepath.Join(root, "data", "kernel", "vmlinux")
	disk := filepath.Join(root, "data", "images", "abc-123.ext4")
	runner := filepath.Join(root, "data", "runners", "2.330.0-x64.ext4")
	for _, p := range []string{kernel, disk, runner} {
		writeFile(t, p, p)
	}
	d.opts.Kernel = kernel

	chroot := d.chroot("vm1")
	if err := d.prepareChroot(chroot, uid, gid, 1, disk, runner); err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{"vmlinux": kernel, "rootfs.ext4": disk, "runner.ext4": runner} {
		a, err := os.Stat(filepath.Join(chroot, name))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.Stat(src)
		if !os.SameFile(a, b) {
			t.Errorf("%s is not a hard link of %s", name, src)
		}
	}
	st, err := os.Stat(filepath.Join(chroot, "scratch.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 1<<30 {
		t.Errorf("scratch disk has %d bytes, want 1 GiB", st.Size())
	}
	sys := st.Sys().(*syscall.Stat_t)
	if int(sys.Uid) != uid || int(sys.Gid) != gid {
		t.Errorf("scratch disk owner %d:%d, want the IDs of the VM %d:%d", sys.Uid, sys.Gid, uid, gid)
	}
}

func TestPrepareChrootNamesAMissingFile(t *testing.T) {
	root := t.TempDir()
	d := testDriver(t, root, &fakeHost{})
	d.opts.Kernel = filepath.Join(root, "no-kernel")
	disk := filepath.Join(root, "disk.ext4")
	writeFile(t, disk, "")
	err := d.prepareChroot(d.chroot("vm1"), os.Getuid(), os.Getgid(), 1, disk, disk)
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "vmlinux") {
		t.Fatalf("got %v, want an error about the missing vmlinux", err)
	}
}
