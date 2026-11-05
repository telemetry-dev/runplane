package firecracker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/runplane/runplane/internal/sandbox/guest"
)

// Files in the chroot. Firecracker sees them at its root.
const (
	kernelFile  = "vmlinux"
	rootFile    = "rootfs.ext4"
	runnerFile  = "runner.ext4"
	scratchFile = "scratch.ext4"
	apiSocket   = "api.sock"
	vsockFile   = "v.sock"
)

// Firecracker forwards the connections of the guest to vsock port
// guest.VsockPort to controlSocket.
var controlSocket = vsockFile + "_" + strconv.Itoa(guest.VsockPort)

// jails holds the folder that the jailer makes for each VM.
func (d *Driver) jails() string {
	return filepath.Join(d.opts.JailDir, filepath.Base(d.firecracker))
}

// jailDir is the folder of one VM. Its root folder is the chroot.
func (d *Driver) jailDir(id string) string { return filepath.Join(d.jails(), id) }

func (d *Driver) chroot(id string) string { return filepath.Join(d.jailDir(id), "root") }

// jailerCommand starts the jailer in the systemd scope of the VM, so that
// the VM continues when runplane stops. Firecracker runs as uid and gid.
func (d *Driver) jailerCommand(id string, uid, gid int) *exec.Cmd {
	unit := unitName(id)
	args := []string{
		"--scope", "--quiet", "--collect",
		"--slice=" + slice,
		"--unit=" + unit,
		"--description=runplane sandbox " + id,
		// The metrics read memory.current of the scope.
		"--property=MemoryAccounting=yes",
	}
	if d.unit != "" {
		// When systemd stops the two, as at a host shutdown, it stops
		// runplane first. Close then powers the VM off cleanly, before
		// systemd kills what is left in the scope.
		args = append(args, "--property=Before="+d.unit)
	}
	args = append(args, "--",
		d.jailer,
		"--id", id,
		"--exec-file", d.firecracker,
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(gid),
		"--chroot-base-dir", d.opts.JailDir,
		// With cgroup v2 and no --cgroup values, the jailer only moves
		// itself to this cgroup: the scope that it is in already.
		"--cgroup-version", "2",
		"--parent-cgroup", slice+"/"+unit+".scope",
		"--",
		"--api-sock", "/"+apiSocket,
	)
	return exec.Command("systemd-run", args...)
}

// prepareChroot puts the files of the VM in the chroot. The read-only
// drives are hard links, so all VMs share one page cache for them. Only the
// VM with the IDs uid and gid can write the scratch disk.
func (d *Driver) prepareChroot(chroot string, uid, gid, diskGB int, root, runner string) error {
	if err := os.MkdirAll(chroot, 0o700); err != nil {
		return fmt.Errorf("create chroot: %w", err)
	}
	for _, f := range []struct{ name, src string }{
		{kernelFile, d.opts.Kernel},
		{rootFile, root},
		{runnerFile, runner},
	} {
		if err := linkFile(f.src, filepath.Join(chroot, f.name)); err != nil {
			return fmt.Errorf("put %s in the chroot: %w", f.name, err)
		}
	}
	disk := filepath.Join(chroot, scratchFile)
	if err := d.disks.Create(disk, diskGB); err != nil {
		return err
	}
	if err := os.Chown(disk, uid, gid); err != nil {
		return fmt.Errorf("give the scratch disk to the VM: %w", err)
	}
	return nil
}

// linkFile makes a hard link, or a copy when dst is on another file system.
func linkFile(src, dst string) error {
	err := os.Link(src, dst)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// idRange gives each VM its own uid and gid: the start of the range plus
// the index of the TAP lease. Thus the Firecracker process of one VM cannot
// signal, trace, or open the files of another VM. The ranges come from
// /etc/subuid and /etc/subgid, where install.sh reserves them for the
// jailer user.
type idRange struct {
	uid, gid int // the IDs of lease index 0
	count    int
}

func (r idRange) of(index int) (uid, gid int, err error) {
	if index < 0 || index >= r.count {
		return 0, 0, fmt.Errorf("firecracker: the jailer ID range has room for %d VMs", r.count)
	}
	return r.uid + index, r.gid + index, nil
}

func readIDRange(name, subuid, subgid string) (idRange, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return idRange{}, fmt.Errorf("find the jailer user: %w", err)
	}
	uid, uidCount, err := readSubIDs(subuid, u.Username, u.Uid)
	if err != nil {
		return idRange{}, err
	}
	gid, gidCount, err := readSubIDs(subgid, u.Username, u.Uid)
	if err != nil {
		return idRange{}, err
	}
	return idRange{uid: uid, gid: gid, count: min(uidCount, gidCount)}, nil
}

// readSubIDs returns the first range of a user in a subuid or subgid file.
// A line is "<name or uid>:<start>:<count>". A range that starts at 0 would
// give a VM the root ID, so it is an error.
func readSubIDs(path, name, uid string) (start, count int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("read the jailer ID range: %w", err)
	}
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSpace(line)
		fields := strings.Split(line, ":")
		if len(fields) != 3 || (fields[0] != name && fields[0] != uid) {
			continue
		}
		start, err1 := strconv.Atoi(fields[1])
		count, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || start <= 0 || count <= 0 {
			return 0, 0, fmt.Errorf("%s: the range %q of %s is not valid", path, line, name)
		}
		return start, count, nil
	}
	return 0, 0, fmt.Errorf("%s has no ID range for %s; run the install script again", path, name)
}
