package image

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf16"

	"github.com/runplane/runplane/internal/sandbox"
)

func TestRelease(t *testing.T) {
	release, err := os.ReadFile("testdata/runner-release-2.338.2.json")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"x64":   "baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40",
		"arm64": "670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945",
	}
	tests := []struct {
		name string
		body []byte
	}{
		{"SHA markers in the notes", release},
		{"asset digests without markers", bytes.ReplaceAll(release, []byte("BEGIN SHA"), []byte("BEGIN XXX"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBundles(BundleConfig{Token: func(context.Context) (string, error) { return "ghs_test", nil }})
			var auth string
			fakeGitHub(t, b, map[string][]byte{"/repos/actions/runner/releases/latest": tt.body}, func(r *http.Request) {
				auth = r.Header.Get("Authorization")
			})
			version, sums, err := b.release(context.Background(), "/releases/latest")
			if err != nil {
				t.Fatal(err)
			}
			if auth != "Bearer ghs_test" || version != "2.338.2" || !maps.Equal(sums, want) {
				t.Errorf("auth %q, version %q, SHA-256 %v; want %v", auth, version, sums, want)
			}
		})
	}
}

func TestEnsureBundle(t *testing.T) {
	dll := bytes.Join([][]byte{[]byte("MZ"), utf16Bytes("ACTIONS_RESULTS_URL"), {0, 0}, utf16Bytes("ACTIONS_RESULTS_URL")}, nil)
	archive := runnerArchive(t, dll)
	dir := t.TempDir()
	b := NewBundles(BundleConfig{Mode: sandbox.ModeVM, Dir: dir})
	requests := serveRunner(t, b, "2.338.2", "arm64", archive, sha256.Sum256(archive))

	bundle, err := b.Ensure(context.Background(), "2.338.2", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	want := Bundle{
		Version:  "2.338.2",
		Arch:     "arm64",
		SHA256:   hex.EncodeToString(sum[:]),
		Patched:  true,
		Dir:      filepath.Join(dir, "2.338.2-arm64"),
		Ext4Path: filepath.Join(dir, "2.338.2-arm64.ext4"),
	}
	if bundle != want {
		t.Fatalf("bundle = %+v\nwant     %+v", bundle, want)
	}

	got, err := os.ReadFile(filepath.Join(bundle.Dir, "bin", "Runner.Worker.dll"))
	if err != nil {
		t.Fatal(err)
	}
	wantDLL := bytes.Join([][]byte{[]byte("MZ"), utf16Bytes("ACTIONS_RESULTS_ORL"), {0, 0}, utf16Bytes("ACTIONS_RESULTS_ORL")}, nil)
	if !bytes.Equal(got, wantDLL) {
		t.Errorf("Runner.Worker.dll = %q, want %q", got, wantDLL)
	}
	if target, err := os.Readlink(filepath.Join(bundle.Dir, "externals", "node", "bin", "npm")); err != nil || target != "../lib/npm-cli.js" {
		t.Errorf("npm link = %q, %v", target, err)
	}
	assertExt4(t, bundle.Ext4Path)
	debugfs(t, bundle.Ext4Path, "cat /bin/Runner.Worker.dll", string(wantDLL))
	debugfs(t, bundle.Ext4Path, "stat /externals/node/bin/npm", `Fast link dest: "../lib/npm-cli.js"`)

	before := requests.Load()
	again, err := b.Ensure(context.Background(), "2.338.2", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if again != want {
		t.Errorf("second bundle = %+v, want %+v", again, want)
	}
	if n := requests.Load() - before; n != 0 {
		t.Errorf("second Ensure made %d requests, want 0", n)
	}
	assertNoTemp(t, dir)
}

func TestEnsureBundleRejectsBadSHA(t *testing.T) {
	dir := t.TempDir()
	b := NewBundles(BundleConfig{Mode: sandbox.ModeContainer, Dir: dir})
	serveRunner(t, b, "2.338.2", "x64", runnerArchive(t, []byte("MZ")), [32]byte{})
	if _, err := b.Ensure(context.Background(), "2.338.2", "x64"); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("Ensure error = %v, want a SHA-256 error", err)
	}
	if left := names(t, dir); left != "" {
		t.Errorf("runners folder has %s after a bad download, want nothing", left)
	}
}

func TestEnsureBundleWithoutPatch(t *testing.T) {
	archive := runnerArchive(t, []byte("MZ no results url here"))
	b := NewBundles(BundleConfig{Mode: sandbox.ModeContainer, Dir: t.TempDir()})
	serveRunner(t, b, "2.400.0", "x64", archive, sha256.Sum256(archive))
	bundle, err := b.Ensure(context.Background(), "2.400.0", "x64")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Patched || bundle.Ext4Path != "" {
		t.Errorf("bundle = %+v, want unpatched and no ext4 drive", bundle)
	}
	got, err := os.ReadFile(filepath.Join(bundle.Dir, "bin", "Runner.Worker.dll"))
	if err != nil || string(got) != "MZ no results url here" {
		t.Errorf("Runner.Worker.dll = %q, %v", got, err)
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"2.9.0-x64", "2.300.0-x64", "2.338.2-x64", "2.337.0-x64", "2.300.0-arm64", ".2.339.0-x64.tmp-1"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"2.300.0-x64.json", "2.300.0-x64.ext4", "2.338.2-x64.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := NewBundles(BundleConfig{Dir: dir}).Prune([]string{"2.9.0"}); err != nil {
		t.Fatal(err)
	}
	if left, want := names(t, dir), "2.300.0-arm64 2.337.0-x64 2.338.2-x64 2.338.2-x64.json 2.9.0-x64"; left != want {
		t.Errorf("after prune: %s\nwant %s", left, want)
	}
}

// fakeGitHub serves files by path, and points the GitHub URLs of b at it.
func fakeGitHub(t *testing.T, b *Bundles, files map[string][]byte, seen func(*http.Request)) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			seen(r)
		}
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	b.api.URL, b.web = srv.URL, srv.URL
}

// serveRunner serves a runner release whose notes give sum for the archive,
// and returns the count of requests.
func serveRunner(t *testing.T, b *Bundles, version, arch string, archive []byte, sum [32]byte) *atomic.Int32 {
	release := fmt.Appendf(nil, `{"tag_name":"v%s","body":"<!-- BEGIN SHA linux-%s -->%x<!-- END SHA linux-%s -->","assets":[]}`,
		version, arch, sum, arch)
	download := fmt.Sprintf("/actions/runner/releases/download/v%s/actions-runner-linux-%s-%s.tar.gz", version, arch, version)
	var requests atomic.Int32
	fakeGitHub(t, b, map[string][]byte{"/repos/actions/runner/releases/tags/v" + version: release, download: archive},
		func(*http.Request) { requests.Add(1) })
	return &requests
}

func runnerArchive(t *testing.T, dll []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(tarBytes(t,
		dirEntry("./", 0o755),
		dirEntry("./bin/", 0o755),
		fileEntry("./bin/Runner.Worker.dll", string(dll), 0o644),
		fileEntry("./run.sh", "#!/bin/bash\n", 0o755),
		fileEntry("./externals/node/lib/npm-cli.js", "npm", 0o644),
		linkEntry("./externals/node/bin/npm", "../lib/npm-cli.js"),
	))
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func utf16Bytes(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

// assertNoTemp fails if dir has a temp file of an Ensure call.
func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temp entry %s is left in %s", e.Name(), dir)
		}
	}
}
