package image

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf16"

	"github.com/runplane/runplane/internal/sandbox"
)

func TestPatchResultsURL(t *testing.T) {
	url := utf16Bytes("ACTIONS_RESULTS_URL")
	orl := utf16Bytes("ACTIONS_RESULTS_ORL")
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	tests := []struct {
		name string
		in   []byte
		want []byte
		n    int
	}{
		{
			name: "each UTF-16 match",
			in:   join([]byte{0x4d, 0x5a}, url, []byte{0, 0, 7}, url),
			want: join([]byte{0x4d, 0x5a}, orl, []byte{0, 0, 7}, orl),
			n:    2,
		},
		{
			name: "adjacent matches",
			in:   join(url, url),
			want: join(orl, orl),
			n:    2,
		},
		{
			name: "ASCII is not a match",
			in:   []byte("xxACTIONS_RESULTS_URLxx"),
			want: []byte("xxACTIONS_RESULTS_URLxx"),
		},
		{
			name: "cut match at the end",
			in:   join([]byte{1}, url[:len(url)-2]),
			want: join([]byte{1}, url[:len(url)-2]),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := bytes.Clone(tt.in)
			if n := patchResultsURL(b); n != tt.n {
				t.Errorf("count = %d, want %d", n, tt.n)
			}
			if !bytes.Equal(b, tt.want) {
				t.Errorf("bytes = %q, want %q", b, tt.want)
			}
			if len(b) != len(tt.in) {
				t.Errorf("length = %d, want %d", len(b), len(tt.in))
			}
		})
	}
}

func TestRelease(t *testing.T) {
	release, err := os.ReadFile("testdata/runner-release-2.338.2.json")
	if err != nil {
		t.Fatal(err)
	}
	var auth string
	gh := fakeGitHub(t, map[string][]byte{"api.github.com/repos/actions/runner/releases/latest": release}, func(r *http.Request) {
		auth = r.Header.Get("Authorization")
	})
	b := NewBundles(BundleConfig{
		HTTP:  gh.Client(),
		Token: func(context.Context) (string, error) { return "ghs_test", nil },
	})
	rel, err := b.release(context.Background(), "/releases/latest")
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer ghs_test" {
		t.Errorf("Authorization = %q", auth)
	}
	if rel.Version != "2.338.2" {
		t.Errorf("version = %q", rel.Version)
	}
	want := map[string]string{
		"x64":   "baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40",
		"arm64": "670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945",
	}
	for arch, sum := range want {
		if rel.SHA256[arch] != sum {
			t.Errorf("SHA-256 %s = %q, want %q", arch, rel.SHA256[arch], sum)
		}
	}
	if len(rel.SHA256) != 2 {
		t.Errorf("SHA-256 for %d arches, want 2: %v", len(rel.SHA256), rel.SHA256)
	}
}

func TestReleaseUsesAssetDigestWithoutMarkers(t *testing.T) {
	release, err := os.ReadFile("testdata/runner-release-2.338.2.json")
	if err != nil {
		t.Fatal(err)
	}
	release = bytes.ReplaceAll(release, []byte("BEGIN SHA"), []byte("BEGIN XXX"))
	gh := fakeGitHub(t, map[string][]byte{"api.github.com/repos/actions/runner/releases/latest": release}, nil)
	rel, err := NewBundles(BundleConfig{HTTP: gh.Client()}).release(context.Background(), "/releases/latest")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rel.SHA256["arm64"], "670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945"; got != want {
		t.Errorf("SHA-256 arm64 = %q, want %q", got, want)
	}
}

