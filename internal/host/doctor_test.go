package host

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

func iptablesFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/iptables/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// forwardAccept is the iptables of a Docker host with the FORWARD policy ACCEPT.
func forwardAccept(t *testing.T) string {
	return strings.Replace(iptablesFixture(t, "docker"), "-P FORWARD DROP", "-P FORWARD ACCEPT", 1)
}

// healthy returns the scan of a good host for mode.
func healthy(t *testing.T, mode sandbox.Mode) scan {
	s := scan{
		Info: Info{
			OS: "Ubuntu 24.04.3 LTS", OSID: "ubuntu", OSVersion: "24.04",
			Kernel: "6.8.0-48-generic", Arch: "x64",
			Resources: Resources{VCPU: 8, MemoryMB: 16000, DiskBytes: 200e9, FreeDiskBytes: 150e9},
			CgroupV2:  true, NFTables: true, IPForward: true,
			DockerVersion: "29.5.1", DockerFirewall: "iptables",
			SysboxVersion: "0.7.1", SysboxRuntime: true,
		},
		root:        true,
		clockSynced: true,
		skew:        300 * time.Millisecond,
		systemd:     true,
		unitState:   "active",
		sysboxUnit:  "active",
		iptables:    iptablesFixture(t, "docker"),
		ports:       []portScan{{addr: ":8484"}},
	}
	if mode == sandbox.ModeVM {
		s.DockerVersion, s.DockerFirewall, s.SysboxVersion, s.SysboxRuntime = "", "", "", false
		s.iptables = iptablesFixture(t, "plain")
		s.sysboxUnit = ""
		s.KVM, s.CPUVirt = true, true
		s.FirecrackerVersion, s.JailerVersion = "1.13.1", "1.13.1"
	}
	return s
}

func find(checks []Check, id string) (Check, bool) {
	for _, c := range checks {
		if c.ID == id {
			return c, true
		}
	}
	return Check{}, false
}

func TestHealthyHostPasses(t *testing.T) {
	for _, mode := range []sandbox.Mode{sandbox.ModeContainer, sandbox.ModeVM, sandbox.ModeDev} {
		checks := healthy(t, mode).checks(mode, Options{Unit: "runplane", GuestKernel: "/var/lib/runplane/kernel/vmlinux"})
		for _, c := range checks {
			if c.Status != StatusOK {
				t.Errorf("%s: %s is %s: %s", mode, c.ID, c.Status, c.Detail)
			}
		}
	}
}

// The dashboard and the command show the checks in this order.
func TestChecksForMode(t *testing.T) {
	tests := []struct {
		mode sandbox.Mode
		ids  []string
	}{
		{sandbox.ModeContainer, []string{"os", "kernel", "cgroup", "kvm", "nftables", "ip_forward", "disk", "docker", "sysbox", "firewall", "clock", "systemd", "port_8484"}},
		{sandbox.ModeVM, []string{"os", "kernel", "cgroup", "kvm", "nftables", "ip_forward", "disk", "docker", "firecracker", "guest_kernel", "firewall", "clock", "systemd", "port_8484"}},
		{sandbox.ModeDev, []string{"disk", "docker", "clock", "port_8484"}},
	}
	for _, tt := range tests {
		var ids []string
		for _, c := range healthy(t, tt.mode).checks(tt.mode, Options{}) {
			ids = append(ids, c.ID)
		}
		if !slices.Equal(ids, tt.ids) {
			t.Errorf("%s: checks %v, want %v", tt.mode, ids, tt.ids)
		}
	}
}

