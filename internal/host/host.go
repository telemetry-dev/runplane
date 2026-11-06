// Package host reads the facts of the server, and samples its CPU, memory,
// and disk use.
package host

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Info describes a server. An agent sends it to the primary, and the servers
// table keeps it. Read does not read the versions of the sandbox software:
// the doctor adds them.
type Info struct {
	Hostname string
	OS       string // for example "Ubuntu 24.04.3 LTS"
	Kernel   string
	Arch     string // "x64" or "arm64"
	Resources
	DiskKind string // "NVMe", "SSD", "HDD", or empty if unknown
	KVM      bool   // /dev/kvm opens read-write

	DockerVersion      string // empty if Docker does not answer
	SysboxVersion      string
	FirecrackerVersion string

	IPv4     string // address of the default route interface
	Provider string // guess from DMI data and the host name
	Region   string // from the provider metadata service, when cheap
}

// Details are the facts of the server that only the doctor checks.
type Details struct {
	OSID      string // for example "ubuntu"
	OSVersion string // for example "24.04"
	CPUVirt   bool   // the CPU has the vmx or svm flag (x64 only)
	Virtual   bool   // the server itself is a VM
	KVMErr    error  // why /dev/kvm does not open
	CgroupV2  bool
	IPForward bool
	// ResourcesErr tells why the memory or the data disk cannot be read.
	ResourcesErr error
}

// Resources is the size of the server and its data disk.
type Resources struct {
	VCPU          int
	MemoryMB      int
	DiskBytes     int64
	FreeDiskBytes int64 // space available to programs, as df shows it
}

// Read reads the facts of this server. dataDir selects the data disk; if it
// does not exist yet, Read measures its nearest parent. Read asks the cloud
// metadata service, so it can take some milliseconds.
func Read(ctx context.Context, dataDir string) (Info, Details) {
	var d Details
	info := Info{Arch: Arch(), Kernel: kernelRelease()}
	info.Resources, d.ResourcesErr = ReadResources(dataDir)
	info.Hostname, _ = os.Hostname()
	info.OS, d.OSID, d.OSVersion = osRelease()

	flags := cpuFlags()
	d.CPUVirt = flags["vmx"] || flags["svm"]
	info.KVM, d.KVMErr = OpenKVM()
	board := readDMI(os.DirFS("/sys/class/dmi/id"))
	info.Provider = board.provider(info.Hostname)
	d.Virtual = flags["hypervisor"] || board.virtual()
	info.DiskKind = diskKind(dataDir, d.Virtual)
	d.CgroupV2 = exists("/sys/fs/cgroup/cgroup.controllers")
	d.IPForward = readTrimmed("/proc/sys/net/ipv4/ip_forward") == "1"
	info.IPv4 = primaryIPv4()
	info.Region = region(ctx, metadataURL, info.Provider)
	return info, d
}

// ReadResources reads the CPU count, memory, and data disk size. It is cheap.
func ReadResources(dataDir string) (Resources, error) {
	res, _, err := readResources(dataDir)
	return res, err
}

func readResources(dataDir string) (Resources, memInfo, error) {
	total, free, err := DiskSpace(dataDir)
	if err != nil {
		return Resources{}, memInfo{}, err
	}
	mem, err := readMemory()
	if err != nil {
		return Resources{}, memInfo{}, err
	}
	res := Resources{VCPU: runtime.NumCPU(), MemoryMB: int(mem.totalKB / 1024), DiskBytes: total, FreeDiskBytes: free}
	return res, mem, nil
}

// DiskSpace returns the size and the free bytes of the file system that
// holds path, or its nearest parent that exists.
func DiskSpace(path string) (total, free int64, err error) {
	return diskSpace(existingParent(path))
}

// Arch returns the runner name of the CPU architecture: "x64" or "arm64".
func Arch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}

func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	return unix.ByteSliceToString(u.Release[:])
}

// OpenKVM tells if /dev/kvm opens read-write, as the VMs need it, and why it
// does not.
func OpenKVM() (bool, error) {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	f.Close()
	return true, nil
}

