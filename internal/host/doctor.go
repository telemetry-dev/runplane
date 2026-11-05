package host

import (
	"bufio"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

// Status is the result of a check.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

// Check is the result of one doctor check.
type Check struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Detail  string `json:"detail"`
	Fix     string `json:"fix,omitempty"`     // one sentence that says how to fix a check that is not ok
	Command string `json:"command,omitempty"` // one shell command that does the fix, if one can
}

// Options configure Doctor.
type Options struct {
	DataDir     string
	GuestKernel string   // the guest kernel file of VM mode
	Listen      []string // TCP addresses of runplane, for example ":8484"
	// Serving tells that this process listens on Listen, as the doctor of a
	// running server does. Then an address in use needs no owner lookup,
	// which works only on Linux.
	Serving            bool
	MinFreeDiskPercent float64 // the min_free_disk_percent setting; 0 means 15
	Unit               string  // systemd unit of runplane, for example "runplane"; empty skips its state
	// Internet starts a sandbox that connects to the internet.
	Internet func(ctx context.Context) error
	// BaseImage tries to pull the default builtin image, and returns its
	// registry ref and the error of the pull.
	BaseImage func(ctx context.Context) (ref string, err error)
}

// Doctor reads this server and checks it for mode. Some checks need root;
// without root they warn.
func Doctor(ctx context.Context, mode sandbox.Mode, opts Options) []Check {
	first := modeCheck(mode)
	if first.Status == StatusFail {
		return []Check{first}
	}
	s := inspect(ctx, mode, opts)
	ref, pullErr := opts.BaseImage(ctx)
	checks := append([]Check{first}, s.checks()...)
	return append(checks, s.baseImageCheck(ref, pullErr), internetCheck(opts.Internet(ctx)))
}

