package localnode

import (
	"context"
	"errors"
	"path/filepath"
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
	dir := t.TempDir()
	images, err := image.New(image.Config{
		Mode: sandbox.ModeVM, ImagesDir: filepath.Join(dir, "images"), Init: func() ([]byte, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := New(Config{
		Driver: fakeDriver{}, Images: images, Updater: fakeUpdater{},
		Bundles: runner.NewBundles(runner.BundleConfig{Mode: sandbox.ModeVM, Dir: filepath.Join(dir, "runners")}),
		// No disk has 100% free.
		MinFreeDiskPercent: 100,
		Mirror:             mirror.Config{Dir: filepath.Join(dir, "mirror")},
		Doctor: doctor.Options{
			DataDir:   dir,
			Docker:    func(context.Context) (doctor.Docker, error) { return doctor.Docker{}, errors.New("no docker") },
			BaseImage: func(context.Context) (string, error) { return "ghcr.io/runplane/runplane/runner-ubuntu-24.04", nil },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
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

// fakeDriver takes settings and has no sandboxes.
type fakeDriver struct{ sandbox.Driver }

func (fakeDriver) Configure(context.Context, sandbox.Settings) error { return nil }
func (fakeDriver) Probe(context.Context) error                       { return nil }

type fakeUpdater struct{}

func (fakeUpdater) Apply(context.Context, string) error { return nil }
