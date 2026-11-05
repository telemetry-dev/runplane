//go:build linux

package scratch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/atomicfile"
)

// Create makes a sparse ext4 disk of sizeGB at path. It replaces a file at
// path.
func (d *Disks) Create(path string, sizeGB int) error {
	tpl, err := d.template(sizeGB)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create scratch disk folder: %w", err)
	}
	if err := copyFile(tpl, path); err != nil {
		os.Remove(path)
		return fmt.Errorf("create scratch disk %s: %w", path, err)
	}
	return nil
}

// template returns the empty ext4 file of sizeGB and makes it first if it
// does not exist.
func (d *Disks) template(sizeGB int) (string, error) {
	path := filepath.Join(d.dir, fmt.Sprintf("ext4-%dg.img", sizeGB))
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		return "", fmt.Errorf("create template folder: %w", err)
	}
	f, err := atomicfile.Create(path, 0o600)
	if err != nil {
		return "", fmt.Errorf("create template: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(int64(sizeGB) << 30); err != nil {
		return "", fmt.Errorf("size template: %w", err)
	}
	// Without "nodiscard", mke2fs punches a hole over the whole file and
	// then knows that the inode tables read as zero. It marks them zeroed,
	// so the kernel does not write ~1 GB of zeros for each 64 GB at the
	// first mount of each copy, and the copies stay sparse.
	cmd := exec.Command("mkfs.ext4", "-F", "-q", "-m", "0", "-E", "lazy_itable_init=0,lazy_journal_init=1", f.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("mkfs.ext4: %w: %s", err, bytes.TrimSpace(out))
	}
	if err := f.Commit(); err != nil {
		return "", fmt.Errorf("save template: %w", err)
	}
	return path, nil
}

// Attach attaches the disk at path to a free loop device and mounts it at
// mountpoint. The loop device frees itself after the unmount.
func Attach(path, mountpoint string) error {
	if err := os.MkdirAll(mountpoint, 0o700); err != nil {
		return fmt.Errorf("create mountpoint: %w", err)
	}
	loop, err := configureLoop(path)
	if err != nil {
		// losetup also works without /dev/loop-control and on kernels
		// before 5.8, which have no LOOP_CONFIGURE.
		var lerr error
		if loop, lerr = losetup(path); lerr != nil {
			return fmt.Errorf("attach %s to a loop device: %w", path, errors.Join(err, lerr))
		}
	}
	defer loop.Close()
	if err := unix.Mount(loop.Name(), mountpoint, "ext4", unix.MS_NOATIME, ""); err != nil {
		return fmt.Errorf("mount %s at %s: %w", path, mountpoint, err)
	}
	return nil
}

// Detach unmounts the disk at mountpoint and frees its loop device. A
// mountpoint without a mount is not an error.
func Detach(mountpoint string) error {
	dev, err := mountSource(mountpoint)
	if err != nil || dev == "" {
		return err
	}
	// Keep the loop device open over the unmount, so that autoclear cannot
	// free it and give it to a different disk before LOOP_CLR_FD.
	loop, _ := os.Open(dev)
	if loop != nil {
		defer loop.Close()
	}
	err = unix.Unmount(mountpoint, 0)
	if errors.Is(err, unix.EBUSY) {
		err = unix.Unmount(mountpoint, unix.MNT_DETACH)
	}
	if err != nil {
		return fmt.Errorf("unmount %s: %w", mountpoint, err)
	}
	if loop == nil || !strings.HasPrefix(filepath.Base(dev), "loop") {
		return nil
	}
	// If the disk is still open (a lazy unmount), the kernel clears the
	// device when the last user closes it.
	if err := unix.IoctlSetInt(int(loop.Fd()), unix.LOOP_CLR_FD, 0); err != nil && !errors.Is(err, unix.ENXIO) {
		return fmt.Errorf("free loop device %s: %w", dev, err)
	}
	return nil
}

// Used returns the bytes in use on the file system mounted at mountpoint,
// such as a scratch disk.
func Used(mountpoint string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(mountpoint, &st); err != nil {
		return 0, fmt.Errorf("measure %s: %w", mountpoint, err)
	}
	return int64(st.Blocks-st.Bfree) * st.Frsize, nil
}

// Usage returns the bytes that the files under dir use on disk. It stays on
// the file system of dir and counts hard links once. Container mode uses it
// to measure the scratch volume of a sandbox that has no scratch disk.
func Usage(dir string) (int64, error) {
	var root unix.Stat_t
	if err := unix.Lstat(dir, &root); err != nil {
		return 0, fmt.Errorf("measure %s: %w", dir, err)
	}
	seen := map[uint64]bool{} // inodes with more than one link
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		var st unix.Stat_t
		if err == nil {
			err = unix.Lstat(path, &st)
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil // removed while we walk
		}
		if err != nil {
			return err
		}
		if st.Dev != root.Dev {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && st.Nlink > 1 {
			if seen[st.Ino] {
				return nil
			}
			seen[st.Ino] = true
		}
		total += st.Blocks * 512
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure %s: %w", dir, err)
	}
	return total, nil
}

