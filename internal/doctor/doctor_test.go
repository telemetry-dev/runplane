package doctor

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/sandbox"
)

func open(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// healthy returns the scan of a good host, and the options of a mode whose
// driver adds one check.
func healthy() (scan, Options) {
	s := scan{
		Host: &Host{
			Info: host.Info{
				OS: "Ubuntu 24.04.3 LTS", Kernel: "6.8.0-48-generic", Arch: "x64",
				Resources: host.Resources{VCPU: 8, MemoryMB: 16000, DiskBytes: 200e9, FreeDiskBytes: 150e9},
			},
			Details: host.Details{OSID: "ubuntu", OSVersion: "24.04", CgroupV2: true, IPForward: true},
			Docker:  Docker{Version: "29.5.1", Firewall: "iptables"},
			Root:    true, Systemd: true, Unit: "runplane", UnitState: "active",
		},
		clock: clock{synced: true, skew: 300 * time.Millisecond},
		ports: []portScan{{addr: ":8484"}},
	}
	o := Options{
		MinFreeDiskPercent: 15,
		Requirements: Requirements{
			Mode:   ModeCheck.OK("Test mode"),
			Docker: true,
			Checks: func(context.Context, *Host) []Check { return []Check{{ID: "driver", Name: "Driver", Status: StatusOK}} },
		},
	}
	return s, o
}

// A healthy host passes all checks. The dashboard and the command show the
// checks in this order.
func TestHealthyHost(t *testing.T) {
	s, o := healthy()
	var ids []string
	for _, c := range s.checks(t.Context(), o) {
		ids = append(ids, c.ID)
		if c.Status != StatusOK {
			t.Errorf("%s is %s: %s", c.ID, c.Status, c.Detail)
		}
	}
	if want := []string{"mode", "docker", "driver", "disk", "clock", "port_8484"}; !slices.Equal(ids, want) {
		t.Errorf("checks %v, want %v", ids, want)
	}
}

// wantFix fails when a check that is not ok has no fix, or a fix with a
// command: the UI shows the fix as text and the command with a copy button.
func wantFix(t *testing.T, c Check) {
	t.Helper()
	if c.Status != StatusOK && c.Fix == "" {
		t.Errorf("%s is %s without a fix", c.ID, c.Status)
	}
	if strings.Contains(c.Fix, "sudo ") {
		t.Errorf("%s fix %q holds a command; put it in Command", c.ID, c.Fix)
	}
}

func TestDoctorStatus(t *testing.T) {
	inUse := &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	tests := []struct {
		name   string
		change func(*scan, *Options)
		id     string
		want   Status
	}{
		{"disk below 15%", func(s *scan, _ *Options) { s.FreeDiskBytes = 28e9 }, "disk", StatusFail},
		{"disk at the setting", func(s *scan, _ *Options) { s.FreeDiskBytes = 30e9 }, "disk", StatusFail},
		{"disk below the setting", func(s *scan, o *Options) { s.FreeDiskBytes, o.MinFreeDiskPercent = 50e9, 30 }, "disk", StatusFail},
		{"disk above the setting but small", func(s *scan, _ *Options) { s.DiskBytes, s.FreeDiskBytes = 80e9, 18e9 }, "disk", StatusWarn},
		{"disk unreadable", func(s *scan, _ *Options) { s.DiskBytes, s.ResourcesErr = 0, errors.New("statfs: no such file") }, "disk", StatusFail},

		{"docker snap", func(s *scan, _ *Options) { s.dockerSnap = true }, "docker", StatusFail},
		{"docker down", func(s *scan, _ *Options) { s.Docker.Version, s.DockerErr = "", errors.New("connection refused") }, "docker", StatusFail},
		{"docker socket without root", func(s *scan, _ *Options) { s.Docker.Version, s.Root = "", false }, "docker", StatusWarn},
		{"no docker in a mode without it", func(s *scan, o *Options) { s.Docker.Version, o.Requirements.Docker = "", false }, "docker", StatusOK},

		{"clock 2 minutes ahead", func(s *scan, _ *Options) { s.clock.skew = 2 * time.Minute }, "clock", StatusFail},
		{"clock 90 s behind", func(s *scan, _ *Options) { s.clock.skew = -90 * time.Second }, "clock", StatusFail},
		{"clock 10 s ahead", func(s *scan, _ *Options) { s.clock.skew = 10 * time.Second }, "clock", StatusWarn},
		{"clock not synchronized", func(s *scan, _ *Options) { s.clock.synced = false }, "clock", StatusWarn},
		{"clock state unknown, skew small", func(s *scan, _ *Options) { s.clock.syncedErr = errors.New("no adjtimex") }, "clock", StatusOK},
		{"clock unknown", func(s *scan, _ *Options) {
			s.clock.syncedErr, s.clock.skewErr = errors.New("no adjtimex"), errors.New("offline")
		}, "clock", StatusWarn},

		{"port used by nginx", func(s *scan, _ *Options) { s.ports[0] = portScan{addr: ":8484", err: inUse, pid: 812, name: "nginx"} }, "port_8484", StatusFail},
		{"port used by runplane", func(s *scan, _ *Options) {
			s.ports[0] = portScan{addr: ":8484", err: inUse, pid: 900, name: "runplane"}
		}, "port_8484", StatusOK},
		{"port used by this process", func(s *scan, _ *Options) {
			s.ports[0] = portScan{addr: ":8484", err: inUse, pid: 901, name: "main", self: true}
		}, "port_8484", StatusOK},
		{"port used, owner hidden", func(s *scan, _ *Options) { s.ports[0], s.Root = portScan{addr: ":8484", err: inUse}, false }, "port_8484", StatusWarn},
		{"port used, owner unknown as root", func(s *scan, _ *Options) { s.ports[0] = portScan{addr: ":8484", err: inUse} }, "port_8484", StatusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, o := healthy()
			tt.change(&s, &o)
			checks := s.checks(t.Context(), o)
			i := slices.IndexFunc(checks, func(c Check) bool { return c.ID == tt.id })
			if i < 0 {
				t.Fatalf("no %s check", tt.id)
			}
			if c := checks[i]; c.Status != tt.want {
				t.Errorf("%s is %s (%s), want %s", tt.id, c.Status, c.Detail, tt.want)
			}
			wantFix(t, checks[i])
		})
	}
}

