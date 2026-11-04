//go:build linux

package netrules

import (
	"net/netip"
	"slices"
	"testing"
)

// A job must not reach the metadata or host agent service of the cloud it
// runs in: they give out credentials and the settings of the host.
func TestBlockedNetsCoverCloudHostServices(t *testing.T) {
	for _, addr := range []string{
		"169.254.169.254", // AWS, GCP, Azure IMDS, Oracle, DigitalOcean, Hetzner
		"168.63.129.16",   // Azure WireServer and HostGAPlugin
		"100.100.100.200", // Alibaba
		"10.0.0.1",
		"172.17.0.1",
		"192.168.1.1",
	} {
		a := netip.MustParseAddr(addr)
		if !slices.ContainsFunc(blockedNets, func(p netip.Prefix) bool { return p.Contains(a) }) {
			t.Errorf("%s is not in blockedNets", addr)
		}
	}
	for _, addr := range []string{"1.1.1.1", "140.82.112.3", "168.63.129.17"} {
		a := netip.MustParseAddr(addr)
		if slices.ContainsFunc(blockedNets, func(p netip.Prefix) bool { return p.Contains(a) }) {
			t.Errorf("the public address %s is in blockedNets", addr)
		}
	}
}
