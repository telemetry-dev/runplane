package doctor

import (
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Requirements are what the sandbox driver of a mode needs on the server.
// Each driver package gives its Requirements, with the paths, the users,
// and the versions that the driver uses. The methods of Host below are the
// checks that more than one driver needs.
type Requirements struct {
	// Mode is the first check, which names the mode. When it fails, the
	// doctor runs no other check.
	Mode Check
	// Docker tells that the sandboxes need Docker.
	Docker bool
	// Listen are the TCP addresses of the driver, for the port checks.
	Listen []string
	// Software reads the versions of the sandbox software into h.Info.
	Software func(ctx context.Context, h *Host)
	Checks   func(ctx context.Context, h *Host) []Check
}

// ModeCheck is the empty check of Requirements.Mode.
var ModeCheck = Check{ID: "mode", Name: "Mode"}

func (h *Host) CgroupCheck() Check {
	c := Check{ID: "cgroup", Name: "cgroup v2"}
	if h.CgroupV2 {
		return c.OK("Unified hierarchy at /sys/fs/cgroup")
	}
	return c.Fail("The host uses cgroup v1", "Add systemd.unified_cgroup_hierarchy=1 to the kernel command line and reboot.")
}

// NFTablesCheck checks the kernel support of the firewall rules of the
// sandboxes: available is netrules.Available.
func (h *Host) NFTablesCheck(available bool) Check {
	c := Check{ID: "nftables", Name: "nftables"}
	switch {
	case available:
		return c.OK("Available")
	case !h.Root:
		return c.AsRoot("Cannot check without root")
	}
	return c.Fail("The kernel has no nf_tables support", "Load the nf_tables module.").WithCommand("sudo modprobe nf_tables")
}

// runplane turns IP forwarding on when it starts, after Docker, and not with
// a sysctl file at boot: Docker sets the FORWARD policy to DROP only when it
// turns on forwarding itself.
func (h *Host) ForwardCheck() Check {
	c := Check{ID: "ip_forward", Name: "IP forwarding"}
	if h.IPForward {
		return c.OK("net.ipv4.ip_forward = 1")
	}
	c = c.Fail("net.ipv4.ip_forward = 0, so sandboxes have no internet access", "runplane turns on IP forwarding when it starts. Restart runplane.")
	if h.Unit != "" {
		c = c.WithCommand("sudo systemctl restart " + h.Unit)
	}
	return c
}

// SystemdCheck looks at systemd and the unit of runplane. need tells why the
// driver needs systemd; without a need, a host without systemd only warns.
func (h *Host) SystemdCheck(need string) Check {
	c := Check{ID: "systemd", Name: "systemd"}
	switch {
	case !h.Systemd && need != "":
		return c.Fail("The host does not run systemd, "+need, "Use a Linux distribution with systemd.")
	case !h.Systemd:
		return c.Warn("The host does not run systemd", "Use a Linux distribution with systemd.")
	case h.Unit == "":
		return c.OK("Running")
	case h.UnitState == "active":
		return c.OK(h.Unit + ".service is active")
	}
	return c.Warn(fmt.Sprintf("%s.service is %s", h.Unit, cmp.Or(h.UnitState, "unknown")), "Start the "+h.Unit+" service.").
		WithCommand("sudo systemctl enable --now " + h.Unit)
}

func (h *Host) KernelCheck(rules func(c Check, v Version) Check) Check {
	c := Check{ID: "kernel", Name: "Kernel"}
	v, ok := ParseVersion(h.Kernel)
	if !ok {
		return c.Warn(fmt.Sprintf("Cannot read the kernel version %q", h.Kernel), "Check that uname -r prints a version such as 6.8.0.").WithCommand("uname -r")
	}
	return rules(c, v)
}

// Blocked fails c when one of drops, the nftables chains that drop by
// default, blocks the sandboxes. ifaces names the sandbox interfaces of the
// driver, which the chains must accept.
func Blocked(c Check, drops []string, ifaces string) (Check, bool) {
	if len(drops) == 0 {
		return c, false
	}
	return c.Fail(fmt.Sprintf("The nftables chain %s drops by default, so sandboxes cannot reach the internet or the cache", strings.Join(drops, ", ")),
		"Accept the traffic of the sandbox interfaces in that chain: "+ifaces+".").WithCommand("sudo nft list ruleset"), true
}

// OpenForward warns with c when this server forwards the packets between
// all its networks: forwardPolicy is the policy of the iptables FORWARD
// chain. Docker sets the FORWARD policy to DROP only when it turns on IP
// forwarding itself, so an ACCEPT here means that forwarding was on before
// Docker started. Without Docker, the policy is the choice of the host
// firewall.
func (h *Host) OpenForward(c Check, forwardPolicy string) (Check, bool) {
	if forwardPolicy == "DROP" || !h.IPForward || h.Docker.Version == "" {
		return c, false
	}
	return c.Warn("The FORWARD policy is ACCEPT, so this server forwards packets between all its networks, not only for the sandboxes",
		"Set the policy to DROP. Docker and runplane accept the sandbox traffic with their own rules. "+
			"So that Docker sets DROP at each boot, remove net.ipv4.ip_forward from /etc/sysctl.conf and /etc/sysctl.d: runplane turns on forwarding after Docker starts.").
		WithCommand("sudo iptables -P FORWARD DROP"), true
}

// UnitState is the output of "systemctl is-active unit".
func UnitState(ctx context.Context, unit string) string {
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output()
	return strings.TrimSpace(string(out))
}
