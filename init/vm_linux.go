//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/sandbox/control"
)

// Drives of a microVM: vda is the read-only root disk.
const (
	bundleDev  = "/dev/vdb" // read-only runner bundle
	scratchDev = "/dev/vdc" // sparse ext4, erased after the job
)

// detectVM reports VM mode: init is PID 1 and the kernel command line has
// runplane.mode=vm. It mounts /proc to read the command line.
func detectVM() (bool, error) {
	if os.Getpid() != 1 {
		return false, nil
	}
	if err := mountOnce("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return false, err
	}
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false, fmt.Errorf("read kernel command line: %w", err)
	}
	return slices.Contains(strings.Fields(string(cmdline)), "runplane.mode=vm"), nil
}

// setupVM makes the writable root of a microVM. The root disk is read-only
// and shared by all VMs of the image, so init puts an overlay on it with the
// upper layer on the scratch disk, moves into the overlay, and mounts the
// kernel file systems there.
func setupVM() error {
	// A move or pivot fails under a shared mount.
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	if err := mountOnce("devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755"); err != nil {
		return err
	}
	if err := mount("tmpfs", "/run", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755"); err != nil {
		return err
	}
	stage := "/run/runplane-init"
	disk, root := stage+"/scratch", stage+"/root"
	upper, work := disk+"/root/upper", disk+"/root/work"
	if err := mkdirs(disk, root); err != nil {
		return err
	}
	if err := mount(scratchDev, disk, "ext4", 0, ""); err != nil {
		return err
	}
	if err := mkdirs(upper, work); err != nil {
		return err
	}
	if err := mount("overlay", root, "overlay", 0, "lowerdir=/,upperdir="+upper+",workdir="+work); err != nil {
		return err
	}
	if err := mkdirs(root+scratchDir, root+"/.oldroot"); err != nil {
		return err
	}
	if err := unix.Mount(disk, root+scratchDir, "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move scratch disk: %w", err)
	}
	if err := pivot(root); err != nil {
		return err
	}

	mounts := []struct {
		src, target, fstype string
		flags               uintptr
		data                string
	}{
		{"proc", "/proc", "proc", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"sysfs", "/sys", "sysfs", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755"},
		{"devpts", "/dev/pts", "devpts", unix.MS_NOSUID | unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620,gid=5"},
		{"tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755"},
	}
	for _, m := range mounts {
		if err := mkdirs(m.target); err != nil {
			return err
		}
		if err := mount(m.src, m.target, m.fstype, m.flags, m.data); err != nil {
			return err
		}
	}
	// devtmpfs has only device nodes. Docker (runc) and systemd add these
	// links, and jobs need them: bash process substitution, <(...), reads
	// /dev/fd/N. A kernel has one devtmpfs, so a link can already exist.
	for name, target := range map[string]string{
		"/dev/fd":     "/proc/self/fd",
		"/dev/stdin":  "/proc/self/fd/0",
		"/dev/stdout": "/proc/self/fd/1",
		"/dev/stderr": "/proc/self/fd/2",
	} {
		if err := os.Symlink(target, name); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	// /tmp stays on the scratch disk through the root overlay: jobs write
	// large temporary files, and a tmpfs would take them from VM memory.
	if err := os.Chmod("/tmp", 0o777|os.ModeSticky); err != nil {
		return err
	}
	// Ctrl-Alt-Del (Firecracker SendCtrlAltDel) then sends SIGINT to init
	// instead of an instant reset, so the runner can stop. A failure only
	// keeps the instant reset, which also ends the VM.
	unix.Reboot(unix.LINUX_REBOOT_CMD_CAD_OFF)
	// PID 1 starts with a limit of 1024 open files; GitHub runners have 65536.
	return unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: 65536, Max: 65536})
}

// pivot makes root the new root mount and drops the old root.
func pivot(root string) error {
	if err := unix.Chdir(root); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", ".oldroot"); err != nil {
		return fmt.Errorf("pivot root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	if err := unix.Unmount("/.oldroot", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	return os.Remove("/.oldroot")
}

// configureVM applies the parts of the config that Docker does for a
// container: host name, /etc/hosts, DNS, and the loopback device.
func configureVM(cfg control.GuestConfig) error {
	if cfg.Hostname != "" {
		if err := unix.Sethostname([]byte(cfg.Hostname)); err != nil {
			return fmt.Errorf("set host name: %w", err)
		}
		if err := os.WriteFile("/etc/hostname", []byte(cfg.Hostname+"\n"), 0o644); err != nil {
			return err
		}
		hosts, err := os.ReadFile("/etc/hosts")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.WriteFile("/etc/hosts", etcHosts(hosts, cfg.Hostname), 0o644); err != nil {
			return err
		}
	}
	if len(cfg.DNS) > 0 {
		var b strings.Builder
		for _, ns := range cfg.DNS {
			fmt.Fprintf(&b, "nameserver %s\n", ns)
		}
		// The image can have a symlink to the systemd-resolved stub here.
		os.Remove("/etc/resolv.conf")
		if err := os.WriteFile("/etc/resolv.conf", []byte(b.String()), 0o644); err != nil {
			return err
		}
	}
	return loopbackUp()
}

func mountBundle(dir string) error {
	if err := mkdirs(dir); err != nil {
		return err
	}
	var errs []error
	for _, fstype := range []string{"ext4", "erofs", "squashfs"} {
		err := unix.Mount(bundleDev, dir, fstype, unix.MS_RDONLY, "")
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", fstype, err))
	}
	return fmt.Errorf("mount runner bundle %s: %w", bundleDev, errors.Join(errs...))
}

func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("read lo flags: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("set lo up: %w", err)
	}
	return nil
}

// powerOff ends the sandbox. In container mode, the exit of PID 1 stops the
// container. reboot returns only on failure.
func powerOff(vm bool, code int) {
	unix.Sync()
	if !vm {
		os.Exit(code)
	}
	// Firecracker on x86 has no power-off device. It exits when the guest
	// resets the CPU, which a reboot does with the kernel option reboot=k.
	cmd := unix.LINUX_REBOOT_CMD_POWER_OFF
	if runtime.GOARCH == "amd64" {
		cmd = unix.LINUX_REBOOT_CMD_RESTART
	}
	err := unix.Reboot(cmd)
	fmt.Fprintln(os.Stderr, "runplane-init: reboot:", err)
	// The exit of PID 1 panics the kernel, and panic=1 resets the VM.
	os.Exit(1)
}

func mount(src, target, fstype string, flags uintptr, data string) error {
	if err := unix.Mount(src, target, fstype, flags, data); err != nil {
		return fmt.Errorf("mount %s on %s: %w", fstype, target, err)
	}
	return nil
}

// mountOnce mounts unless something is mounted at target already.
func mountOnce(src, target, fstype string, flags uintptr, data string) error {
	if isMountPoint(target) {
		return nil
	}
	return mount(src, target, fstype, flags, data)
}

// isMountPoint reports if path is a mount point. Without /proc it reports
// false.
func isMountPoint(path string) bool {
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	path = filepath.Clean(path)
	for line := range strings.Lines(string(info)) {
		if f := strings.Fields(line); len(f) > 4 && f[4] == path {
			return true
		}
	}
	return false
}

func mkdirs(dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
