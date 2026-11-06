//go:build linux

package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func clockSynced() (bool, error) {
	var tx unix.Timex
	if _, err := unix.Adjtimex(&tx); err != nil {
		return false, fmt.Errorf("read kernel clock state: %w", err)
	}
	// systemd uses this rule: synchronized when the maximum error is below 16 s.
	return tx.Maxerror < 16_000_000, nil
}

// portOwner finds the process that listens on a TCP port. It needs root to
// see the sockets of other users.
func portOwner(port int) (pid int, name string) {
	inodes := map[string]bool{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		for _, inode := range listenInodes(f, port) {
			inodes["socket:["+inode+"]"] = true
		}
		f.Close()
	}
	if len(inodes) == 0 {
		return 0, ""
	}
	procs, _ := filepath.Glob("/proc/[0-9]*")
	for _, proc := range procs {
		fds, _ := os.ReadDir(filepath.Join(proc, "fd"))
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(proc, "fd", fd.Name()))
			if err != nil || !inodes[target] {
				continue
			}
			pid, _ := strconv.Atoi(filepath.Base(proc))
			comm, _ := os.ReadFile(filepath.Join(proc, "comm"))
			return pid, strings.TrimSpace(string(comm))
		}
	}
	return 0, ""
}
