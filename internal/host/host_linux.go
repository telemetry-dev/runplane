//go:build linux

package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/google/nftables"
	"golang.org/x/sys/unix"
)

// nftDropChains returns the nftables base chains of the IPv4 input and
// forward hooks with a drop policy, as "family table chain", outside the
// tables where runplane can accept sandbox traffic: its own, and the filter
// table of iptables-nft. It returns nil if it cannot read nftables.
func nftDropChains() []string {
	c, err := nftables.New()
	if err != nil {
		return nil
	}
	chains, err := c.ListChains()
	if err != nil {
		return nil
	}
	var out []string
	for _, ch := range chains {
		if ch.Hooknum == nil || ch.Policy == nil || *ch.Policy != nftables.ChainPolicyDrop {
			continue
		}
		if h := *ch.Hooknum; h != *nftables.ChainHookInput && h != *nftables.ChainHookForward {
			continue
		}
		family := map[nftables.TableFamily]string{nftables.TableFamilyINet: "inet", nftables.TableFamilyIPv4: "ip"}[ch.Table.Family]
		if family == "" || ch.Table.Name == "runplane" || family == "ip" && ch.Table.Name == "filter" {
			continue
		}
		out = append(out, family+" "+ch.Table.Name+" "+ch.Name)
	}
	return out
}

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

func nftablesAvailable() bool {
	if exists("/sys/module/nf_tables") {
		return true
	}
	if os.Geteuid() != 0 {
		return false
	}
	// A netlink request makes the kernel load nf_tables when it is a module.
	c, err := nftables.New()
	if err != nil {
		return false
	}
	_, err = c.ListTables()
	return err == nil
}

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
			return pid, readTrimmed(filepath.Join(proc, "comm"))
		}
	}
	return 0, ""
}
