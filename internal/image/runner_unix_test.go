//go:build unix

package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	sum := sha256.Sum256(archive)
	files := map[string][]byte{
		"api.github.com/repos/actions/runner/releases/tags/v2.338.2":                                   releaseJSON("2.338.2", "x64", hex.EncodeToString(sum[:])),
		"github.com/actions/runner/releases/download/v2.338.2/actions-runner-linux-x64-2.338.2.tar.gz": archive,
	}
	b := NewBundles(BundleConfig{Mode: sandbox.ModeContainer, Dir: t.TempDir(), HTTP: fakeGitHub(t, files, nil).Client()})
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