// The checks that more than one driver adds.
func TestHostChecks(t *testing.T) {
	tests := []struct {
		name  string
		check func(*Host) Check
		want  Status
	}{
		{"cgroup v1", func(h *Host) Check { h.CgroupV2 = false; return h.CgroupCheck() }, StatusFail},
		{"no nftables as root", func(h *Host) Check { return h.NFTablesCheck(false) }, StatusFail},
		{"no nftables without root", func(h *Host) Check { h.Root = false; return h.NFTablesCheck(false) }, StatusWarn},
		{"no ip forwarding", func(h *Host) Check { h.IPForward = false; return h.ForwardCheck() }, StatusFail},
		{"no systemd for a driver that needs it", func(h *Host) Check { h.Systemd = false; return h.SystemdCheck("which keeps VMs in scopes") }, StatusFail},
		{"no systemd", func(h *Host) Check { h.Systemd = false; return h.SystemdCheck("") }, StatusWarn},
		{"unit stopped", func(h *Host) Check { h.UnitState = "inactive"; return h.SystemdCheck("") }, StatusWarn},
		{"kernel version unreadable", func(h *Host) Check {
			h.Kernel = "custom"
			return h.KernelCheck(func(c Check, _ Version) Check { return c.OK("") })
		}, StatusWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := healthy()
			c := tt.check(s.Host)
			if c.Status != tt.want {
				t.Errorf("%s is %s (%s), want %s", c.ID, c.Status, c.Detail, tt.want)
			}
			wantFix(t, c)
		})
	}
}

// A probe that cannot run here warns; only a probe that ran and did not
// reach the internet fails, because a failed check fails the install.
func TestInternetCheck(t *testing.T) {
	probe := func(err error) func(context.Context) error {
		return func(context.Context) error { return err }
	}
	for _, tt := range []struct {
		name  string
		probe func(context.Context) error
		want  Status
	}{
		{"reached", probe(nil), StatusOK},
		{"no image yet", probe(sandbox.ErrNoProbe), StatusWarn},
		{"only the service can probe", nil, StatusWarn},
		{"no internet", probe(errors.New("Get \"https://api.github.com\": dial tcp 140.82.112.6:443: i/o timeout")), StatusFail},
	} {
		if c := internetCheck(t.Context(), tt.probe); c.Status != tt.want || c.Fix == "" && tt.want != StatusOK {
			t.Errorf("%s: %s %q (fix %q), want %s with a fix", tt.name, c.Status, c.Detail, c.Fix, tt.want)
		}
	}
}