// copyFile copies src to dst. It clones the file if the file system can
// (btrfs, XFS). Else it copies the data parts only, so holes stay holes.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) != nil {
		err = copySparse(out, in)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

func copySparse(out, in *os.File) error {
	info, err := in.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if err := out.Truncate(size); err != nil {
		return err
	}
	buf := make([]byte, 128<<10)
	zero := make([]byte, len(buf))
	fd := int(in.Fd())
	for off := int64(0); off < size; {
		start, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			return nil // only a hole is left
		}
		end := size
		if err != nil {
			start = off // no SEEK_DATA: read all, skip blocks of zeros
		} else if e, err := unix.Seek(fd, start, unix.SEEK_HOLE); err == nil {
			end = e
		}
		for pos := start; pos < end; {
			n, err := in.ReadAt(buf[:min(int64(len(buf)), end-pos)], pos)
			if n > 0 && !bytes.Equal(buf[:n], zero[:n]) {
				if _, err := out.WriteAt(buf[:n], pos); err != nil {
					return err
				}
			}
			if err != nil && err != io.EOF {
				return err
			}
			if n == 0 {
				break
			}
			pos += int64(n)
		}
		off = end
	}
	return nil
}

// configureLoop binds a free loop device with autoclear to the file with
// LOOP_CONFIGURE and returns the open device. Direct I/O keeps the data out
// of the page cache of the backing file; the kernel turns it off if the
// file system cannot do it.
func configureLoop(path string) (*os.File, error) {
	back, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer back.Close() // the loop device keeps its own reference
	ctl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer ctl.Close()
	cfg := unix.LoopConfig{Fd: uint32(back.Fd())}
	cfg.Info.Flags = unix.LO_FLAGS_AUTOCLEAR | unix.LO_FLAGS_DIRECT_IO
	copy(cfg.Info.File_name[:len(cfg.Info.File_name)-1], path)
	// A different process can take the free device first.
	for range 16 {
		n, err := unix.IoctlRetInt(int(ctl.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			return nil, fmt.Errorf("find free loop device: %w", err)
		}
		loop, err := openLoop(n)
		if err != nil {
			return nil, err
		}
		err = unix.IoctlLoopConfigure(int(loop.Fd()), &cfg)
		if err == nil {
			return loop, nil
		}
		loop.Close()
		if !errors.Is(err, unix.EBUSY) {
			return nil, fmt.Errorf("LOOP_CONFIGURE: %w", err)
		}
	}
	return nil, errors.New("no free loop device")
}

func losetup(path string) (*os.File, error) {
	out, err := exec.Command("losetup", "--find", "--show", "--direct-io=on", path).Output()
	if err != nil {
		return nil, fmt.Errorf("losetup: %w", err)
	}
	dev := strings.TrimSpace(string(out))
	loop, err := setAutoclear(dev)
	if err != nil {
		exec.Command("losetup", "--detach", dev).Run()
		return nil, fmt.Errorf("set autoclear on %s: %w", dev, err)
	}
	return loop, nil
}

func setAutoclear(dev string) (*os.File, error) {
	loop, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	info, err := unix.IoctlLoopGetStatus64(int(loop.Fd()))
	if err == nil {
		info.Flags |= unix.LO_FLAGS_AUTOCLEAR
		err = unix.IoctlLoopSetStatus64(int(loop.Fd()), info)
	}
	if err != nil {
		loop.Close()
		return nil, err
	}
	return loop, nil
}

// openLoop opens /dev/loop<n>. In a container, /dev can miss devices that
// the host made later; then it makes the node from the number in sysfs.
func openLoop(n int) (*os.File, error) {
	path := "/dev/loop" + strconv.Itoa(n)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if !errors.Is(err, fs.ErrNotExist) {
		return f, err
	}
	b, err := os.ReadFile(fmt.Sprintf("/sys/block/loop%d/dev", n))
	if err != nil {
		return nil, err
	}
	major, minor, _ := strings.Cut(strings.TrimSpace(string(b)), ":")
	maj, err1 := strconv.ParseUint(major, 10, 32)
	min, err2 := strconv.ParseUint(minor, 10, 32)
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("read loop device number: %w", err)
	}
	if err := unix.Mknod(path, unix.S_IFBLK|0o600, int(unix.Mkdev(uint32(maj), uint32(min)))); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("make %s: %w", path, err)
	}
	return os.OpenFile(path, os.O_RDWR, 0)
}

// mountSource returns the source device of the top mount at mountpoint, or
// "" if nothing is mounted there.
func mountSource(mountpoint string) (string, error) {
	abs, err := filepath.Abs(mountpoint)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	var source string
	for line := range strings.Lines(string(b)) {
		// id parent major:minor root mountpoint options [optional...] - fstype source superoptions
		fields := strings.Fields(line)
		sep := slices.Index(fields, "-")
		if len(fields) < 5 || sep < 0 || sep+2 >= len(fields) {
			continue
		}
		if unescape(fields[4]) == abs {
			source = unescape(fields[sep+2])
		}
	}
	return source, nil
}

// unescape decodes the octal escapes (\040 for a space) of mountinfo.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
