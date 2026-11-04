//go:build !linux

package host

import (
	"errors"
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// Other systems report their size and no use, so that development works.

func readMemory() (memInfo, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return memInfo{}, fmt.Errorf("read memory size: %w", err)
	}
	return memInfo{totalKB: total / 1024, availableKB: total / 1024}, nil
}

func readCPU() (cpuTimes, error) {
	return cpuTimes{}, nil
}

func osRelease() (pretty, id, version string) {
	version, _ = unix.Sysctl("kern.osproductversion")
	if runtime.GOOS == "darwin" {
		return "macOS " + version, "macos", version
	}
	return runtime.GOOS + " " + version, runtime.GOOS, version
}

func diskSpace(path string) (total, free int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("read disk space of %s: %w", path, err)
	}
	return int64(st.Blocks) * int64(st.Bsize), int64(st.Bavail) * int64(st.Bsize), nil
}

func nftablesAvailable() bool { return false }

func nftDropChains() []string { return nil }

func clockSynced() (bool, error) {
	return false, errors.New("no kernel clock state on " + runtime.GOOS)
}

func portOwner(int) (pid int, name string) { return 0, "" }
