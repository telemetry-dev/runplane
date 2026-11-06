//go:build linux

package netrules

import (
	"os"
	"slices"
	"testing"
)

func TestParseListing(t *testing.T) {
	tests := []struct {
		file       string
		forward    string
		dockerUser bool
	}{
		{"docker", "DROP", true},
		{"ufw", "DROP", false},
		{"plain", "ACCEPT", false},
	}
	for _, tt := range tests {
		b, err := os.ReadFile("testdata/iptables/" + tt.file)
		if err != nil {
			t.Fatal(err)
		}
		chains, forward := parseListing(string(b))
		if forward != tt.forward || slices.Contains(chains, dockerUser) != tt.dockerUser {
			t.Errorf("%s: FORWARD policy %q, DOCKER-USER %v; want %q, %v", tt.file, forward, slices.Contains(chains, dockerUser), tt.forward, tt.dockerUser)
		}
	}
}

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
}
