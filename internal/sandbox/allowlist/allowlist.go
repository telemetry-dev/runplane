// Package allowlist reads the entries of the egress allowlist. The settings
// accept only the entries that netrules can apply: sandboxes have IPv4 only,
// and the host resolves each name to its addresses, thus a wildcard cannot
// match.
package allowlist

import (
	"net/netip"
	"regexp"
	"strings"
)

// Entry is a host name, or else an IPv4 prefix. An address is a /32.
type Entry struct {
	Name   string
	Prefix netip.Prefix
}

// Parse reads one entry. It ignores the case, the spaces around the entry,
// and the final dot of a name.
func Parse(s string) (Entry, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	p, err := netip.ParsePrefix(s)
	if a, aerr := netip.ParseAddr(s); aerr == nil {
		p, err = netip.PrefixFrom(a, a.BitLen()), nil
	}
	if err == nil {
		return Entry{Prefix: p.Masked()}, p.Addr().Is4()
	}
	name := strings.TrimSuffix(s, ".")
	return Entry{Name: name}, hostName(name)
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// hostName accepts a lowercase name with at least two labels.
func hostName(name string) bool {
	labels := strings.Split(name, ".")
	if len(name) > 253 || len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !dnsLabel.MatchString(l) {
			return false
		}
	}
	return true
}
