package image

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/runplane/runplane/internal/ext4/ext4test"
	"github.com/runplane/runplane/internal/sandbox"
)

func TestEnsureRootDisk(t *testing.T) {
	ctx := context.Background()
	img := testImage(t, runtime.GOARCH)
	ref := pushImage(t, img)
	wantDigest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	init := []byte("#!runplane-init")
	m, err := New(Config{Mode: sandbox.ModeVM, ImagesDir: dir, Init: func() ([]byte, error) { return init, nil }})
	if err != nil {
		t.Fatal(err)
	}
	// A runplane update with a new runplane-init leaves the root disks of
	// the old one, also for an image whose digest stays.
	leftover := filepath.Join(dir, strings.TrimPrefix(wantDigest.String(), "sha256:")+"-0badc0de.ext4")
	if err := os.WriteFile(leftover, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	var steps []Progress
	got, err := m.Ensure(ctx, ref, func(p Progress) { steps = append(steps, p) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("root disk of an earlier runplane-init after Ensure: %v, want it removed", err)
	}
	initSum := sha256.Sum256(init)
	wantPath := filepath.Join(dir, strings.TrimPrefix(wantDigest.String(), "sha256:")+"-"+hex.EncodeToString(initSum[:4])+".ext4")
	if got.Ref != ref || got.Digest != wantDigest.String() || got.Path != wantPath {
		t.Errorf("image = %+v, want ref %s, digest %s, path %s", got, ref, wantDigest, wantPath)
	}
	if st, err := os.Stat(wantPath); err != nil || st.Size() != got.Size {
		t.Errorf("root disk: size %d, %v; image size %d", st.Size(), err, got.Size)
	}
	if s := statuses(steps); s != "pending pulling converting ready" {
		t.Errorf("statuses = %s", s)
	}
	if p := lastOf(steps, StatusPulling); p.Total == 0 || p.Done != p.Total {
		t.Errorf("last pulling step = %+v, want all bytes done", p)
	}
	assertNoTemp(t, dir)

	ext4test.Check(t, got.Path)
	ext4test.Debugfs(t, got.Path, "ls -l /sbin/", "runplane-init")
	ext4test.Debugfs(t, got.Path, "cat /sbin/runplane-init", string(init))
	ext4test.Debugfs(t, got.Path, "stat /usr/sbin/runplane-init", "Mode:  0755")
	ext4test.Debugfs(t, got.Path, "stat /sbin", `Fast link dest: "usr/sbin"`)
	ext4test.Debugfs(t, got.Path, "cat /etc/kept", "new")
	ext4test.Debugfs(t, got.Path, "stat /etc/gone", "File not found")
	ext4test.Debugfs(t, got.Path, "stat /tmp", "Mode:  01777")
	ext4test.Debugfs(t, got.Path, "stat /mnt/scratch", "Type: directory")
	ext4test.Debugfs(t, got.Path, "stat /proc", "Mode:  0711")
	ext4test.Debugfs(t, got.Path, "cat /etc/runplane/environ", "PATH=/usr/sbin:/usr/bin\x00GOFLAGS=-tags=vm\x00")

	steps = nil
	again, err := m.Ensure(ctx, ref, func(p Progress) { steps = append(steps, p) })
	if err != nil {
		t.Fatal(err)
	}
	if again != got {
		t.Errorf("second image = %+v, want %+v", again, got)
	}
	if s := statuses(steps); s != "pending pulling ready" {
		t.Errorf("second statuses = %s, want the root disk used again", s)
	}
	if p, err := m.RootDisk(got.Digest); err != nil || p != wantPath {
		t.Errorf("RootDisk = %q, %v", p, err)
	}
	if _, err := m.RootDisk("sha256:../../etc/passwd"); err == nil {
		t.Error("RootDisk accepted a bad digest")
	}

	oldInit := filepath.Join(dir, strings.TrimPrefix(wantDigest.String(), "sha256:")+"-00000000.ext4")
	unused := filepath.Join(dir, strings.Repeat("a", 64)+"-"+hex.EncodeToString(initSum[:4])+".ext4")
	for _, p := range []string{oldInit, unused} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, ".pull-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	// keep is empty, but Ensure just returned the image.
	if err := m.GC(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if left := names(t, dir); left != filepath.Base(wantPath) {
		t.Errorf("after the first GC: %s, want only %s", left, filepath.Base(wantPath))
	}
	if err := m.GC(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if left := names(t, dir); left != "" {
		t.Errorf("after the second GC: %s, want nothing", left)
	}
}

func TestEnsureRootDiskRejectsOtherArch(t *testing.T) {
	ref := pushImage(t, testImage(t, "s390x"))
	m, err := New(Config{Mode: sandbox.ModeVM, ImagesDir: t.TempDir(), Init: func() ([]byte, error) { return []byte("init"), nil }})
	if err != nil {
		t.Fatal(err)
	}
	var last Progress
	_, err = m.Ensure(context.Background(), ref, func(p Progress) { last = p })
	if err == nil || !strings.Contains(err.Error(), "linux/s390x") {
		t.Fatalf("Ensure error = %v, want an arch error", err)
	}
	if last.Status != StatusFailed || last.Error != err.Error() {
		t.Errorf("last step = %+v, want failed with the error", last)
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		links map[string]string
		want  string
	}{
		{"no links", "mnt/scratch", nil, "mnt/scratch"},
		{"relative link", "sbin/runplane-init", map[string]string{"sbin": "usr/sbin"}, "usr/sbin/runplane-init"},
		{"absolute link", "sbin/runplane-init", map[string]string{"sbin": "/usr/sbin"}, "usr/sbin/runplane-init"},
		{"link to a link", "a/x", map[string]string{"a": "b", "b": "/c/d"}, "c/d/x"},
		{"link with ..", "usr/sbin/x", map[string]string{"usr/sbin": "../bin"}, "bin/x"},
		{"link in the target", "a/x", map[string]string{"a": "b/c", "b": "d"}, "d/c/x"},
		{"cannot leave the root", "tmp", map[string]string{"tmp": "../../.."}, ""},
		{"loop stops", "a/x", map[string]string{"a": "a"}, "a/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolve(tt.path, tt.links); got != tt.want {
				t.Errorf("resolve(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// testImage has a base layer with /sbin -> usr/sbin, an upper layer that
// removes /etc/gone and changes /etc/kept, and two variables in its config.
func testImage(t *testing.T, arch string) v1.Image {
	t.Helper()
	base := tarLayer(t,
		dirEntry("usr/", 0o755),
		dirEntry("usr/sbin/", 0o755),
		linkEntry("sbin", "usr/sbin"),
		dirEntry("proc/", 0o711),
		dirEntry("etc/", 0o755),
		fileEntry("etc/gone", "x", 0o644),
		fileEntry("etc/kept", "old", 0o644),
	)
	upper := tarLayer(t,
		fileEntry("etc/.wh.gone", "", 0o644),
		fileEntry("etc/kept", "new", 0o644),
	)
	img, err := mutate.AppendLayers(empty.Image, base, upper)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg = cfg.DeepCopy()
	cfg.OS, cfg.Architecture = "linux", arch
	cfg.Config.Env = []string{"PATH=/usr/sbin:/usr/bin", "GOFLAGS=-tags=vm"}
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func tarLayer(t *testing.T, entries ...entry) v1.Layer {
	t.Helper()
	b := tarBytes(t, entries...)
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

type entry struct {
	hdr  tar.Header
	body string
}

func dirEntry(name string, mode int64) entry {
	return entry{hdr: tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: mode}}
}

func fileEntry(name, body string, mode int64) entry {
	return entry{hdr: tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(body))}, body: body}
}

func linkEntry(name, target string) entry {
	return entry{hdr: tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777}}
}

func tarBytes(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if err := tw.WriteHeader(&e.hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pushImage pushes img to a registry in this process and returns its reference.
func pushImage(t *testing.T, img v1.Image) string {
	t.Helper()
	reg := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(reg.Close)
	ref := strings.TrimPrefix(reg.URL, "http://") + "/test/base:latest"
	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(parsed, img); err != nil {
		t.Fatal(err)
	}
	return ref
}

// statuses lists the statuses of steps without repeats.
func statuses(steps []Progress) string {
	var out []string
	for _, p := range steps {
		if len(out) == 0 || out[len(out)-1] != string(p.Status) {
			out = append(out, string(p.Status))
		}
	}
	return strings.Join(out, " ")
}

func lastOf(steps []Progress, s Status) Progress {
	var last Progress
	for _, p := range steps {
		if p.Status == s {
			last = p
		}
	}
	return last
}

func names(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return strings.Join(out, " ")
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temp entry %s is left in %s", e.Name(), dir)
		}
	}
}
