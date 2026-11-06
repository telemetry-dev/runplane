//go:build linux

package netrules

import (
	"slices"
	"testing"
)

// The doctor reports a drop policy only where runplane cannot add accepts.
func TestDropChains(t *testing.T) {
	isolate(t)
	for _, cmd := range []string{
		"add table inet mine",
		"add chain inet mine input { type filter hook input priority 0; policy drop; }",
		"add chain inet mine output { type filter hook output priority 0; policy drop; }",
		"add table ip filter",
		"add chain ip filter INPUT { type filter hook input priority 0; policy drop; }",
		"add table inet runplane",
		"add chain inet runplane forward { type filter hook forward priority 0; policy drop; }",
		"add table inet open",
		"add chain inet open forward { type filter hook forward priority 0; policy accept; }",
	} {
		run(t, "nft", cmd)
	}
	if got := ReadFirewall(t.Context()).Drops; !slices.Equal(got, []string{"inet mine input"}) {
		t.Errorf("drop chains = %v, want [inet mine input]", got)
	}
}
