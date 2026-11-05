//go:build linux

package firecracker

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/netrules"
)

// TestProbe runs the internet probe through the real VM firewall rules. It
// needs root, nftables, and the internet, and it changes the firewall of
// the network namespace, so it runs only with RUNPLANE_TEST_PROBE=1, for
// example in a privileged container.
func TestProbe(t *testing.T) {
	if os.Getenv("RUNPLANE_TEST_PROBE") != "1" {
		t.Skip("set RUNPLANE_TEST_PROBE=1 to fetch https://api.github.com through the VM firewall (needs root)")
	}
	ctx := context.Background()
	m := netrules.New(slog.New(slog.NewTextHandler(io.Discard, nil)), netrules.DefaultTapSubnet)
	t.Cleanup(func() { m.Teardown() })
	d := testDriver(t, t.TempDir(), systemHost{net: m})
	dns := []string{"1.1.1.1", "8.8.8.8"}
	if err := d.Configure(ctx, sandbox.Settings{DNS: dns}); err != nil {
		t.Fatal(err)
	}
	if err := d.Probe(ctx); err != nil {
		t.Fatalf("probe: %v", err)
	}
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if strings.HasPrefix(l.Attrs().Name, netrules.TapPrefix) {
			t.Errorf("the probe left %s", l.Attrs().Name)
		}
	}

	// The VM rules apply to the probe: with an allowlist that does not
	// have GitHub, it fails.
	if err := d.Configure(ctx, sandbox.Settings{DNS: dns, EgressAllowlist: []string{"192.0.2.1"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Probe(ctx); err == nil {
		t.Error("the probe reached GitHub outside the egress allowlist")
	}
}