func TestEnsureBundle(t *testing.T) {
	dll := bytes.Join([][]byte{[]byte("MZ"), utf16Bytes("ACTIONS_RESULTS_URL"), {0, 0}, utf16Bytes("ACTIONS_RESULTS_URL")}, nil)
	archive := runnerArchive(t, dll)
	sum := sha256.Sum256(archive)
	files := map[string][]byte{
		"api.github.com/repos/actions/runner/releases/tags/v2.338.2":                                     releaseJSON("2.338.2", "arm64", hex.EncodeToString(sum[:])),
		"github.com/actions/runner/releases/download/v2.338.2/actions-runner-linux-arm64-2.338.2.tar.gz": archive,
	}
	var requests atomic.Int32
	gh := fakeGitHub(t, files, func(*http.Request) { requests.Add(1) })
	dir := t.TempDir()
	b := NewBundles(BundleConfig{Mode: sandbox.ModeVM, Dir: dir, HTTP: gh.Client()})

	bundle, err := b.Ensure(context.Background(), "2.338.2", "arm64")
	if err != nil {
		t.Fatal(err)
	}
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
	archive := runnerArchive(t, []byte("MZ"))
	files := map[string][]byte{
		"api.github.com/repos/actions/runner/releases/tags/v2.338.2":                                   releaseJSON("2.338.2", "x64", strings.Repeat("0", 64)),
		"github.com/actions/runner/releases/download/v2.338.2/actions-runner-linux-x64-2.338.2.tar.gz": archive,
	}
	dir := t.TempDir()
	b := NewBundles(BundleConfig{Mode: sandbox.ModeContainer, Dir: dir, HTTP: fakeGitHub(t, files, nil).Client()})
	if _, err := b.Ensure(context.Background(), "2.338.2", "x64"); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("Ensure error = %v, want a SHA-256 error", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("runners folder has %d entries after a bad download, want 0", len(entries))
	}
}

func TestEnsureBundleWithoutPatch(t *testing.T) {
	archive := runnerArchive(t, []byte("MZ no results url here"))
	sum := sha256.Sum256(archive)
	files := map[string][]byte{
		"api.github.com/repos/actions/runner/releases/tags/v2.400.0":                                   releaseJSON("2.400.0", "x64", hex.EncodeToString(sum[:])),
		"github.com/actions/runner/releases/download/v2.400.0/actions-runner-linux-x64-2.400.0.tar.gz": archive,
	}
	b := NewBundles(BundleConfig{Mode: sandbox.ModeContainer, Dir: t.TempDir(), HTTP: fakeGitHub(t, files, nil).Client()})
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

// TestEnsureBundleFromOtherGitHub checks that the release read and the
// download go to the GitHub of APIURL and WebURL, as runplane does with the
// fake GitHub of the end-to-end tests.
func TestEnsureBundleFromOtherGitHub(t *testing.T) {
	archive := runnerArchive(t, utf16Bytes("ACTIONS_RESULTS_URL"))
	sum := sha256.Sum256(archive)
	files := map[string][]byte{
		"/api/v3/repos/actions/runner/releases/tags/v2.338.2":                                releaseJSON("2.338.2", "x64", hex.EncodeToString(sum[:])),
		"/actions/runner/releases/download/v2.338.2/actions-runner-linux-x64-2.338.2.tar.gz": archive,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	b := NewBundles(BundleConfig{Mode: sandbox.ModeContainer, Dir: t.TempDir(), APIURL: srv.URL + "/api/v3", WebURL: srv.URL})
	bundle, err := b.Ensure(context.Background(), "2.338.2", "x64")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.SHA256 != hex.EncodeToString(sum[:]) || !bundle.Patched {
		t.Errorf("bundle = %+v, want the patched release of the other GitHub", bundle)
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
	b := NewBundles(BundleConfig{Dir: dir})
	if err := b.Prune([]string{"2.9.0"}); err != nil {
		t.Fatal(err)
	}
	var left []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	want := []string{"2.300.0-arm64", "2.337.0-x64", "2.338.2-x64", "2.338.2-x64.json", "2.9.0-x64"}
	if strings.Join(left, " ") != strings.Join(want, " ") {
		t.Errorf("after prune: %v\nwant %v", left, want)
	}
}

// fakeGitHub serves files by host and path. Its client sends all requests to it.
func fakeGitHub(t *testing.T, files map[string][]byte, seen func(*http.Request)) *fakeServer {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			seen(r)
		}
		body, ok := files[r.Host+r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return &fakeServer{srv}
}

type fakeServer struct{ *httptest.Server }

func (s *fakeServer) Client() *http.Client {
	target, _ := url.Parse(s.URL)
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(r)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func releaseJSON(version, arch, sum string) []byte {
	return fmt.Appendf(nil, `{"tag_name":"v%s","published_at":"2026-10-26T14:45:12Z","body":"- actions-runner-linux-%s-%s.tar.gz <!-- BEGIN SHA linux-%s -->%s<!-- END SHA linux-%s -->","assets":[]}`,
		version, arch, version, arch, sum, arch)
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
