//go:build linux

package host

import (
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/vishvananda/netns"
)

// The doctor reports a drop policy only where runplane cannot add accepts.
// Run as root in a privileged container; the test uses a new network
// namespace.
func TestNftDropChains(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("needs nft")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer orig.Close()
	ns, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { netns.Set(orig); ns.Close() }()
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
		if out, err := exec.Command("nft", strings.Fields(cmd)...).CombinedOutput(); err != nil {
			t.Fatalf("nft %s: %v\n%s", cmd, err, out)
		}
	}
	if got := nftDropChains(); !slices.Equal(got, []string{"inet mine input"}) {
		t.Errorf("nftDropChains = %v, want [inet mine input]", got)
	}
}