func TestDoctorStatus(t *testing.T) {
	const (
		container = sandbox.ModeContainer
		vm        = sandbox.ModeVM
		dev       = sandbox.ModeDev
	)
	inUse := &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	tests := []struct {
		name   string
		mode   sandbox.Mode
		change func(*scan)
		opts   Options
		id     string
		want   Status
	}{
		{"debian", container, func(s *scan) { s.OSID, s.OSVersion = "debian", "12" }, Options{}, "os", StatusWarn},
		{"debian in vm mode", vm, func(s *scan) { s.OSID, s.OSVersion = "debian", "12" }, Options{}, "os", StatusOK},
		{"kernel 5.4", container, func(s *scan) { s.Kernel = "5.4.0-200-generic" }, Options{}, "kernel", StatusFail},
		{"kernel 5.10 needs shiftfs", container, func(s *scan) { s.Kernel = "5.10.0-30-amd64" }, Options{}, "kernel", StatusWarn},
		{"kernel 5.15", container, func(s *scan) { s.Kernel = "5.15.0-1060-azure" }, Options{}, "kernel", StatusOK},
		{"kernel 5.4 for firecracker", vm, func(s *scan) { s.Kernel = "5.4.0-200-generic" }, Options{}, "kernel", StatusFail},
		{"kernel 5.10 for firecracker", vm, func(s *scan) { s.Kernel = "5.10.0-30-amd64" }, Options{}, "kernel", StatusOK},
		{"cgroup v1", container, func(s *scan) { s.CgroupV2 = false }, Options{}, "cgroup", StatusFail},
		{"cgroup v1 in vm mode", vm, func(s *scan) { s.CgroupV2 = false }, Options{}, "cgroup", StatusFail},

		{"no kvm in container mode", container, func(s *scan) { s.kvmErr = fs.ErrNotExist }, Options{}, "kvm", StatusOK},
		{"no kvm device", vm, func(s *scan) { s.KVM, s.CPUVirt, s.kvmErr = false, false, fs.ErrNotExist }, Options{}, "kvm", StatusFail},
		{"kvm module not loaded", vm, func(s *scan) { s.KVM, s.kvmErr = false, fs.ErrNotExist }, Options{}, "kvm", StatusFail},
		{"kvm without root", vm, func(s *scan) { s.KVM, s.kvmErr, s.root = false, fs.ErrPermission, false }, Options{}, "kvm", StatusWarn},
		{"kvm denied to root", vm, func(s *scan) { s.KVM, s.kvmErr = false, fs.ErrPermission }, Options{}, "kvm", StatusFail},
		{"nested kvm", vm, func(s *scan) { s.Virtual = true }, Options{}, "kvm", StatusOK},
		{"nested kvm on ovhcloud", vm, func(s *scan) { s.Virtual, s.Provider = true, providerOVH }, Options{}, "kvm", StatusWarn},

		{"no nftables as root", container, func(s *scan) { s.NFTables = false }, Options{}, "nftables", StatusFail},
		{"no nftables without root", container, func(s *scan) { s.NFTables, s.root = false, false }, Options{}, "nftables", StatusWarn},
		{"no ip forwarding", vm, func(s *scan) { s.IPForward = false }, Options{}, "ip_forward", StatusFail},

		{"disk below 15%", container, func(s *scan) { s.FreeDiskBytes = 28e9 }, Options{}, "disk", StatusFail},
		{"disk below the setting", container, func(s *scan) { s.FreeDiskBytes = 50e9 }, Options{MinFreeDiskPercent: 30}, "disk", StatusFail},
		{"disk above the setting but small", container, func(s *scan) { s.DiskBytes, s.FreeDiskBytes = 80e9, 18e9 }, Options{}, "disk", StatusWarn},
		{"disk unreadable", dev, func(s *scan) { s.DiskBytes, s.resourcesErr = 0, errors.New("statfs: no such file") }, Options{}, "disk", StatusFail},

		{"docker snap", container, func(s *scan) { s.DockerSnap = true }, Options{}, "docker", StatusFail},
		{"docker snap in dev mode", dev, func(s *scan) { s.DockerSnap = true }, Options{}, "docker", StatusFail},
		{"docker down", container, func(s *scan) { s.DockerVersion, s.dockerErr = "", errors.New("connection refused") }, Options{}, "docker", StatusFail},
		{"docker socket without root", container, func(s *scan) { s.DockerVersion, s.root = "", false }, Options{}, "docker", StatusWarn},
		{"no docker in vm mode", vm, func(s *scan) { s.DockerVersion = "" }, Options{}, "docker", StatusOK},

		{"no sysbox", container, func(s *scan) { s.SysboxVersion = "" }, Options{}, "sysbox", StatusFail},
		{"sysbox 0.7.0 with docker 29.5", container, func(s *scan) { s.SysboxVersion = "0.7.0" }, Options{}, "sysbox", StatusFail},
		{"sysbox 0.7.0 with docker 29.4", container, func(s *scan) { s.SysboxVersion, s.DockerVersion = "0.7.0", "29.4.2" }, Options{}, "sysbox", StatusWarn},
		{"sysbox 0.10.0", container, func(s *scan) { s.SysboxVersion = "0.10.0" }, Options{}, "sysbox", StatusOK},
		{"sysbox runtime missing", container, func(s *scan) { s.SysboxRuntime = false }, Options{}, "sysbox", StatusFail},
		{"sysbox service stopped", container, func(s *scan) { s.sysboxUnit = "inactive" }, Options{}, "sysbox", StatusFail},

		{"firecracker missing", vm, func(s *scan) { s.FirecrackerVersion = "" }, Options{}, "firecracker", StatusFail},
		{"jailer missing", vm, func(s *scan) { s.JailerVersion = "" }, Options{}, "firecracker", StatusFail},
		{"jailer from another release", vm, func(s *scan) { s.JailerVersion = "1.12.0" }, Options{}, "firecracker", StatusFail},
		{"guest kernel missing", vm, func(s *scan) { s.guestKernelErr = fs.ErrNotExist }, Options{}, "guest_kernel", StatusFail},

		{"docker nftables backend", container, func(s *scan) { s.DockerFirewall = "nftables" }, Options{}, "firewall", StatusFail},
		{"no DOCKER-USER chain", container, func(s *scan) { s.iptables = iptablesFixture(t, "plain") }, Options{}, "firewall", StatusFail},
		{"firewall without root", container, func(s *scan) { s.iptables, s.iptablesErr = "", errNeedRoot }, Options{}, "firewall", StatusWarn},
		{"ufw drops by default, runplane accepts its VMs", vm, func(s *scan) { s.iptables = iptablesFixture(t, "ufw") }, Options{}, "firewall", StatusOK},
		{"nftables input chain drops in vm mode", vm, func(s *scan) { s.nftDrops = []string{"inet filter input"} }, Options{}, "firewall", StatusFail},
		{"nftables forward chain drops in container mode", container, func(s *scan) { s.nftDrops = []string{"inet filter forward"} }, Options{}, "firewall", StatusFail},
		{"docker drops forwarded VM traffic", vm, func(s *scan) { s.iptables = iptablesFixture(t, "docker") }, Options{}, "firewall", StatusOK},
		// Forwarding came on before Docker started, as from a sysctl file.
		{"docker kept the FORWARD policy ACCEPT", container, func(s *scan) { s.iptables = forwardAccept(t) }, Options{}, "firewall", StatusWarn},
		{"docker kept the FORWARD policy ACCEPT in vm mode", vm, func(s *scan) { s.iptables, s.DockerVersion = forwardAccept(t), "29.5.1" }, Options{}, "firewall", StatusWarn},
		{"FORWARD policy ACCEPT without forwarding", container, func(s *scan) { s.iptables, s.IPForward = forwardAccept(t), false }, Options{}, "firewall", StatusOK},
		{"no iptables in vm mode", vm, func(s *scan) { s.iptables, s.iptablesErr = "", fmt.Errorf("list: %w", exec.ErrNotFound) }, Options{}, "firewall", StatusOK},

		{"clock 2 minutes ahead", container, func(s *scan) { s.skew = 2 * time.Minute }, Options{}, "clock", StatusFail},
		{"clock 90 s behind", container, func(s *scan) { s.skew = -90 * time.Second }, Options{}, "clock", StatusFail},
		{"clock 10 s ahead", container, func(s *scan) { s.skew = 10 * time.Second }, Options{}, "clock", StatusWarn},
		{"clock not synchronized", container, func(s *scan) { s.clockSynced = false }, Options{}, "clock", StatusWarn},
		{"clock state unknown, skew small", dev, func(s *scan) { s.clockErr = errors.New("no adjtimex") }, Options{}, "clock", StatusOK},
		{"clock unknown", dev, func(s *scan) { s.clockErr, s.skewErr = errors.New("no adjtimex"), errors.New("offline") }, Options{}, "clock", StatusWarn},

		{"no systemd in vm mode", vm, func(s *scan) { s.systemd = false }, Options{}, "systemd", StatusFail},
		{"no systemd in container mode", container, func(s *scan) { s.systemd = false }, Options{}, "systemd", StatusWarn},
		{"unit stopped", container, func(s *scan) { s.unitState = "inactive" }, Options{Unit: "runplane"}, "systemd", StatusWarn},

		{"port used by nginx", container, func(s *scan) { s.ports[0] = portScan{addr: ":8484", err: inUse, pid: 812, name: "nginx"} }, Options{}, "port_8484", StatusFail},
		{"port used by runplane", container, func(s *scan) { s.ports[0] = portScan{addr: ":8484", err: inUse, pid: 900, name: "runplane"} }, Options{}, "port_8484", StatusOK},
		{"port used by this process", dev, func(s *scan) { s.ports[0] = portScan{addr: ":8484", err: inUse, pid: 901, name: "main", self: true} }, Options{}, "port_8484", StatusOK},
		{"port used, owner hidden", container, func(s *scan) { s.ports[0], s.root = portScan{addr: ":8484", err: inUse}, false }, Options{}, "port_8484", StatusWarn},
		{"port used, owner unknown as root", container, func(s *scan) { s.ports[0] = portScan{addr: ":8484", err: inUse} }, Options{}, "port_8484", StatusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := healthy(t, tt.mode)
			tt.change(&s)
			c, ok := find(s.checks(tt.mode, tt.opts), tt.id)
			if !ok {
				t.Fatalf("no %s check", tt.id)
			}
			if c.Status != tt.want {
				t.Errorf("%s is %s (%s), want %s", tt.id, c.Status, c.Detail, tt.want)
			}
			if c.Status != StatusOK && c.Fix == "" {
				t.Errorf("%s is %s without a fix", tt.id, c.Status)
			}
			// The UI shows the fix as text and the command with a copy button.
			if strings.Contains(c.Fix, "sudo ") {
				t.Errorf("%s fix %q holds a command; put it in Command", tt.id, c.Fix)
			}
		})
	}
}

