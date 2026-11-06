package sysbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/runplane/runplane/internal/doctor"
)

const (
	// minSysbox is the first Sysbox that starts containers with Docker
	// dockerTimeNS and newer, which put containers in time namespaces.
	minSysbox    = "0.7.1"
	dockerTimeNS = "29.5"
	sysboxUnit   = "sysbox"
)

func Requirements() doctor.Requirements {
	return doctor.Requirements{
		Mode:   doctor.ModeCheck.OK("Container mode (Sysbox): use it for trusted code only"),
		Docker: true,
		Software: func(ctx context.Context, h *doctor.Host) {
			h.SysboxVersion = doctor.ToolVersion(ctx, cmp.Or(h.Docker.Runtimes[Container.runtime], Container.runtime))
		},
		Checks: func(ctx context.Context, h *doctor.Host) []doctor.Check {
			return containerHost{Host: h, sysboxUnit: doctor.UnitState(ctx, sysboxUnit), firewall: doctor.ReadFirewall(ctx, h.Root)}.checks()
		},
	}
}

type containerHost struct {
	*doctor.Host
	sysboxUnit string // systemctl is-active of the sysbox service
	firewall   doctor.Firewall
}

func (h containerHost) checks() []doctor.Check {
	cs := []doctor.Check{
		h.osCheck(), h.KernelCheck(h.kernelRules), h.CgroupCheck(), h.kvmCheck(), h.NFTablesCheck(), h.ForwardCheck(),
		h.sysboxCheck(),
	}
	// Without Docker, the docker check tells the problem.
	if h.Docker.Version != "" {
		cs = append(cs, h.firewallCheck())
	}
	return append(cs, h.SystemdCheck(""))
}

func (h containerHost) osCheck() doctor.Check {
	c := doctor.Check{ID: "os", Name: "Operating system"}
	name := cmp.Or(h.OS, "Unknown")
	if h.OSID == "ubuntu" && (h.OSVersion == "22.04" || h.OSVersion == "24.04") {
		return c.OK(name)
	}
	return c.Warn(name+": Sysbox supports only Ubuntu 22.04 and 24.04", "Use Ubuntu 24.04 for container mode.")
}

func (h containerHost) kernelRules(c doctor.Check, v doctor.Version) doctor.Check {
	switch {
	case v.Less(doctor.Version{5, 5}):
		return c.Fail(h.Kernel+": Sysbox needs kernel 5.5 or newer", "Upgrade the kernel to 5.15 or newer.")
	case v.Less(doctor.Version{5, 12}):
		return c.Warn(h.Kernel+": Sysbox needs shiftfs on kernels before 5.12", "Upgrade the kernel to 5.15 or newer.")
	}
	return c.OK(h.Kernel)
}

// kvmCheck tells if VM mode, which isolates jobs better, can run here.
func (h containerHost) kvmCheck() doctor.Check {
	c := doctor.Check{ID: "kvm", Name: "KVM"}
	if h.KVM || errors.Is(h.KVMErr, fs.ErrPermission) {
		return c.OK("Available: VM mode gives stronger isolation")
	}
	return c.OK("Not available; container mode does not need it")
}

func (h containerHost) sysboxCheck() doctor.Check {
	c := doctor.Check{ID: "sysbox", Name: "Sysbox"}
	install := "Install Sysbox " + minSysbox + " or newer from github.com/nestybox/sysbox/releases."
	_, listed := h.Docker.Runtimes[Container.runtime]
	switch {
	case h.SysboxVersion == "":
		return c.Fail("Sysbox is not installed", install)
	case !doctor.AtLeast(h.SysboxVersion, minSysbox) && doctor.AtLeast(h.Docker.Version, dockerTimeNS):
		return c.Fail(fmt.Sprintf("Sysbox %s cannot start containers with Docker %s, which uses time namespaces", h.SysboxVersion, h.Docker.Version), install)
	case !doctor.AtLeast(h.SysboxVersion, minSysbox):
		return c.Warn(fmt.Sprintf("Sysbox %s: Docker %s and newer need Sysbox %s", h.SysboxVersion, dockerTimeNS, minSysbox), install)
	case h.sysboxUnit != "" && h.sysboxUnit != "active":
		return c.Fail("The sysbox service is "+h.sysboxUnit, "Start the sysbox service.").WithCommand("sudo systemctl enable --now " + sysboxUnit)
	case h.Docker.Version != "" && !listed:
		return c.Fail("Docker does not list the "+Container.runtime+" runtime", "Restart Docker.").WithCommand("sudo systemctl restart docker")
	}
	return c.OK("Sysbox " + h.SysboxVersion)
}

// The traffic of the containers goes through the DOCKER-USER chain, where
// runplane puts its sandbox rules.
func (h containerHost) firewallCheck() doctor.Check {
	c := doctor.Check{ID: "firewall", Name: "Firewall rules"}
	fw := h.firewall
	if blocked, ok := fw.Blocked(c, bridgeName); ok {
		return blocked
	}
	switch {
	case h.Docker.Firewall == "nftables":
		return c.Fail("Docker uses its nftables firewall backend, which has no DOCKER-USER chain", `Remove "firewall-backend" from /etc/docker/daemon.json and restart Docker.`)
	case errors.Is(fw.Err, doctor.ErrNeedRoot):
		return c.AsRoot("Cannot read the firewall without root")
	case fw.Err != nil:
		return c.Fail(fmt.Sprintf("Cannot read the iptables rules: %v", fw.Err), "Install iptables.").WithCommand("sudo apt-get install -y iptables")
	case !fw.DockerUser:
		return c.Fail("The DOCKER-USER chain is missing, so Docker does not manage iptables", `Remove "iptables": false from /etc/docker/daemon.json and restart Docker.`)
	}
	if open, ok := fw.OpenForward(c, h.Host); ok {
		return open
	}
	return c.OK("DOCKER-USER chain present")
}
