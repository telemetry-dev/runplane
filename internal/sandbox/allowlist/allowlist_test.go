package allowlist

import (
	"net/netip"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Entry
		ok   bool
	}{
		{" GitHub.com. ", Entry{Name: "github.com"}, true},
		{"203.0.113.7", Entry{Prefix: netip.MustParsePrefix("203.0.113.7/32")}, true},
		{"140.82.112.9/20", Entry{Prefix: netip.MustParsePrefix("140.82.112.0/20")}, true},
		// The settings refuse what the firewall cannot apply.
		{"*.example.com", Entry{}, false},
		{"::1", Entry{}, false},
		{"2001:db8::/32", Entry{}, false},
		{"localhost", Entry{}, false},
		{"not a host", Entry{}, false},
		{"", Entry{}, false},
	} {
		got, ok := Parse(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("Parse(%q) = %+v, %v; want %+v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
