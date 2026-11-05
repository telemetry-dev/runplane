package firecracker

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/host"
)

// healthyHost is a good host for VM mode.
func healthyHost() vmHost {
	return vmHost{
		Host: &doctor.Host{
			Info: host.Info{
				OS: "Ubuntu 24.04.3 LTS", Kernel: "6.8.0-48-generic", Arch: "x64", KVM: true,
				FirecrackerVersion: "1.13.1",
			},
			Details: host.Details{OSID: "ubuntu", OSVersion: "24.04", CPUVirt: true, CgroupV2: true, IPForward: true},
			Root:    true, Systemd: true, Unit: "runplane", UnitState: "active", NFTables: true,
		},
		jailerVersion: "1.13.1",
		guestKernel:   "/var/lib/runplane/kernel/vmlinux",
		ids:           idRange{uid: 1879048192, gid: 1879048192, count: 16384},
		jailDir:       "/var/lib/runplane/jail",
		mount:         []string{"rw", "relatime"},
		firewall:      doctor.Firewall{ForwardPolicy: "ACCEPT"},
	}
}

// A healthy host passes all checks of VM mode, in this order.
func TestHealthyVMHost(t *testing.T) {
	var ids []string
	for _, c := range healthyHost().checks() {
		ids = append(ids, c.ID)
		if c.Status != doctor.StatusOK {
			t.Errorf("%s is %s: %s", c.ID, c.Status, c.Detail)
		}
	}
	want := []string{"os", "kernel", "cgroup", "kvm", "nftables", "ip_forward", "firecracker", "guest_kernel", "vm_ids", "tun", "jail_mount", "firewall", "systemd"}
	if !slices.Equal(ids, want) {
		t.Errorf("checks %v, want %v", ids, want)
	}
}

func TestVMChecks(t *testing.T) {
	tests := []struct {
		name   string
		change func(*vmHost)
		id     string
		want   doctor.Status
	}{
		{"debian", func(h *vmHost) { h.OSID, h.OSVersion = "debian", "12" }, "os", doctor.StatusOK},
		{"kernel 5.4", func(h *vmHost) { h.Kernel = "5.4.0-200-generic" }, "kernel", doctor.StatusFail},
		{"kernel 5.10", func(h *vmHost) { h.Kernel = "5.10.0-30-amd64" }, "kernel", doctor.StatusOK},
		{"no systemd", func(h *vmHost) { h.Systemd = false }, "systemd", doctor.StatusFail},

		{"no kvm device", func(h *vmHost) { h.KVM, h.CPUVirt, h.KVMErr = false, false, fs.ErrNotExist }, "kvm", doctor.StatusFail},
		{"kvm module not loaded", func(h *vmHost) { h.KVM, h.KVMErr = false, fs.ErrNotExist }, "kvm", doctor.StatusFail},
		{"kvm without root", func(h *vmHost) { h.KVM, h.KVMErr, h.Root = false, fs.ErrPermission, false }, "kvm", doctor.StatusWarn},
		{"kvm denied to root", func(h *vmHost) { h.KVM, h.KVMErr = false, fs.ErrPermission }, "kvm", doctor.StatusFail},
		{"nested kvm", func(h *vmHost) { h.Virtual = true }, "kvm", doctor.StatusOK},
		{"nested kvm on ovhcloud", func(h *vmHost) { h.Virtual, h.Provider = true, host.ProviderOVH }, "kvm", doctor.StatusWarn},

		{"firecracker missing", func(h *vmHost) { h.FirecrackerVersion = "" }, "firecracker", doctor.StatusFail},
		{"jailer missing", func(h *vmHost) { h.jailerVersion = "" }, "firecracker", doctor.StatusFail},
		{"jailer from another release", func(h *vmHost) { h.jailerVersion = "1.12.0" }, "firecracker", doctor.StatusFail},
		{"guest kernel missing", func(h *vmHost) { h.guestKernelErr = fs.ErrNotExist }, "guest_kernel", doctor.StatusFail},
		{"no ID range", func(h *vmHost) {
			h.idsErr = fmt.Errorf("/etc/subuid has no ID range for %s; run the install script again", jailerUser)
		}, "vm_ids", doctor.StatusFail},
		{"no tun device", func(h *vmHost) { h.tunErr = fs.ErrNotExist }, "tun", doctor.StatusFail},
		{"data folder noexec", func(h *vmHost) { h.mount = []string{"rw", "nosuid", "noexec"} }, "jail_mount", doctor.StatusFail},
		{"data folder nodev", func(h *vmHost) { h.mount = []string{"rw", "nodev"} }, "jail_mount", doctor.StatusFail},
		{"data folder mount unknown", func(h *vmHost) { h.mountErr = errors.New("no mount holds /var/lib/runplane/jail") }, "jail_mount", doctor.StatusWarn},

		{"ufw drops by default, runplane accepts its VMs", func(h *vmHost) { h.firewall.ForwardPolicy = "DROP" }, "firewall", doctor.StatusOK},
		{"nftables input chain drops", func(h *vmHost) { h.firewall.Drops = []string{"inet filter input"} }, "firewall", doctor.StatusFail},
		{"docker drops forwarded VM traffic", func(h *vmHost) { h.firewall = doctor.Firewall{ForwardPolicy: "DROP", DockerUser: true} }, "firewall", doctor.StatusOK},
		// Forwarding came on before Docker started, as from a sysctl file.
		{"docker kept the FORWARD policy ACCEPT", func(h *vmHost) {
			h.firewall, h.Docker.Version = doctor.Firewall{ForwardPolicy: "ACCEPT", DockerUser: true}, "29.5.1"
		}, "firewall", doctor.StatusWarn},
		{"no iptables", func(h *vmHost) { h.firewall = doctor.Firewall{Err: fmt.Errorf("list: %w", exec.ErrNotFound)} }, "firewall", doctor.StatusOK},
		{"firewall without root", func(h *vmHost) { h.firewall = doctor.Firewall{Err: doctor.ErrNeedRoot} }, "firewall", doctor.StatusWarn},
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

func TestMountOptions(t *testing.T) {
	const mountinfo = `22 1 252:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw
30 22 0:26 / /var rw,nosuid,nodev,noexec,relatime shared:2 - ext4 /dev/vdb rw
31 30 252:2 / /var/lib/runplane rw,relatime shared:3 - xfs /dev/vdc rw
32 30 0:27 / /var/lib/runplane rw,nodev,relatime shared:4 - tmpfs tmpfs rw
33 22 0:28 / /mnt/my\040data rw,noexec shared:5 - ext4 /dev/vdd rw
`
	tests := []struct {
		path string
		want string
	}{
		{"/var/lib/runplane/jail", "rw,nodev,relatime"}, // the later of two mounts at one point
		{"/var/lib", "rw,nosuid,nodev,noexec,relatime"},
		{"/var/libx", "rw,nosuid,nodev,noexec,relatime"},
		{"/varx", "rw,relatime"},
		{"/mnt/my data/jail", "rw,noexec"},
	}
	for _, tt := range tests {
		opts, err := mountOptions(strings.NewReader(mountinfo), tt.path)
		if err != nil || strings.Join(opts, ",") != tt.want {
			t.Errorf("%s: %v, %v; want %s", tt.path, opts, err, tt.want)
		}
	}
	if _, err := mountOptions(strings.NewReader(""), "/var/lib/runplane"); err == nil {
		t.Error("no mounts: want an error")
	}
}
