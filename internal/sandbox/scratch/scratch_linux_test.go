//go:build linux

package scratch

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These tests need root, loop devices, and mkfs.ext4. Run them in a
// privileged container.

func TestDiskLifecycle(t *testing.T) {
	needRoot(t)
	dir := t.TempDir()
	d := New(filepath.Join(dir, "templates"))
	disk := filepath.Join(dir, "sb", "scratch.ext4")
	if err := d.Create(disk, 2); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(disk, &st); err != nil {
		t.Fatal(err)
	}
	if st.Size != 2<<30 {
		t.Errorf("disk size = %d, want %d", st.Size, 2<<30)
	}
	if used := st.Blocks * 512; used > 32<<20 {
		t.Errorf("disk uses %d bytes, want a sparse file under 32 MiB", used)
	}
	// Inode tables that mkfs marks as zeroed need no writes at the first
	// mount, so the disk stays sparse in a VM too.
	out, err := exec.Command("dumpe2fs", disk).Output()
	if err != nil {
		t.Fatal(err)
	}
	groups := len(regexp.MustCompile(`(?m)^Group \d+:`).FindAll(out, -1))
	zeroed := bytes.Count(out, []byte("ITABLE_ZEROED"))
	if groups == 0 || zeroed != groups {
		t.Errorf("%d of %d groups have zeroed inode tables, want all", zeroed, groups)
	}

	mnt := filepath.Join(dir, "sb", "mnt")
	if err := Attach(disk, mnt); err != nil {
		t.Fatal(err)
	}
	var fsst unix.Statfs_t
	if err := unix.Statfs(mnt, &fsst); err != nil {
		t.Fatal(err)
	}
	if total := int64(fsst.Blocks) * fsst.Bsize; total < 19<<30/10 {
		t.Errorf("mounted file system has %d bytes, want about 2 GiB", total)
	}
	if err := os.MkdirAll(filepath.Join(mnt, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "work", "hello"), []byte("job data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Detach(mnt); err != nil {
		t.Fatal(err)
	}
	if src, _ := mountSource(mnt); src != "" {
		t.Fatalf("%s is still mounted from %s", mnt, src)
	}
	if loops := loopsAfterRelease(t, disk); len(loops) > 0 {
		t.Fatalf("loop devices %v still use the disk after Detach", loops)
	}

	if err := Attach(disk, mnt); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(mnt, "work", "hello")); err != nil || string(got) != "job data" {
		t.Errorf("file after a second Attach = %q (%v), want %q", got, err, "job data")
	}
	if err := Detach(mnt); err != nil {
		t.Fatal(err)
	}
	if err := Detach(mnt); err != nil {
		t.Errorf("Detach without a mount: %v", err)
	}
	if err := Remove(disk); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(disk); !os.IsNotExist(err) {
		t.Errorf("disk exists after Remove: %v", err)
	}
	if err := Remove(disk); err != nil {
		t.Errorf("Remove of a missing disk: %v", err)
	}
}

func TestDisksFromOneTemplateAreIndependent(t *testing.T) {
	needRoot(t)
	dir := t.TempDir()
	d := New(filepath.Join(dir, "templates"))
	a, b := filepath.Join(dir, "a.ext4"), filepath.Join(dir, "b.ext4")
	for _, p := range []string{a, b} {
		if err := d.Create(p, 1); err != nil {
			t.Fatal(err)
		}
	}
	mnt := filepath.Join(dir, "mnt")
	if err := Attach(a, mnt); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "secret"), []byte("job a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Detach(mnt); err != nil {
		t.Fatal(err)
	}
	if err := Attach(b, mnt); err != nil {
		t.Fatal(err)
	}
	defer Detach(mnt)
	if _, err := os.Stat(filepath.Join(mnt, "secret")); !os.IsNotExist(err) {
		t.Fatalf("disk b has the file of disk a: %v", err)
	}
}