// A probe that cannot run here warns; only a probe that ran and did not
// reach the internet fails, because a failed check fails the install.
func TestInternetCheck(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want Status
	}{
		{nil, StatusOK},
		{ErrNoProbe, StatusWarn},
		{ErrProbeInService, StatusWarn},
		{errors.New("Get \"https://api.github.com\": dial tcp 140.82.112.6:443: i/o timeout"), StatusFail},
	} {
		if c := internetCheck(tt.err); c.Status != tt.want || c.Fix == "" && tt.want != StatusOK {
			t.Errorf("%v: %s %q (fix %q), want %s with a fix", tt.err, c.Status, c.Detail, c.Fix, tt.want)
		}
	}
}

func TestDoctorUnknownMode(t *testing.T) {
	ran := false
	checks := Doctor(t.Context(), "kvm", Options{Internet: func(context.Context) error { ran = true; return nil }})
	if len(checks) != 1 || checks[0].ID != "mode" || checks[0].Status != StatusFail {
		t.Errorf("got %+v, want one failed mode check", checks)
	}
	if ran {
		t.Error("the internet probe ran for an unknown mode")
	}
}

func TestScanPortFindsThisProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("port owners come from /proc")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := scanPort(l.Addr().String(), false)
	if !errors.Is(p.err, syscall.EADDRINUSE) || !p.self || p.pid != os.Getpid() {
		t.Errorf("got %+v, want this process as the owner", p)
	}
}

