package api

import (
	"net/netip"
	"slices"
	"testing"
)

func TestDNSState(t *testing.T) {
	server := addrs("203.0.113.7", "2001:db8::7")
	tests := []struct {
		name    string
		records []netip.Addr
		server  []netip.Addr
		want    string
	}{
		{"no record", nil, server, dnsMissing},
		{"A record at this server", addrs("203.0.113.7"), server, dnsOK},
		{"A and AAAA at this server", addrs("203.0.113.7", "2001:db8::7"), server, dnsOK},
		{"IPv4 in IPv6 form", addrs("::ffff:203.0.113.7"), server, dnsOK},
		// Let's Encrypt can connect to the AAAA address, thus one wrong record is enough to fail.
		{"AAAA at a proxy", addrs("203.0.113.7", "2606:4700::1"), server, dnsOther},
		{"A at a proxy", addrs("104.21.36.133"), server, dnsOther},
		{"no known server address", addrs("203.0.113.7"), nil, dnsUnknown},
	}
	for _, tt := range tests {
		if got := dnsState(tt.records, tt.server); got != tt.want {
			t.Errorf("%s: dnsState = %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestPublicAddrs(t *testing.T) {
	in := addrs(
		"2001:db8::7", "203.0.113.7", "::ffff:203.0.113.7",
		"127.0.0.1", "10.214.0.1", "172.17.0.1", "192.168.1.5", "100.101.102.103",
		"fe80::1", "fd7a:115c:a1e0::1", "::1",
	)
	got := addrStrings(publicAddrs(in))
	if want := []string{"203.0.113.7", "2001:db8::7"}; !slices.Equal(got, want) {
		t.Errorf("publicAddrs = %v, want %v", got, want)
	}
}

func addrs(list ...string) []netip.Addr {
	out := make([]netip.Addr, len(list))
	for i, s := range list {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}
