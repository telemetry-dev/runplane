package main

import (
	"context"
	"net/netip"
	"os"
	"slices"
	"testing"

	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/sandbox"
)

// The doctor must fail where serve and the agent refuse to start.
func TestDoctorModeCheck(t *testing.T) {
	cases := []struct {
		mode        sandbox.Mode
		devInsecure bool
		want        doctor.Status
	}{
		{"", false, doctor.StatusFail},
		{sandbox.ModeDev, false, doctor.StatusFail},
		{sandbox.ModeDev, true, doctor.StatusWarn},
		{sandbox.ModeContainer, false, doctor.StatusOK},
	}
	for _, c := range cases {
		cfg := config.Default()
		cfg.ConfigDir, cfg.DataDir, cfg.DevInsecure = t.TempDir(), t.TempDir(), c.devInsecure
		if got := requirements(cfg, c.mode, false).Mode; got.Status != c.want {
			t.Errorf("mode %q, RUNPLANE_DEV_INSECURE %v: the mode check is %s (%s), want %s", c.mode, c.devInsecure, got.Status, got.Detail, c.want)
		}
	}
}

// The firewall of an agent lets the sandboxes send DNS requests only to the
// DNS servers of the settings that the primary sent, so the doctor of an
// agent must probe with these servers.
func TestAgentDoctorUsesTheSettingsOfThePrimary(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.ConfigDir, cfg.DataDir = t.TempDir(), t.TempDir()

	_, node, err := serviceSettings(ctx, cfg, true)
	defaults := []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")}
	if err != nil || !slices.Equal(node.DNS, defaults) || node.MinFreeDiskPercent != 15 {
		t.Errorf("before the first connection: DNS %v, min free %v%%, %v; want the defaults", node.DNS, node.MinFreeDiskPercent, err)
	}

	sent := `{"dns":["9.9.9.9"],"egress_allowlist":["github.com"],"min_free_disk_percent":5}`
	if err := os.WriteFile(cfg.AgentSettingsPath(), []byte(sent), 0o600); err != nil {
		t.Fatal(err)
	}
	_, node, err = serviceSettings(ctx, cfg, true)
	if err != nil || !slices.Equal(node.DNS, []netip.Addr{netip.MustParseAddr("9.9.9.9")}) || node.MinFreeDiskPercent != 5 {
		t.Errorf("DNS %v, min free %v%%, %v; want 9.9.9.9 and 5%% from the primary", node.DNS, node.MinFreeDiskPercent, err)
	}
}
