// Package host reads facts about the server, runs the doctor checks, and
// samples CPU, memory, and disk use.
package host

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/client"
	"golang.org/x/sys/unix"
)

// Info describes a server. An agent sends it to the primary, and the servers
// table keeps most of the fields.
type Info struct {
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`         // for example "Ubuntu 24.04.3 LTS"
	OSID      string `json:"os_id"`      // for example "ubuntu"
	OSVersion string `json:"os_version"` // for example "24.04"
	Kernel    string `json:"kernel"`
	Arch      string `json:"arch"` // "x64" or "arm64"
	Resources
	DiskKind  string `json:"disk_kind"` // "NVMe", "SSD", "HDD", or empty if unknown
	KVM       bool   `json:"kvm"`       // /dev/kvm opens read-write
	CPUVirt   bool   `json:"cpu_virt"`  // the CPU has the vmx or svm flag (x64 only)
	Virtual   bool   `json:"virtual"`   // the server itself is a VM
	CgroupV2  bool   `json:"cgroup_v2"`
	NFTables  bool   `json:"nftables"`
	IPForward bool   `json:"ip_forward"`

	DockerVersion  string `json:"docker_version"`  // empty if Docker does not answer
	DockerSnap     bool   `json:"docker_snap"`     // Docker comes from the snap package
	DockerFirewall string `json:"docker_firewall"` // firewall backend of Docker, for example "iptables"
	SysboxVersion  string `json:"sysbox_version"`
	SysboxRuntime  bool   `json:"sysbox_runtime"` // docker info lists sysbox-runc

	FirecrackerVersion string `json:"firecracker_version"`
	JailerVersion      string `json:"jailer_version"`

	IPv4     string `json:"ipv4"`     // address of the default route interface
	Provider string `json:"provider"` // guess from DMI data and the host name
	Region   string `json:"region"`   // from the provider metadata service, when cheap
}

// Resources is the size of the server and its data disk.
type Resources struct {
	VCPU          int   `json:"vcpu"`
	MemoryMB      int   `json:"memory_mb"`
	DiskBytes     int64 `json:"disk_bytes"`
	FreeDiskBytes int64 `json:"free_disk_bytes"` // space available to programs, as df shows it
}

// Facts reads the facts of this server. dataDir selects the data disk. It
// does not need to exist yet: Facts then measures its nearest parent.
// Facts asks Docker, the Sysbox and Firecracker binaries, and the cloud
// metadata service, so it takes some milliseconds. The only error is a
// failed reading of the memory or the disk; the other facts are then still set.
func Facts(ctx context.Context, dataDir string) (Info, error) {
	info, n := gather(ctx, dataDir)
	return info, n.resourcesErr
}

// ReadResources reads the CPU count, memory, and data disk size. It is cheap.
func ReadResources(dataDir string) (Resources, error) {
	res, _, err := readResources(dataDir)
	return res, err
}

func readResources(dataDir string) (Resources, memInfo, error) {
	total, free, err := diskSpace(existingParent(dataDir))
	if err != nil {
		return Resources{}, memInfo{}, err
	}
	mem, err := readMemory()
	if err != nil {
		return Resources{}, memInfo{}, err
	}
	res := Resources{
		VCPU:          runtime.NumCPU(),
		MemoryMB:      int(mem.totalKB / 1024),
		DiskBytes:     total,
		FreeDiskBytes: free,
	}
	return res, mem, nil
}

// Arch returns the runner name of the CPU architecture: "x64" or "arm64".
func Arch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}

// notes keeps the reasons for some negative facts, for the doctor.
type notes struct {
	resourcesErr error
	kvmErr       error
	dockerErr    error
}

func gather(ctx context.Context, dataDir string) (Info, notes) {
	var n notes
	info := Info{Arch: Arch(), Kernel: kernelRelease()}
	info.Resources, n.resourcesErr = ReadResources(dataDir)
	info.Hostname, _ = os.Hostname()
	info.OS, info.OSID, info.OSVersion = osRelease()

	flags := cpuFlags()
	info.CPUVirt = flags["vmx"] || flags["svm"]
	info.KVM, n.kvmErr = openKVM()
	board := readDMI(os.DirFS("/sys/class/dmi/id"))
	info.Provider = board.provider(info.Hostname)
	info.Virtual = flags["hypervisor"] || board.virtual()
	info.DiskKind = diskKind(dataDir, info.Virtual)
	info.CgroupV2 = exists("/sys/fs/cgroup/cgroup.controllers")
	info.NFTables = nftablesAvailable()
	info.IPForward = readTrimmed("/proc/sys/net/ipv4/ip_forward") == "1"
	info.IPv4 = primaryIPv4()

	d, err := readDocker(ctx)
	n.dockerErr = err
	info.DockerVersion = d.version
	info.DockerFirewall = d.firewall
	info.SysboxRuntime = d.sysbox
	// A snap install conflicts with Docker Engine even when it does not answer.
	info.DockerSnap = d.snap || exists("/snap/docker")
	info.SysboxVersion = toolVersion(ctx, cmp.Or(d.sysboxPath, "sysbox-runc"))
	info.FirecrackerVersion = toolVersion(ctx, "firecracker")
	info.JailerVersion = toolVersion(ctx, "jailer")

	info.Region = region(ctx, metadataURL, info.Provider)
	return info, n
}

type dockerFacts struct {
	version    string
	firewall   string
	snap       bool
	sysbox     bool // Docker lists the sysbox-runc runtime
	sysboxPath string
}

// readDocker asks the Docker Engine API at DOCKER_HOST or the default socket.
func readDocker(ctx context.Context) (dockerFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := client.New(client.FromEnv)
	if err != nil {
		return dockerFacts{}, fmt.Errorf("make docker client: %w", err)
	}
	defer c.Close()
	res, err := c.Info(ctx, client.InfoOptions{})
	if err != nil {
		return dockerFacts{}, fmt.Errorf("read docker info: %w", err)
	}
	d := dockerFacts{
		version: res.Info.ServerVersion,
		snap:    strings.HasPrefix(res.Info.DockerRootDir, "/var/snap/"),
	}
	if fw := res.Info.FirewallBackend; fw != nil {
		d.firewall = fw.Driver
	}
	if rt, ok := res.Info.Runtimes["sysbox-runc"]; ok {
		d.sysbox, d.sysboxPath = true, rt.Path
	}
	return d, nil
}

// toolVersion runs "name --version" and returns the first version number in
// the output, or "" if the tool is missing.
func toolVersion(ctx context.Context, name string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, "--version").Output()
	if err != nil {
		return ""
	}
	return firstVersion(string(out))
}

func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	return unix.ByteSliceToString(u.Release[:])
}

func openKVM() (bool, error) {
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
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return nil
	}
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
		key, rest, ok := strings.Cut(s.Text(), ":")
		if !ok {
			continue
		}
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
	addrs, err := ifi.Addrs()
	if err != nil {
		return ""
	}
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
// DiskSpace returns the size and the free bytes of the file system that
// holds path, or its nearest parent that exists.
func DiskSpace(path string) (total, free int64, err error) {
	return diskSpace(existingParent(path))
}

func existingParent(path string) string {
	for {
		if _, err := os.Stat(path); err == nil || path == filepath.Dir(path) {
			return path
		}
		path = filepath.Dir(path)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