// FormatDoctor writes the checks for a terminal: one line for each check
// with a status mark, the fix and its command below a check that is not ok,
// and a summary.
func FormatDoctor(w io.Writer, checks []Check) error {
	marks := map[Status]string{StatusOK: "✓", StatusWarn: "!", StatusFail: "✗"}
	width := 0
	for _, c := range checks {
		width = max(width, len(c.Name))
	}
	var b strings.Builder
	count := map[Status]int{}
	for _, c := range checks {
		count[c.Status]++
		fmt.Fprintf(&b, "%s %-*s  %s\n", marks[c.Status], width, c.Name, c.Detail)
		if c.Status != StatusOK && c.Fix != "" {
			fmt.Fprintf(&b, "  %-*s  Fix: %s\n", width, "", c.Fix)
		}
		if c.Status != StatusOK && c.Command != "" {
			fmt.Fprintf(&b, "  %-*s  Run: %s\n", width, "", c.Command)
		}
	}
	if count[StatusFail]+count[StatusWarn] == 0 {
		b.WriteString("\nAll checks passed.\n")
	} else {
		fmt.Fprintf(&b, "\n%s, %s.\n", plural(count[StatusFail], "failure"), plural(count[StatusWarn], "warning"))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// The doctor measures the clock against an NTP server, because GitHub
// rejects app tokens from a server with a wrong clock. The Date header of
// GitHub can come from a cache, so it is seconds old.
const ntpServer = "time.cloudflare.com:123"

const (
	minSysbox         = "0.7.1"
	dockerTimeNSStart = "29.5" // Docker 29.5 puts containers in time namespaces
	minFreeDiskBytes  = 20e9
)

var errNeedRoot = errors.New("needs root")

// scan holds what the doctor reads for a mode, in addition to Info.
type scan struct {
	Info
	notes
	mode sandbox.Mode
	opts Options
	root bool

	clockSynced bool
	clockErr    error // nil when clockSynced is known
	skew        time.Duration
	skewErr     error

	systemd    bool
	unitState  string // output of systemctl is-active for Options.Unit
	sysboxUnit string // the same for the sysbox service

	iptables    string // iptables -S output
	iptablesErr error
	nftDrops    []string // base chains that drop sandbox traffic, see nftDropChains

	guestKernelErr error
	ports          []portScan
}

type portScan struct {
	addr string
	err  error // from listening on addr; nil when the port is free
	pid  int   // the process that listens, when known
	name string
	self bool // this process listens
}

func inspect(ctx context.Context, mode sandbox.Mode, opts Options) scan {
	info, n := gather(ctx, opts.DataDir)
	s := scan{Info: info, notes: n, mode: mode, opts: opts, root: os.Geteuid() == 0}
	s.clockSynced, s.clockErr = clockSynced()
	s.skew, s.skewErr = ntpOffset(ctx, ntpServer)
	s.systemd = exists("/run/systemd/system")
	if opts.Unit != "" {
		s.unitState = unitState(ctx, opts.Unit)
	}
	if mode == sandbox.ModeContainer {
		s.sysboxUnit = unitState(ctx, "sysbox")
	}
	if mode != sandbox.ModeDev {
		s.iptables, s.iptablesErr = readIptables(ctx, s.root)
		if s.root {
			s.nftDrops = nftDropChains()
		}
	}
	if mode == sandbox.ModeVM {
		s.guestKernelErr = checkKernel(opts.GuestKernel)
	}
	for _, addr := range opts.Listen {
		s.ports = append(s.ports, scanPort(addr, opts.Serving))
	}
	return s
}

// checks returns the checks for a valid mode, in the order that the
// dashboard and the command show them.
func (s scan) checks() []Check {
	var cs []Check
	if s.mode != sandbox.ModeDev {
		cs = append(cs, s.osCheck(), s.kernelCheck(), s.cgroupCheck(), s.kvmCheck(), s.nftablesCheck(), s.forwardCheck())
	}
	cs = append(cs, s.diskCheck(), s.dockerCheck())
	switch s.mode {
	case sandbox.ModeContainer:
		cs = append(cs, s.sysboxCheck())
		// Without Docker, the docker check tells the problem.
		if s.DockerVersion != "" {
			cs = append(cs, s.firewallCheck())
		}
	case sandbox.ModeVM:
		cs = append(cs, s.firecrackerCheck(), s.guestKernelCheck(), s.firewallCheck())
	}
	cs = append(cs, s.clockCheck())
	if s.mode != sandbox.ModeDev {
		cs = append(cs, s.systemdCheck())
	}
	return append(cs, s.portChecks()...)
}

func (c Check) ok(detail string) Check {
	c.Status, c.Detail = StatusOK, detail
	return c
}

func (c Check) warn(detail, fix string) Check {
	c.Status, c.Detail, c.Fix = StatusWarn, detail, fix
	return c
}

func (c Check) fail(detail, fix string) Check {
	c.Status, c.Detail, c.Fix = StatusFail, detail, fix
	return c
}

// run sets the command that does the fix.
func (c Check) run(command string) Check {
	c.Command = command
	return c
}

// asRoot is the fix of the checks that need root to read the server.
func (c Check) asRoot(detail string) Check {
	return c.warn(detail, "Run the doctor as root.").run("sudo runplane doctor")
}

func modeCheck(mode sandbox.Mode) Check {
	c := Check{ID: "mode", Name: "Mode"}
	switch mode {
	case sandbox.ModeContainer:
		return c.ok("Container mode (Sysbox): use it for trusted code only")
	case sandbox.ModeVM:
		return c.ok("VM mode (Firecracker)")
	case sandbox.ModeDev:
		return c.warn("Dev mode runs jobs in privileged Docker containers", "Set RUNPLANE_MODE to container or vm in /etc/runplane/runplane.env on a production server.")
	}
	return c.fail(fmt.Sprintf("Unknown mode %q", mode), "Set RUNPLANE_MODE to container, vm, or dev in /etc/runplane/runplane.env.")
}

func (s scan) osCheck() Check {
	c := Check{ID: "os", Name: "Operating system"}
	name := cmp.Or(s.OS, "Unknown")
	if s.mode == sandbox.ModeVM || s.OSID == "ubuntu" && (s.OSVersion == "22.04" || s.OSVersion == "24.04") {
		return c.ok(name)
	}
	return c.warn(name+": Sysbox supports only Ubuntu 22.04 and 24.04", "Use Ubuntu 24.04 for container mode.")
}

func (s scan) kernelCheck() Check {
	c := Check{ID: "kernel", Name: "Kernel"}
	v, ok := parseVersion(s.Kernel)
	if !ok {
		return c.warn(fmt.Sprintf("Cannot read the kernel version %q", s.Kernel), "Check that uname -r prints a version such as 6.8.0.").run("uname -r")
	}
	if s.mode == sandbox.ModeContainer {
		switch {
		case v.less(version{5, 5}):
			return c.fail(s.Kernel+": Sysbox needs kernel 5.5 or newer", "Upgrade the kernel to 5.15 or newer.")
		case v.less(version{5, 12}):
			return c.warn(s.Kernel+": Sysbox needs shiftfs on kernels before 5.12", "Upgrade the kernel to 5.15 or newer.")
		}
		return c.ok(s.Kernel)
	}
	if v.less(version{5, 10}) {
		return c.fail(s.Kernel+": Firecracker needs kernel 5.10 or newer", "Upgrade the kernel to 6.1 or newer.")
	}
	switch [2]int{v[0], v[1]} {
	case [2]int{5, 10}, [2]int{6, 1}, [2]int{6, 18}:
		return c.ok(s.Kernel)
	}
	return c.ok(s.Kernel + " (Firecracker tests host kernels 5.10, 6.1, and 6.18)")
}

func (s scan) cgroupCheck() Check {
	c := Check{ID: "cgroup", Name: "cgroup v2"}
	if s.CgroupV2 {
		return c.ok("Unified hierarchy at /sys/fs/cgroup")
	}
	return c.fail("The host uses cgroup v1", "Add systemd.unified_cgroup_hierarchy=1 to the kernel command line and reboot.")
}

func (s scan) kvmCheck() Check {
	c := Check{ID: "kvm", Name: "KVM"}
	if s.mode != sandbox.ModeVM {
		if s.KVM || errors.Is(s.kvmErr, fs.ErrPermission) {
			return c.ok("Available: VM mode gives stronger isolation")
		}
		return c.ok("Not available; container mode does not need it")
	}
	switch {
	case s.KVM && s.Virtual && s.Provider == providerOVH:
		return c.warn("/dev/kvm works, but a live migration on OVHcloud Public Cloud can crash nested VMs", "Use an OVHcloud bare metal server, or container mode.")
	case s.KVM && s.Virtual:
		return c.ok("/dev/kvm opens read-write (nested virtualization: VMs are slower than on bare metal)")
	case s.KVM:
		return c.ok("/dev/kvm opens read-write")
	case errors.Is(s.kvmErr, fs.ErrPermission) && !s.root:
		return c.asRoot("No permission to open /dev/kvm")
	case errors.Is(s.kvmErr, fs.ErrNotExist) && s.CPUVirt:
		return c.fail("/dev/kvm is missing, but the CPU supports virtualization", "Load the KVM module of the CPU: kvm_intel or kvm_amd.").
			run("sudo modprobe kvm_intel || sudo modprobe kvm_amd")
	case errors.Is(s.kvmErr, fs.ErrNotExist):
		return c.fail("/dev/kvm is missing", "Use container mode, or a server with KVM.")
	}
	return c.fail(fmt.Sprintf("Cannot open /dev/kvm: %v", s.kvmErr), "Use container mode, or a server with KVM.")
}

func (s scan) nftablesCheck() Check {
	c := Check{ID: "nftables", Name: "nftables"}
	switch {
	case s.NFTables:
		return c.ok("Available")
	case !s.root:
		return c.asRoot("Cannot check without root")
	}
	return c.fail("The kernel has no nf_tables support", "Load the nf_tables module.").run("sudo modprobe nf_tables")
}

// forwardCheck looks at IP forwarding. runplane turns it on when it starts,
// after Docker, and not with a sysctl file at boot: Docker sets the FORWARD
// policy to DROP only when it turns on forwarding itself.
func (s scan) forwardCheck() Check {
	c := Check{ID: "ip_forward", Name: "IP forwarding"}
	if s.IPForward {
		return c.ok("net.ipv4.ip_forward = 1")
	}
	return c.fail("net.ipv4.ip_forward = 0, so sandboxes have no internet access", "runplane turns on IP forwarding when it starts. Restart runplane.").
		run("sudo systemctl restart " + cmp.Or(s.opts.Unit, "runplane"))
}

func (s scan) diskCheck() Check {
	c := Check{ID: "disk", Name: "Free disk space"}
	if s.resourcesErr != nil || s.DiskBytes <= 0 {
		return c.fail(fmt.Sprintf("Cannot read the data disk: %v", s.resourcesErr), "Make sure that "+s.opts.DataDir+" is on a mounted disk.")
	}
	gigabytes := func(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/1e9) }
	minPercent := cmp.Or(s.opts.MinFreeDiskPercent, 15)
	freePercent := float64(s.FreeDiskBytes) * 100 / float64(s.DiskBytes)
	detail := fmt.Sprintf("%s free of %s (%.0f%%)", gigabytes(s.FreeDiskBytes), gigabytes(s.DiskBytes), freePercent)
	switch {
	case freePercent < minPercent:
		return c.fail(fmt.Sprintf("%s; runplane starts no sandbox below %g%% free", detail, minPercent), "Free disk space, or lower min_free_disk_percent.")
	case s.FreeDiskBytes < minFreeDiskBytes:
		return c.warn(detail+"; images and jobs need 20 GB or more", "Free disk space, or use a larger disk.")
	}
	return c.ok(detail)
}

func (s scan) dockerCheck() Check {
	c := Check{ID: "docker", Name: "Docker"}
	if s.mode == sandbox.ModeVM {
		if s.DockerVersion != "" {
			return c.ok("Docker Engine " + s.DockerVersion)
		}
		return c.ok("Not running; VM mode does not need Docker")
	}
	switch {
	case s.DockerSnap:
		return c.fail("Docker comes from the snap package, which runplane does not support", "Remove the Docker snap, then install Docker Engine.").
			run("sudo snap remove docker && curl -fsSL https://get.docker.com | sh")
	case s.DockerVersion == "" && !s.root:
		return c.asRoot(fmt.Sprintf("Docker does not answer: %v", s.dockerErr))
	case s.DockerVersion == "":
		return c.fail(fmt.Sprintf("Docker does not answer: %v", s.dockerErr), "Install Docker Engine.").run("curl -fsSL https://get.docker.com | sh")
	}
	return c.ok("Docker Engine " + s.DockerVersion)
}

func (s scan) sysboxCheck() Check {
	c := Check{ID: "sysbox", Name: "Sysbox"}
	install := "Install Sysbox " + minSysbox + " or newer from github.com/nestybox/sysbox/releases."
	switch {
	case s.SysboxVersion == "":
		return c.fail("Sysbox is not installed", install)
	case !atLeast(s.SysboxVersion, minSysbox) && atLeast(s.DockerVersion, dockerTimeNSStart):
		return c.fail(fmt.Sprintf("Sysbox %s cannot start containers with Docker %s, which uses time namespaces", s.SysboxVersion, s.DockerVersion), install)
	case !atLeast(s.SysboxVersion, minSysbox):
		return c.warn(fmt.Sprintf("Sysbox %s: Docker %s and newer need Sysbox %s", s.SysboxVersion, dockerTimeNSStart, minSysbox), install)
	case s.sysboxUnit != "" && s.sysboxUnit != "active":
		return c.fail("The sysbox service is "+s.sysboxUnit, "Start the sysbox service.").run("sudo systemctl enable --now sysbox")
	case s.DockerVersion != "" && !s.SysboxRuntime:
		return c.fail("Docker does not list the sysbox-runc runtime", "Restart Docker.").run("sudo systemctl restart docker")
	}
	return c.ok("Sysbox " + s.SysboxVersion)
}

func (s scan) firecrackerCheck() Check {
	c := Check{ID: "firecracker", Name: "Firecracker"}
	switch {
	case s.FirecrackerVersion == "":
		return c.fail("firecracker is not in PATH", "Run the install script again.")
	case s.JailerVersion == "":
		return c.fail("jailer is not in PATH", "Run the install script again.")
	case s.FirecrackerVersion != s.JailerVersion:
		return c.fail(fmt.Sprintf("firecracker %s and jailer %s differ", s.FirecrackerVersion, s.JailerVersion), "Install firecracker and jailer from the same release.")
	}
	return c.ok(fmt.Sprintf("Firecracker %s and jailer %s", s.FirecrackerVersion, s.JailerVersion))
}

func (s scan) guestKernelCheck() Check {
	c := Check{ID: "guest_kernel", Name: "Guest kernel"}
	if s.guestKernelErr != nil {
		return c.fail(fmt.Sprintf("Cannot use the guest kernel: %v", s.guestKernelErr), "Run the install script again to download the guest kernel.")
	}
	return c.ok(s.opts.GuestKernel)
}

func checkKernel(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() == 0 {
		return fmt.Errorf("%s is not a kernel file", path)
	}
	return nil
}

// firewallCheck looks at the firewall. Container mode needs the DOCKER-USER
// chain for its sandbox rules. runplane puts accept rules for its sandboxes
// at the top of INPUT and FORWARD of the iptables filter table, so a drop
// policy there, as from ufw, is fine. A drop policy of another nftables
// chain on these hooks, as from a hand-written ruleset, blocks the sandboxes
// whatever runplane accepts: in nftables, each base chain must accept.
func (s scan) firewallCheck() Check {
	c := Check{ID: "firewall", Name: "Firewall rules"}
	if len(s.nftDrops) > 0 {
		return c.fail(fmt.Sprintf("The nftables chain %s drops by default, so sandboxes cannot reach the internet or the cache", strings.Join(s.nftDrops, ", ")),
			"Accept the traffic of the sandbox interfaces in that chain: rp-* in VM mode, runplane0 in container mode.").
			run("sudo nft list ruleset")
	}
	policy, dockerUser := parseIptables(s.iptables)
	// Docker sets the FORWARD policy to DROP only when it turns on IP
	// forwarding itself, so an ACCEPT here means that forwarding was on
	// before Docker started. Without Docker, the policy is the choice of the
	// host firewall.
	openForward := policy != "DROP" && s.IPForward && s.DockerVersion != ""
	openForwardWarning := c.warn("The FORWARD policy is ACCEPT, so this server forwards packets between all its networks, not only for the sandboxes",
		"Set the policy to DROP. Docker and runplane accept the sandbox traffic with their own rules. "+
			"So that Docker sets DROP at each boot, remove net.ipv4.ip_forward from /etc/sysctl.conf and /etc/sysctl.d: runplane turns on forwarding after Docker starts.").
		run("sudo iptables -P FORWARD DROP")
	if s.mode == sandbox.ModeContainer {
		switch {
		case s.DockerFirewall == "nftables":
			return c.fail("Docker uses its nftables firewall backend, which has no DOCKER-USER chain", `Remove "firewall-backend" from /etc/docker/daemon.json and restart Docker.`)
		case errors.Is(s.iptablesErr, errNeedRoot):
			return c.asRoot("Cannot read the firewall without root")
		case s.iptablesErr != nil:
			return c.fail(fmt.Sprintf("Cannot read the iptables rules: %v", s.iptablesErr), "Install iptables.").run("sudo apt-get install -y iptables")
		case !dockerUser:
			return c.fail("The DOCKER-USER chain is missing, so Docker does not manage iptables", `Remove "iptables": false from /etc/docker/daemon.json and restart Docker.`)
		case openForward:
			return openForwardWarning
		}
		return c.ok("DOCKER-USER chain present")
	}
	switch {
	case errors.Is(s.iptablesErr, exec.ErrNotFound):
		return c.ok("No iptables rules")
	case errors.Is(s.iptablesErr, errNeedRoot):
		return c.asRoot("Cannot read the firewall without root")
	case s.iptablesErr != nil:
		return c.warn(fmt.Sprintf("Cannot read the iptables rules: %v", s.iptablesErr), "Check the policy of the FORWARD chain.").run("sudo iptables -S FORWARD")
	case openForward:
		return openForwardWarning
	case policy != "DROP":
		return c.ok("FORWARD policy " + cmp.Or(policy, "ACCEPT"))
	case dockerUser:
		return c.ok("Docker sets the FORWARD policy to DROP; runplane adds accept rules for its sandboxes")
	}
	return c.ok("The FORWARD policy is DROP; runplane adds accept rules for its sandboxes at the top of INPUT and FORWARD")
}

func readIptables(ctx context.Context, root bool) (string, error) {
	if !root {
		return "", errNeedRoot
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "iptables", "-w", "5", "-S").Output()
	if err != nil {
		return "", fmt.Errorf("run iptables: %w", err)
	}
	return string(out), nil
}

// parseIptables reads the policy of the FORWARD chain and whether the
// DOCKER-USER chain exists from "iptables -S" output.
func parseIptables(rules string) (forwardPolicy string, dockerUser bool) {
	for line := range strings.Lines(rules) {
		switch f := strings.Fields(line); {
		case len(f) == 3 && f[0] == "-P" && f[1] == "FORWARD":
			forwardPolicy = f[2]
		case len(f) == 2 && f[0] == "-N" && f[1] == "DOCKER-USER":
			dockerUser = true
		}
	}
	return forwardPolicy, dockerUser
}

func (s scan) clockCheck() Check {
	c := Check{ID: "clock", Name: "Time synchronization"}
	const fix, command = "Turn on time sync.", "sudo timedatectl set-ntp true"
	off := s.skew.Abs().Round(10 * time.Millisecond)
	side := "ahead"
	if s.skew < 0 {
		side = "behind"
	}
	switch {
	case s.skewErr == nil && off >= time.Minute:
		return c.fail(fmt.Sprintf("The clock is %s %s, so GitHub rejects the app tokens", off, side), fix).run(command)
	case s.clockErr == nil && !s.clockSynced:
		return c.warn("The kernel clock is not synchronized", fix).run(command)
	case s.skewErr == nil && off >= 5*time.Second:
		return c.warn(fmt.Sprintf("The clock is %s %s", off, side), fix).run(command)
	case s.skewErr == nil:
		return c.ok(fmt.Sprintf("Synchronized (%s %s)", off, side))
	case s.clockErr == nil:
		return c.ok("Synchronized")
	}
	return c.warn(fmt.Sprintf("Cannot check: %v", s.skewErr), "Allow NTP (UDP port 123) to the internet.")
}

// ntpOffset asks an NTP server for the time, as an SNTP client (RFC 4330).
// A positive offset means that this clock is ahead.
func ntpOffset(ctx context.Context, server string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", server)
	if err != nil {
		return 0, fmt.Errorf("ask %s for the time: %w", server, err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	req := make([]byte, 48)
	req[0] = 4<<3 | 3 // version 4, client mode
	sent := time.Now()
	if _, err := conn.Write(req); err != nil {
		return 0, fmt.Errorf("ask %s for the time: %w", server, err)
	}
	resp := make([]byte, 48)
	n, err := conn.Read(resp)
	received := time.Now()
	if err != nil {
		return 0, fmt.Errorf("read the time of %s: %w", server, err)
	}
	if n < 48 || resp[0]&7 != 4 || resp[1] == 0 { // server mode; stratum 0 is a refusal
		return 0, fmt.Errorf("read the time of %s: bad answer", server)
	}
	serverReceived, serverSent := ntpTime(resp[32:40]), ntpTime(resp[40:48])
	return -(serverReceived.Sub(sent) + serverSent.Sub(received)) / 2, nil
}

// ntpTime reads a 64-bit NTP timestamp. Seconds without the top bit belong
// to the era that starts in 2036.
func ntpTime(b []byte) time.Time {
	sec := int64(binary.BigEndian.Uint32(b[:4]))
	if sec&0x80000000 == 0 {
		sec += 1 << 32
	}
	frac := int64(binary.BigEndian.Uint32(b[4:]))
	const unixToNTP = 2208988800
	return time.Unix(sec-unixToNTP, frac*1e9>>32)
}

func (s scan) systemdCheck() Check {
	c := Check{ID: "systemd", Name: "systemd"}
	unit := s.opts.Unit
	switch {
	case !s.systemd && s.mode == sandbox.ModeVM:
		return c.fail("The host does not run systemd, which keeps VMs in scopes", "Use a Linux distribution with systemd.")
	case !s.systemd:
		return c.warn("The host does not run systemd", "Use a Linux distribution with systemd.")
	case unit == "":
		return c.ok("Running")
	case s.unitState == "active":
		return c.ok(unit + ".service is active")
	}
	return c.warn(fmt.Sprintf("%s.service is %s", unit, cmp.Or(s.unitState, "unknown")), "Start the "+unit+" service.").
		run("sudo systemctl enable --now " + unit)
}

func unitState(ctx context.Context, unit string) string {
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output()
	return strings.TrimSpace(string(out))
}

func (s scan) portChecks() []Check {
	var cs []Check
	for _, p := range s.ports {
		_, port, _ := net.SplitHostPort(p.addr)
		port = cmp.Or(port, p.addr)
		c := Check{ID: "port_" + port, Name: "Port " + port}
		inUse := errors.Is(p.err, syscall.EADDRINUSE)
		switch {
		case p.err == nil:
			c = c.ok(p.addr + " is free")
		case inUse && (p.self || p.name == "runplane"):
			c = c.ok("runplane listens on " + p.addr)
		case inUse && p.name != "":
			c = c.fail(fmt.Sprintf("%s (pid %d) listens on %s", p.name, p.pid, p.addr), "Stop "+p.name+", or give runplane a different port.")
		case inUse && !s.root:
			c = c.asRoot("Another process listens on " + p.addr)
		case inUse:
			c = c.fail("Another process listens on "+p.addr, "Stop that process, or give runplane a different port.")
		case errors.Is(p.err, syscall.EACCES):
			c = c.warn("Ports below 1024 need root", "Run runplane as root.")
		default:
			c = c.warn(fmt.Sprintf("Cannot listen on %s: %v", p.addr, p.err), "Check the listen address in /etc/runplane/runplane.env.")
		}
		cs = append(cs, c)
	}
	return cs
}

// scanPort tries to listen on addr. own tells that this process listens on
// it already.
func scanPort(addr string, own bool) portScan {
	p := portScan{addr: addr}
	l, err := net.Listen("tcp", addr)
	if err == nil {
		l.Close()
		return p
	}
	p.err = err
	if !errors.Is(err, syscall.EADDRINUSE) {
		return p
	}
	if own {
		p.self = true
		return p
	}
	_, port, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(port)
	p.pid, p.name = portOwner(n)
	p.self = p.pid == os.Getpid()
	return p
}

// listenInodes returns the inodes of the sockets that listen on port in a
// /proc/net/tcp or /proc/net/tcp6 table.
func listenInodes(r io.Reader, port int) []string {
	const tcpListen = "0A"
	var inodes []string
	s := bufio.NewScanner(r)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 10 || f[3] != tcpListen {
			continue
		}
		_, hexPort, _ := strings.Cut(f[1], ":")
		if p, err := strconv.ParseUint(hexPort, 16, 16); err == nil && int(p) == port {
			inodes = append(inodes, f[9])
		}
	}
	return inodes
}

// baseImageCheck tells where the default image comes from: a pull of ref,
// or a build of its Dockerfile with Docker when the pull fails (pullErr).
func (s scan) baseImageCheck(ref string, pullErr error) Check {
	c := Check{ID: "base_image", Name: "Base image source"}
	if pullErr == nil {
		return c.ok("Pulls " + ref)
	}
	why := pullErr.Error()
	if len(why) > 200 {
		why = why[:200] + "..."
	}
	switch {
	case s.DockerVersion != "":
		return c.ok(fmt.Sprintf("Builds the image from its Dockerfile with Docker, because %s cannot be pulled: %s", ref, why))
	case s.mode != sandbox.ModeVM:
		return c.fail(fmt.Sprintf("Cannot pull %s (%s), and Docker does not answer to build it", ref, why), "Fix the Docker check above.")
	case !s.root:
		return c.asRoot(fmt.Sprintf("Cannot pull %s (%s), and Docker does not answer to this user", ref, why))
	}
	return c.fail(fmt.Sprintf("Cannot pull %s (%s), and Docker is not installed to build it", ref, why),
		"Run the install script again: it installs Docker Engine for local image builds. Or make the image public on GHCR.")
}

func internetCheck(err error) Check {
	c := Check{ID: "internet", Name: "Sandbox internet access"}
	switch {
	case errors.Is(err, sandbox.ErrNoProbe):
		return c.warn(fmt.Sprintf("Not checked: %v.", err), "Run runplane doctor again when the default image is ready.")
	case errors.Is(err, sandbox.ErrProbeInService):
		return c.warn(fmt.Sprintf("Not checked here: %v.", err), "Open this server in the dashboard and select Run doctor.")
	case err != nil:
		return c.fail(fmt.Sprintf("A sandbox cannot reach the internet: %v", err), "Fix the IP forwarding and firewall checks above.")
	}
	return c.ok("A sandbox reached the internet")
}
