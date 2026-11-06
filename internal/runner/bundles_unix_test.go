//go:build unix

package runner

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/runplane/runplane/internal/sandbox"
)

// Sandboxes read the bundle as other users, so a strict umask of the service
// must not change its modes.
func TestEnsureBundleModesWithStrictUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })

	archive := runnerArchive(t, []byte("MZ"))
	releases, _ := serveRunner(t, "2.338.2", "x64", archive, sha256.Sum256(archive))
	b := NewBundles(BundleConfig{Mode: sandbox.ModeContainer, Dir: t.TempDir(), Releases: releases})
	bundle, err := b.Ensure(context.Background(), "2.338.2", "x64")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]os.FileMode{
		"":                              0o755,
		"bin":                           0o755,
		"externals/node/lib":            0o755,
		"run.sh":                        0o755,
		"bin/Runner.Worker.dll":         0o644,
		"externals/node/lib/npm-cli.js": 0o644,
	}
	for name, mode := range want {
		st, err := os.Stat(filepath.Join(bundle.Dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if st.Mode().Perm() != mode {
			t.Errorf("%s: mode %v, want %v", name, st.Mode().Perm(), mode)
		}
	}
}
