package localnode

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/runner"
	"github.com/runplane/runplane/internal/sandbox"
)

// The primary sends min_free_disk_percent with the settings of a server.
// The doctor of the server must check the free disk against the last
// value, also before the first settings arrive.
func TestDoctorUsesTheLastSettings(t *testing.T) {
	ctx := t.Context()
	// No disk has 100% free.
	n := newNode(t, func(c *Config) { c.MinFreeDiskPercent = 100 })
	disk := func() doctor.Check {
		t.Helper()
		r, err := n.Doctor(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range r.Checks {
			if c.ID == "disk" {
				return c
			}
		}
		t.Fatalf("the doctor has no disk check: %+v", r.Checks)
		return doctor.Check{}
	}

	if c := disk(); c.Status != doctor.StatusFail {
		t.Errorf("disk check with the first min_free_disk_percent 100: %+v, want fail", c)
	}
	if err := n.Configure(ctx, agent.Settings{MinFreeDiskPercent: 0}); err != nil {
		t.Fatal(err)
	}
	if c := disk(); c.Status == doctor.StatusFail {
		t.Errorf("disk check after the settings with min_free_disk_percent 0: %+v, want no failure", c)
	}
}

// An agent keeps the settings of the primary. After a restart, its driver
// applies the egress allowlist to the sandboxes before the primary connects.
func TestSettingsLastUntilTheNextStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-settings.json")
	if s, err := ReadSettings(path); err != nil || !reflect.DeepEqual(s, agent.Settings{}) {
		t.Errorf("settings before the first Configure: %+v, %v; want none", s, err)
	}
	n := newNode(t, func(c *Config) { c.SettingsFile = path })
	want := agent.Settings{
		Settings: sandbox.Settings{
			DNS: []netip.Addr{netip.MustParseAddr("9.9.9.9")}, EgressAllowlist: []string{"github.com", "10.0.0.0/8"},
			BandwidthMbps: 100, RegistryMirror: true,
		},
		MinFreeDiskPercent: 12.5,
	}
	if err := n.Configure(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadSettings(path); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("settings after Configure:\n%+v, %v\nwant\n%+v", got, err, want)
	}
}

// newNode makes a VM mode node in a temporary folder, with a fake driver.
func newNode(t *testing.T, opts func(*Config)) *Node {
	t.Helper()
	dir := t.TempDir()
	images, err := image.New(image.Config{
		Mode: sandbox.ModeVM, ImagesDir: filepath.Join(dir, "images"), Init: func() ([]byte, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Driver: fakeDriver{}, Images: images, Updater: fakeUpdater{},
		Bundles: runner.NewBundles(runner.BundleConfig{Mode: sandbox.ModeVM, Dir: filepath.Join(dir, "runners")}),
		Mirror:  mirror.Config{Dir: filepath.Join(dir, "mirror")},
		Doctor: doctor.Options{
			DataDir:   dir,
			Docker:    func(context.Context) (doctor.Docker, error) { return doctor.Docker{}, errors.New("no docker") },
			BaseImage: func(context.Context) (string, error) { return "ghcr.io/runplane/runplane/runner-ubuntu-24.04", nil },
		},
	}
	opts(&cfg)
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

// fakeDriver takes settings and has no sandboxes.
type fakeDriver struct{ sandbox.Driver }

func (fakeDriver) Configure(context.Context, sandbox.Settings) error { return nil }
func (fakeDriver) Probe(context.Context) error                       { return nil }

type fakeUpdater struct{}

func (fakeUpdater) Apply(context.Context, string) error { return nil }