func TestCreateClonesOnReflinkFileSystems(t *testing.T) {
	needRoot(t)
	if _, err := exec.LookPath("mkfs.xfs"); err != nil {
		t.Skip("needs mkfs.xfs")
	}
	dir := t.TempDir()
	img, mnt := filepath.Join(dir, "xfs.img"), filepath.Join(dir, "xfs")
	run(t, "truncate", "-s", "2G", img)
	run(t, "mkfs.xfs", "-q", "-m", "reflink=1", img)
	if err := os.Mkdir(mnt, 0o700); err != nil {
		t.Fatal(err)
	}
	run(t, "mount", "-o", "loop", img, mnt)
	defer run(t, "umount", mnt)

	disk := filepath.Join(mnt, "disk.ext4")
	if err := New(filepath.Join(mnt, "templates")).Create(disk, 1); err != nil {
		t.Fatal(err)
	}
	if out := run(t, "filefrag", "-v", disk); !strings.Contains(out, "shared") {
		t.Errorf("disk shares no extents with its template:\n%s", out)
	}
}

func TestLoopDeviceFreesItselfAfterUnmount(t *testing.T) {
	needRoot(t)
	dir := t.TempDir()
	disk, mnt := filepath.Join(dir, "d.ext4"), filepath.Join(dir, "mnt")
	if err := New(dir).Create(disk, 1); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mnt, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, attach := range map[string]func(string) (*os.File, error){"LOOP_CONFIGURE": configureLoop, "losetup": losetup} {
		loop, err := attach(disk)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		err = unix.Mount(loop.Name(), mnt, "ext4", 0, "")
		loop.Close()
		if err != nil {
			t.Fatalf("%s: mount: %v", name, err)
		}
		if err := unix.Unmount(mnt, 0); err != nil {
			t.Fatal(err)
		}
		if loops := loopsAfterRelease(t, disk); len(loops) > 0 {
			t.Errorf("%s: loop devices %v stay after the unmount", name, loops)
		}
	}
}

func TestUsageCountsBytesOnDiskOfOneFileSystem(t *testing.T) {
	needRoot(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data"), bytes.Repeat([]byte{0xab}, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "data"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	sparse, err := os.Create(filepath.Join(dir, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	sparse.Truncate(1 << 30)
	sparse.WriteAt(bytes.Repeat([]byte{1}, 4096), 0)
	sparse.Close()

	disk, mnt := filepath.Join(t.TempDir(), "d.ext4"), filepath.Join(dir, "mnt")
	if err := New(t.TempDir()).Create(disk, 1); err != nil {
		t.Fatal(err)
	}
	if err := Attach(disk, mnt); err != nil {
		t.Fatal(err)
	}
	defer Detach(mnt)
	if err := os.WriteFile(filepath.Join(mnt, "other"), bytes.Repeat([]byte{1}, 8<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Usage(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 1 MiB of data once, 4 KiB of the sparse file, a few directory
	// blocks. Not the hard link again, not 1 GiB, not the 8 MiB mount.
	if low, high := int64(1<<20+4096), int64(1<<20+4096+256<<10); got < low || got > high {
		t.Errorf("Usage = %d, want %d to %d", got, low, high)
	}
}

func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("needs mkfs.ext4")
	}
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// loopsAfterRelease waits up to 5 seconds for the loop devices of path to
// go, and returns the devices that stay. The kernel frees a loop device at
// its last close. udev opens a loop device to probe it after a writer closes
// it, thus the device can stay for some milliseconds after Detach.
func loopsAfterRelease(t *testing.T, path string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		loops := loopsOf(t, path)
		if len(loops) == 0 || time.Now().After(deadline) {
			return loops
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// loopsOf returns the loop devices with path as backing file.
func loopsOf(t *testing.T, path string) []string {
	t.Helper()
	files, err := filepath.Glob("/sys/block/loop*/loop/backing_file")
	if err != nil {
		t.Fatal(err)
	}
	var loops []string
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil && strings.TrimSpace(string(b)) == path {
			loops = append(loops, strings.Split(f, "/")[3])
		}
	}
	return loops
}
