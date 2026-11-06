package sysbox

import (
	"net/netip"
	"testing"
)

func TestServiceHost(t *testing.T) {
	cases := []struct {
		gateway string
		want    string
	}{
		{"127.0.0.1", "127.0.0.1"},            // Docker on this machine
		{"192.0.2.1", "host.docker.internal"}, // a documentation address: Docker in a VM
	}
	for _, c := range cases {
		got, err := serviceHost(netip.MustParseAddr(c.gateway))
		if err != nil || got != c.want {
			t.Errorf("serviceHost(%s) = %q, %v; want %q", c.gateway, got, err, c.want)
		}
	}
}
