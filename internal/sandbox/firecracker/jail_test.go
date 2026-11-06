package firecracker

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestPrepareChroot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs Linux, where scratch disks work")
	}
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("needs mkfs.ext4")
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

func TestJailerIDs(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	subuid, subgid := filepath.Join(dir, "subuid"), filepath.Join(dir, "subgid")
	writeFile(t, subuid, "ubuntu:100000:65536\n"+me.Username+":1879048192:16384\n")
	// The gid file names the user by its uid and has a smaller range.
	writeFile(t, subgid, "ubuntu:100000:65536\n"+me.Uid+":1879000000:8\n")

	r, err := readIDRange(me.Username, subuid, subgid)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ index, uid, gid int }{{0, 1879048192, 1879000000}, {7, 1879048199, 1879000007}} {
		uid, gid, err := r.of(tt.index)
		if err != nil || uid != tt.uid || gid != tt.gid {
			t.Errorf("lease %d: %d:%d, %v; want %d:%d", tt.index, uid, gid, err, tt.uid, tt.gid)
		}
	}
	// Two VMs must never share IDs, so a lease past the smaller range fails.
	if _, _, err := r.of(8); err == nil {
		t.Error("lease 8 got IDs from a gid range of 8")
	}

	for name, content := range map[string]string{
		"no range for the user": "ubuntu:100000:65536\n",
		"a range from root":     me.Username + ":0:65536\n",
		"a count of 0":          me.Username + ":1879048192:0\n",
	} {
		writeFile(t, subgid, content)
		if _, err := readIDRange(me.Username, subuid, subgid); err == nil || !strings.Contains(err.Error(), subgid) {
			t.Errorf("%s: got %v, want an error about %s", name, err, subgid)
		}
	}
}

func TestScopeIsOrderedBeforeTheService(t *testing.T) {
	tests := []struct {
		cgroup, before string
	}{
		{"0::/system.slice/runplane.service\n", "--property=Before=runplane.service"},
		{"0::/system.slice/runplane-agent.service\n", "--property=Before=runplane-agent.service"},
		// A test or a shell: no service to order the scopes against.
		{"0::/user.slice/user-1000.slice/session-3.scope\n", ""},
	}
	d := testDriver(t, t.TempDir(), &fakeHost{})
	for _, tt := range tests {
		d.unit = serviceUnit([]byte(tt.cgroup))
		args := d.jailerCommand("vm1", 1879048195, 1879048195).Args
		var before []string
		for _, a := range args[:slices.Index(args, "--")] {
			if strings.HasPrefix(a, "--property=Before=") {
				before = append(before, a)
			}
		}
		if strings.Join(before, " ") != tt.before {
			t.Errorf("%q: systemd-run orders the scope with %q, want %q", tt.cgroup, before, tt.before)
		}
	}
}
