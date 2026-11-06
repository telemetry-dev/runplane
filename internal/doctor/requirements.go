package doctor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/sandbox/netrules"
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
// sandboxes.
func (h *Host) NFTablesCheck() Check {
	c := Check{ID: "nftables", Name: "nftables"}
	switch {
	case h.NFTables:
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

// ErrNeedRoot tells that only root can read a fact.
var ErrNeedRoot = errors.New("needs root")

// Firewall is what the doctor reads of the firewall of the host, for the
// rules of the sandboxes (netrules).
type Firewall struct {
	Drops         []string // the nftables chains that block the sandboxes (netrules.DropChains)
	ForwardPolicy string   // of the iptables FORWARD chain
	DockerUser    bool     // the iptables DOCKER-USER chain exists
	// Err tells why iptables cannot be read: ErrNeedRoot, or exec.ErrNotFound
	// on a host without iptables.
	Err error
}

// Without root, ReadFirewall reads nothing.
func ReadFirewall(ctx context.Context, root bool) Firewall {
	if !root {
		return Firewall{Err: ErrNeedRoot}
	}
	f := Firewall{Drops: netrules.DropChains()}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "iptables", "-w", "5", "-S").Output()
	if err != nil {
		f.Err = fmt.Errorf("run iptables: %w", err)
		return f
	}
	f.ForwardPolicy, f.DockerUser = parseIptables(string(out))
	return f
}

// Blocked fails c when an nftables chain blocks the sandboxes. ifaces names
// the sandbox interfaces of the driver, which the chain must accept.
func (f Firewall) Blocked(c Check, ifaces string) (Check, bool) {
	if len(f.Drops) == 0 {
		return c, false
	}
	return c.Fail(fmt.Sprintf("The nftables chain %s drops by default, so sandboxes cannot reach the internet or the cache", strings.Join(f.Drops, ", ")),
		"Accept the traffic of the sandbox interfaces in that chain: "+ifaces+".").WithCommand("sudo nft list ruleset"), true
}

// OpenForward warns with c when this server forwards the packets between
// all its networks. Docker sets the FORWARD policy to DROP only when it
// turns on IP forwarding itself, so an ACCEPT here means that forwarding was
// on before Docker started. Without Docker, the policy is the choice of the
// host firewall.
func (f Firewall) OpenForward(c Check, h *Host) (Check, bool) {
	if f.ForwardPolicy == "DROP" || !h.IPForward || h.Docker.Version == "" {
		return c, false
	}
	return c.Warn("The FORWARD policy is ACCEPT, so this server forwards packets between all its networks, not only for the sandboxes",
		"Set the policy to DROP. Docker and runplane accept the sandbox traffic with their own rules. "+
			"So that Docker sets DROP at each boot, remove net.ipv4.ip_forward from /etc/sysctl.conf and /etc/sysctl.d: runplane turns on forwarding after Docker starts.").
		WithCommand("sudo iptables -P FORWARD DROP"), true
}

// parseIptables reads "iptables -S" output.
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

// UnitState is the output of "systemctl is-active unit".
func UnitState(ctx context.Context, unit string) string {
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output()
	return strings.TrimSpace(string(out))
}
