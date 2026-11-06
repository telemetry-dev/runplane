package sysbox

import (
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/host"
)

func healthyHost() containerHost {
	return containerHost{
		Host: &doctor.Host{
			Info: host.Info{
				OS: "Ubuntu 24.04.3 LTS", Kernel: "6.8.0-48-generic", Arch: "x64",
				SysboxVersion: "0.7.1",
			},
			Details: host.Details{OSID: "ubuntu", OSVersion: "24.04", CgroupV2: true, IPForward: true},
			Docker:  doctor.Docker{Version: "29.5.1", Firewall: "iptables", Runtimes: map[string]string{"runc": "runc", "sysbox-runc": "/usr/bin/sysbox-runc"}},
			Root:    true, Systemd: true, Unit: "runplane", UnitState: "active", NFTables: true,
		},
		sysboxUnit: "active",
		firewall:   doctor.Firewall{ForwardPolicy: "DROP", DockerUser: true},
	}
}

// A healthy host passes all checks of container mode, in this order.
func TestHealthyContainerHost(t *testing.T) {
	var ids []string
	for _, c := range healthyHost().checks() {
		ids = append(ids, c.ID)
		if c.Status != doctor.StatusOK {
			t.Errorf("%s is %s: %s", c.ID, c.Status, c.Detail)
		}
	}
	if want := []string{"os", "kernel", "cgroup", "kvm", "nftables", "ip_forward", "sysbox", "firewall", "systemd"}; !slices.Equal(ids, want) {
		t.Errorf("checks %v, want %v", ids, want)
	}
}

func TestContainerChecks(t *testing.T) {
	tests := []struct {
		name   string
		change func(*containerHost)
		id     string
		want   doctor.Status
	}{
		{"debian", func(h *containerHost) { h.OSID, h.OSVersion = "debian", "12" }, "os", doctor.StatusWarn},
		{"kernel 5.4", func(h *containerHost) { h.Kernel = "5.4.0-200-generic" }, "kernel", doctor.StatusFail},
		{"kernel 5.10 needs shiftfs", func(h *containerHost) { h.Kernel = "5.10.0-30-amd64" }, "kernel", doctor.StatusWarn},
		{"kernel 5.15", func(h *containerHost) { h.Kernel = "5.15.0-1060-azure" }, "kernel", doctor.StatusOK},
		{"no kvm", func(h *containerHost) { h.KVMErr = fs.ErrNotExist }, "kvm", doctor.StatusOK},
		{"no systemd", func(h *containerHost) { h.Systemd = false }, "systemd", doctor.StatusWarn},

		{"no sysbox", func(h *containerHost) { h.SysboxVersion = "" }, "sysbox", doctor.StatusFail},
		{"sysbox 0.7.0 with docker 29.5", func(h *containerHost) { h.SysboxVersion = "0.7.0" }, "sysbox", doctor.StatusFail},
		{"sysbox 0.7.0 with docker 29.4", func(h *containerHost) { h.SysboxVersion, h.Docker.Version = "0.7.0", "29.4.2" }, "sysbox", doctor.StatusWarn},
		{"sysbox 0.10.0", func(h *containerHost) { h.SysboxVersion = "0.10.0" }, "sysbox", doctor.StatusOK},
		{"sysbox runtime missing", func(h *containerHost) { delete(h.Docker.Runtimes, "sysbox-runc") }, "sysbox", doctor.StatusFail},
		{"sysbox service stopped", func(h *containerHost) { h.sysboxUnit = "inactive" }, "sysbox", doctor.StatusFail},

		{"docker nftables backend", func(h *containerHost) { h.Docker.Firewall = "nftables" }, "firewall", doctor.StatusFail},
		{"no DOCKER-USER chain", func(h *containerHost) { h.firewall = doctor.Firewall{ForwardPolicy: "ACCEPT"} }, "firewall", doctor.StatusFail},
		{"firewall without root", func(h *containerHost) { h.firewall = doctor.Firewall{Err: doctor.ErrNeedRoot} }, "firewall", doctor.StatusWarn},
		{"nftables forward chain drops", func(h *containerHost) { h.firewall.Drops = []string{"inet filter forward"} }, "firewall", doctor.StatusFail},
		// Forwarding came on before Docker started, as from a sysctl file.
		{"docker kept the FORWARD policy ACCEPT", func(h *containerHost) { h.firewall.ForwardPolicy = "ACCEPT" }, "firewall", doctor.StatusWarn},
		{"FORWARD policy ACCEPT without forwarding", func(h *containerHost) { h.firewall.ForwardPolicy, h.IPForward = "ACCEPT", false }, "firewall", doctor.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := healthyHost()
			tt.change(&h)
			checks := h.checks()
			i := slices.IndexFunc(checks, func(c doctor.Check) bool { return c.ID == tt.id })
			if i < 0 {
				t.Fatalf("no %s check", tt.id)
			}
			c := checks[i]
			if c.Status != tt.want {
				t.Errorf("%s is %s (%s), want %s", tt.id, c.Status, c.Detail, tt.want)
			}
			if c.Status != doctor.StatusOK && c.Fix == "" {
				t.Errorf("%s is %s without a fix", tt.id, c.Status)
			}
			// The UI shows the fix as text and the command with a copy button.
			if strings.Contains(c.Fix, "sudo ") {
				t.Errorf("%s fix %q holds a command; put it in Command", tt.id, c.Fix)
			}
		})
	}
}

// Without Docker, the docker check of the doctor tells the problem.
func TestNoFirewallCheckWithoutDocker(t *testing.T) {
	h := healthyHost()
	h.Docker = doctor.Docker{}
	if slices.ContainsFunc(h.checks(), func(c doctor.Check) bool { return c.ID == "firewall" }) {
		t.Error("a firewall check without Docker")
	}
}
