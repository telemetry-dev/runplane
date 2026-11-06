//go:build linux

package host

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func readMemory() (memInfo, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return memInfo{}, fmt.Errorf("read meminfo: %w", err)
	}
	defer f.Close()
	return parseMeminfo(f)
}

func readCPU() (cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, fmt.Errorf("read cpu times: %w", err)
	}
	defer f.Close()
	return parseStat(f)
}

func osRelease() (pretty, id, version string) {
	for _, path := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		defer f.Close()
		r := parseOSRelease(f)
		return r["PRETTY_NAME"], r["ID"], r["VERSION_ID"]
	}
	return "Linux", "linux", ""
}

func diskSpace(path string) (total, free int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("read disk space of %s: %w", path, err)
	}
	return int64(st.Blocks) * int64(st.Frsize), int64(st.Bavail) * int64(st.Frsize), nil
}
