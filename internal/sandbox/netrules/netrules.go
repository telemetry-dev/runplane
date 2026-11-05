// Package netrules owns the host network parts of sandboxes: the nftables
// tables "inet runplane" and "bridge runplane", the runplane rules in the
// iptables filter table (INPUT, FORWARD, and Docker's DOCKER-USER), VM TAP
// devices, and the allocator for TAP addresses.
package netrules

import (
	"fmt"
	"net/netip"
	"time"
)

// TapPrefix starts the name of each VM TAP device. The firewall treats each
// interface with this prefix as a sandbox, so a TAP gets no traffic through
// until AddTap gives it a source address.
const TapPrefix = "rp-"

// DefaultTapSubnet is the VM mode address range. Each VM gets one /30 of it.
var DefaultTapSubnet = netip.MustParsePrefix("10.214.0.0/16")

// Config is the firewall policy for all sandboxes on the host.
type Config struct {
	// TapSubnet is the VM mode range (see Allocator). Leave it zero in
	// container mode.
	TapSubnet netip.Prefix
	// ServicePort is the TCP port of the cache server and the registry
	// mirror on the gateway address of each sandbox.
	ServicePort int
	// DNS servers stay reachable on port 53, also inside blocked ranges and
	// outside the allowlist. The allowlist names resolve through them too.
	DNS []netip.Addr
	// Allowlist limits egress to these host names, IPv4 addresses, and IPv4
	// prefixes. Empty allows all public addresses. Blocked ranges stay
	// blocked.
	Allowlist []string
	// Refresh is how often the Manager resolves the allowlist names again
	// and repairs the iptables rules. Zero means one minute.
	Refresh time.Duration
}

func (c *Config) check() error {
	if c.TapSubnet.IsValid() {
		if err := checkTapSubnet(c.TapSubnet); err != nil {
			return err
		}
	}
	if c.ServicePort < 1 || c.ServicePort > 65535 {
		return fmt.Errorf("netrules: service port %d out of range", c.ServicePort)
	}
	for _, a := range c.DNS {
		if !a.Is4() {
			return fmt.Errorf("netrules: DNS server %s is not IPv4", a)
		}
	}
	if c.Refresh <= 0 {
		c.Refresh = time.Minute
	}
	return nil
}

func checkTapSubnet(p netip.Prefix) error {
	if !p.Addr().Is4() || p.Bits() > 30 || p != p.Masked() {
		return fmt.Errorf("netrules: TAP subnet %s must be an IPv4 network of /30 or larger", p)
	}
	return nil
}