// The doctor of a running server checks its own ports, also where it cannot
// find the owner of a port, such as on macOS.
func TestDoctorOfARunningServerKnowsItsPorts(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := inspect(t.Context(), sandbox.ModeDev, Options{DataDir: t.TempDir(), Listen: []string{l.Addr().String()}, Serving: true})
	cs := s.portChecks()
	if len(cs) != 1 || cs[0].Status != StatusOK {
		t.Errorf("port checks = %+v, want one OK check", cs)
	}
}

// fakeNTP answers each request with a clock that is offset from this one.
func fakeNTP(t *testing.T, offset time.Duration) string {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 48)
		for {
			_, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			now := time.Now().Add(offset)
			sec := uint64(now.Unix() + 2208988800)
			frac := uint64(now.Nanosecond()) << 32 / 1e9
			resp := make([]byte, 48)
			resp[0], resp[1] = 4<<3|4, 2 // version 4, server mode, stratum 2
			for _, at := range []int{32, 40} {
				binary.BigEndian.PutUint32(resp[at:], uint32(sec))
				binary.BigEndian.PutUint32(resp[at+4:], uint32(frac))
			}
			conn.WriteTo(resp, addr)
		}
	}()
	return conn.LocalAddr().String()
}

func TestNTPOffset(t *testing.T) {
	for _, serverAhead := range []time.Duration{30 * time.Second, -90 * time.Second, 0} {
		skew, err := ntpOffset(t.Context(), fakeNTP(t, serverAhead))
		if err != nil {
			t.Fatal(err)
		}
		// This clock is ahead when the server is behind.
		if d := skew + serverAhead; d.Abs() > 50*time.Millisecond {
			t.Errorf("server ahead %s: got skew %s", serverAhead, skew)
		}
	}
}