func TestFailedModeStopsTheDoctor(t *testing.T) {
	ran := false
	r := Run(t.Context(), Options{
		Requirements: Requirements{Mode: ModeCheck.Fail(`Unknown mode "kvm"`, "Set RUNPLANE_MODE.")},
		Internet:     func(context.Context) error { ran = true; return nil },
	})
	if len(r.Checks) != 1 || r.Checks[0].ID != "mode" || !r.Failed() {
		t.Errorf("got %+v, want one failed mode check", r.Checks)
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

// The doctor of a running server checks its own ports and the ports of its
// driver, also where it cannot find the owner of a port, such as on macOS.
func TestDoctorOfARunningServerKnowsItsPorts(t *testing.T) {
	var addrs []string
	for range 2 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		addrs = append(addrs, l.Addr().String())
	}
	o := Options{DataDir: t.TempDir(), Listen: addrs[:1], Serving: true, Requirements: Requirements{Listen: addrs[1:]}}
	o.Docker = func(context.Context) (Docker, error) { return Docker{}, errors.New("no docker") }
	s := o.scan(t.Context())
	cs := portChecks(s.ports, s.Root, "")
	if len(cs) != 2 || cs[0].Status != StatusOK || cs[1].Status != StatusOK {
		t.Errorf("port checks = %+v, want two OK checks", cs)
	}
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

func TestBaseImageSource(t *testing.T) {
	const ref = "ghcr.io/acme/runplane/runner-ubuntu-24.04:latest"
	denied := errors.New("GET https://ghcr.io/v2/acme/runplane/runner-ubuntu-24.04/manifests/latest: DENIED")
	tests := []struct {
		name       string
		needDocker bool
		change     func(*Host)
		pullErr    error
		want       Status
		detail     string
	}{
		{"published image", false, func(h *Host) { h.Docker.Version = "" }, nil, StatusOK, "Pulls " + ref},
		{"builds with docker", false, func(*Host) {}, denied, StatusOK, "Builds the image"},
		{"no docker", false, func(h *Host) { h.Docker.Version = "" }, denied, StatusFail, "Docker is not installed"},
		{"docker unknown without root", false, func(h *Host) { h.Docker.Version, h.Root = "", false }, denied, StatusWarn, "Docker does not answer"},
		{"mode with docker builds", true, func(*Host) {}, denied, StatusOK, "Builds the image"},
		{"mode with docker, docker down", true, func(h *Host) { h.Docker.Version = "" }, denied, StatusFail, "Docker does not answer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := healthy()
			tt.change(s.Host)
			c := baseImageCheck(s.Host, tt.needDocker, ref, tt.pullErr)
			if c.Status != tt.want || !strings.Contains(c.Detail, tt.detail) {
				t.Errorf("%s: %q, want %s with %q", c.Status, c.Detail, tt.want, tt.detail)
			}
			if c.Status != StatusOK && c.Fix == "" {
				t.Errorf("%s without a fix", c.Status)
			}
		})
	}
}

func TestFormat(t *testing.T) {
	tests := []struct {
		checks []Check
		want   string
	}{
		{[]Check{
			{ID: "kvm", Name: "KVM", Status: StatusOK, Detail: "/dev/kvm opens read-write", Fix: "not shown", Command: "not shown"},
			{ID: "clock", Name: "Time synchronization", Status: StatusWarn, Detail: "The kernel clock is not synchronized",
				Fix: "Turn on time sync.", Command: "sudo timedatectl set-ntp true"},
			{ID: "sysbox", Name: "Sysbox", Status: StatusFail, Detail: "Sysbox is not installed", Fix: "Install Sysbox"},
		}, `✓ KVM                   /dev/kvm opens read-write
! Time synchronization  The kernel clock is not synchronized
                        Fix: Turn on time sync.
                        Run: sudo timedatectl set-ntp true
✗ Sysbox                Sysbox is not installed
                        Fix: Install Sysbox

1 failure, 1 warning.
`},
		{[]Check{{Name: "KVM", Status: StatusOK, Detail: "ok"}}, "✓ KVM  ok\n\nAll checks passed.\n"},
	}
	for _, tt := range tests {
		var b strings.Builder
		if err := Format(&b, tt.checks); err != nil {
			t.Fatal(err)
		}
		if b.String() != tt.want {
			t.Errorf("got\n%s\nwant\n%s", b.String(), tt.want)
		}
	}
}

func TestListenInodes(t *testing.T) {
	// Port 8484 (0x2124) has a listening socket and an established one.
	got := listenInodes(open(t, "testdata/proc/net-tcp"), 8484)
	if !slices.Equal(got, []string{"44817"}) {
		t.Errorf("got %v, want [44817]", got)
	}
}

func TestAtLeast(t *testing.T) {
	tests := []struct {
		have, want string
		ok         bool
	}{
		{"29.5.0", "29.5", true},
		{"29.10.1", "29.5", true}, // numbers, not text
		{"28.5.2", "29.5", false},
		{"30.0.0", "29.5", true},
		{"0.7.0", "0.7.1", false},
		{"0.7.1", "0.7.1", true},
		{"0.7.1-0.linux", "0.7.1", true},
		{"0.10.0", "0.7.1", true},
		{"0.6.7+dev", "0.7.1", false},
		{"v1.13.1", "1.13", true},
		{"6.8.0-48-generic", "5.12", true},
		{"5.4.0-200-generic", "5.12", false},
		{"", "0.7.1", false},
		{"unknown", "0.7.1", false},
		{"29", "29.5", false}, // a major number alone is not a version
	}
	for _, tt := range tests {
		if got := AtLeast(tt.have, tt.want); got != tt.ok {
			t.Errorf("AtLeast(%q, %q) = %v, want %v", tt.have, tt.want, got, tt.ok)
		}
	}
}

func TestFirstVersion(t *testing.T) {
	for file, want := range map[string]string{
		"firecracker": "1.13.1",
		"jailer":      "1.13.1",
		"sysbox-runc": "0.7.1",
	} {
		b, err := os.ReadFile("testdata/tools/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if got := firstVersion(string(b)); got != want {
			t.Errorf("%s: got %q, want %q", file, got, want)
		}
	}
}
