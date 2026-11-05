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
	for addr, want := range map[string]bool{
		"169.254.169.254": true, // AWS, GCP, Azure IMDS, Oracle, DigitalOcean, Hetzner
		"168.63.129.16":   true, // Azure WireServer and HostGAPlugin
		"100.100.100.200": true, // Alibaba
		"10.0.0.1":        true,
		"172.17.0.1":      true,
		"192.168.1.1":     true,
		"1.1.1.1":         false,
		"140.82.112.3":    false,
		"168.63.129.17":   false,
	} {
		a := netip.MustParseAddr(addr)
		if got := slices.ContainsFunc(blockedNets, func(p netip.Prefix) bool { return p.Contains(a) }); got != want {
			t.Errorf("%s blocked: %v, want %v", addr, got, want)
		}
	}
}