func TestBaseImageSource(t *testing.T) {
	const ref = "ghcr.io/acme/runplane/runner-ubuntu-24.04:latest"
	denied := errors.New("GET https://ghcr.io/v2/acme/runplane/runner-ubuntu-24.04/manifests/latest: DENIED")
	tests := []struct {
		name    string
		mode    sandbox.Mode
		change  func(*scan)
		pullErr error
		want    Status
		detail  string
	}{
		{"published image", sandbox.ModeVM, func(*scan) {}, nil, StatusOK, "Pulls " + ref},
		{"vm mode builds with docker", sandbox.ModeVM, func(s *scan) { s.DockerVersion = "29.5.1" }, denied, StatusOK, "Builds the image"},
		{"vm mode without docker", sandbox.ModeVM, func(*scan) {}, denied, StatusFail, "Docker is not installed"},
		{"vm mode, docker unknown without root", sandbox.ModeVM, func(s *scan) { s.root = false }, denied, StatusWarn, "Docker does not answer"},
		{"container mode builds", sandbox.ModeContainer, func(*scan) {}, denied, StatusOK, "Builds the image"},
		{"container mode without docker", sandbox.ModeContainer, func(s *scan) { s.DockerVersion = "" }, denied, StatusFail, "Docker does not answer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := healthy(t, tt.mode)
			tt.change(&s)
			c := s.baseImageCheck(tt.mode, ref, tt.pullErr)
			if c.Status != tt.want || !strings.Contains(c.Detail, tt.detail) {
				t.Errorf("%s: %q, want %s with %q", c.Status, c.Detail, tt.want, tt.detail)
			}
			if c.Status != StatusOK && c.Fix == "" {
				t.Errorf("%s without a fix", c.Status)
			}
		})
	}
}
