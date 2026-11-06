//go:build linux

package netrules

import (
	"testing"
)

// The doctor reads the filter tables of the two iptables backends, as the
// rules of runplane go to both. A drop policy in one of them drops.
func TestReadFirewall(t *testing.T) {
	isolate(t)
	run(t, "iptables-nft", "-N DOCKER-USER")
	run(t, "iptables-legacy", "-P FORWARD DROP")
	got := ReadFirewall(t.Context())
	if got.Err != nil || got.ForwardPolicy != "DROP" || !got.DockerUser {
		t.Errorf("ReadFirewall = %+v, want the FORWARD policy DROP and DOCKER-USER", got)
	}

	run(t, "iptables-legacy", "-P FORWARD ACCEPT")
	run(t, "iptables-nft", "-P FORWARD DROP")
	if got := ReadFirewall(t.Context()); got.ForwardPolicy != "DROP" {
		t.Errorf("FORWARD policy %q with a drop in the nf_tables backend, want DROP", got.ForwardPolicy)
	}

	run(t, "iptables-nft", "-P FORWARD ACCEPT")
	run(t, "iptables-nft", "-X DOCKER-USER")
	run(t, "iptables-legacy", "-N DOCKER-USER")
	if got := ReadFirewall(t.Context()); got.ForwardPolicy != "ACCEPT" || !got.DockerUser {
		t.Errorf("ReadFirewall = %+v, want the FORWARD policy ACCEPT and DOCKER-USER of the legacy backend", got)
	}
}