// cpuFlags returns the flags of the first CPU in /proc/cpuinfo. Only x86
// kernels write a "flags" line.
func cpuFlags() map[string]bool {
	b, _ := os.ReadFile("/proc/cpuinfo")
	for line := range strings.Lines(string(b)) {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "flags" {
			continue
		}
		flags := map[string]bool{}
		for _, f := range strings.Fields(value) {
			flags[f] = true
		}
		return flags
	}
	return nil
}

// parseOSRelease reads the os-release format: KEY=value lines with optional
// quotes and backslash escapes.
func parseOSRelease(r io.Reader) map[string]string {
	fields := map[string]string{}
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
			value = value[1 : n-1]
		}
		var b strings.Builder
		for i := 0; i < len(value); i++ {
			if value[i] == '\\' && i+1 < len(value) {
				i++
			}
			b.WriteByte(value[i])
		}
		fields[key] = b.String()
	}
	return fields
}

type memInfo struct {
	totalKB, availableKB uint64
}

// parseMeminfo reads /proc/meminfo. Kernels before 3.14 have no
// MemAvailable, so it then adds the free memory and the page cache.
func parseMeminfo(r io.Reader) (memInfo, error) {
	values := map[string]uint64{}
	s := bufio.NewScanner(r)
	for s.Scan() {
		key, rest, _ := strings.Cut(s.Text(), ":")
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		if v, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
			values[key] = v
		}
	}
	m := memInfo{totalKB: values["MemTotal"]}
	if m.totalKB == 0 {
		return memInfo{}, fmt.Errorf("read meminfo: no MemTotal")
	}
	if v, ok := values["MemAvailable"]; ok {
		m.availableKB = v
	} else {
		m.availableKB = values["MemFree"] + values["Buffers"] + values["Cached"]
	}
	return m, nil
}

// diskKind names the type of the disk that holds path: "NVMe", "SSD", "HDD",
// or "" if unknown.
func diskKind(path string, virtual bool) string {
	var st unix.Stat_t
	if err := unix.Stat(existingParent(path), &st); err != nil {
		return ""
	}
	dev := uint64(st.Dev)
	return blockKind("/sys", fmt.Sprintf("/sys/dev/block/%d:%d", unix.Major(dev), unix.Minor(dev)), virtual)
}

// blockKind reads the kind of the block device at link in the sysfs tree sys.
func blockKind(sys, link string, virtual bool) string {
	dir, err := filepath.EvalSymlinks(link)
	if err != nil {
		return ""
	}
	if exists(filepath.Join(dir, "partition")) {
		dir = filepath.Dir(dir)
	}
	// LVM, dm-crypt, and RAID devices sit on other block devices.
	if slaves, _ := os.ReadDir(filepath.Join(dir, "slaves")); len(slaves) > 0 {
		return blockKind(sys, filepath.Join(sys, "class/block", slaves[0].Name()), virtual)
	}
	if strings.HasPrefix(filepath.Base(dir), "nvme") {
		return "NVMe"
	}
	switch readTrimmed(filepath.Join(dir, "queue/rotational")) {
	case "0":
		return "SSD"
	case "1":
		// Many hypervisors report virtual disks as rotational, so trust the
		// flag only on bare metal.
		if !virtual {
			return "HDD"
		}
	}
	return ""
}

// primaryIPv4 returns the first IPv4 address of the default route interface.
func primaryIPv4() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	ifi, err := net.InterfaceByName(defaultInterface(f))
	if err != nil {
		return ""
	}
	addrs, _ := ifi.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP.IsGlobalUnicast() {
			return ipn.IP.String()
		}
	}
	return ""
}

// defaultInterface returns the interface of the default IPv4 route with the
// lowest metric from the /proc/net/route table.
func defaultInterface(r io.Reader) string {
	const rtfUp = 0x1
	best, bestMetric := "", uint64(0)
	s := bufio.NewScanner(r)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		flags, err1 := strconv.ParseUint(f[3], 16, 32)
		metric, err2 := strconv.ParseUint(f[6], 10, 32)
		if err1 != nil || err2 != nil || flags&rtfUp == 0 {
			continue
		}
		if best == "" || metric < bestMetric {
			best, bestMetric = f[0], metric
		}
	}
	return best
}

// existingParent returns path or its nearest parent that exists, so that a
// check before the install still finds the disk.
func existingParent(path string) string {
	for !exists(path) && path != filepath.Dir(path) {
		path = filepath.Dir(path)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readTrimmed(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}
