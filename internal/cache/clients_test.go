package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestActionsCacheClient runs the real @actions/cache client against the
// server. It needs Node, npm, and network access for npm install.
func TestActionsCacheClient(t *testing.T) {
	if os.Getenv("RUNPLANE_CACHE_NODE_TEST") == "" {
		t.Skip("set RUNPLANE_CACHE_NODE_TEST=1 to run @actions/cache against the server")
	}
	modules := t.TempDir()
	run(t, modules, "npm", "init", "-y")
	run(t, modules, "npm", "install", "--no-audit", "--no-fund", "@actions/cache@6.3.0")
	module := filepath.Join(modules, "node_modules", "@actions", "cache", "lib", "cache.js")
	script, err := filepath.Abs(filepath.Join("testdata", "client.mjs"))
	if err != nil {
		t.Fatal(err)
	}

	f := newFixture(t, 0)
	// The clock of the fixture stays still; the client needs real expiry times.
	f.srv.now = time.Now
	var blocks, blockLists atomic.Int64
	var sandboxID atomic.Value // of the next client run
	proxied := f.srv.Proxied()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("comp") {
		case "block":
			blocks.Add(1)
		case "blocklist":
			blockLists.Add(1)
		}
		r.Header.Set(SandboxHeader, sandboxID.Load().(string))
		proxied.ServeHTTP(w, r)
	}))
	defer server.Close()

	// The toolkit makes archive paths relative to GITHUB_WORKSPACE, so the
	// workspace path must not go through a symlink (macOS /var).
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	data := filepath.Join(workspace, "data")
	node := func(sandbox string, args ...string) map[string]any {
		t.Helper()
		sandboxID.Store(sandbox)
		cmd := exec.Command("node", append([]string{script, module}, args...)...)
		cmd.Dir = workspace
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + os.Getenv("HOME"),
			"ACTIONS_RESULTS_URL=" + server.URL + "/",
			"ACTIONS_CACHE_SERVICE_V2=true",
			// The client needs a runtime token, but the server ignores its scopes.
			"ACTIONS_RUNTIME_TOKEN=" + token(scope{"refs/heads/main", 3}),
			"GITHUB_WORKSPACE=" + workspace,
			"RUNNER_TEMP=" + temp,
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("node %v: %v\n%s", args, err, out)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		var result map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[len(lines)-1], "RESULT ")), &result); err != nil {
			t.Fatalf("node %v: no result: %v\n%s", args, err, out)
		}
		if bytes.Contains(out, []byte("Failed to")) || bytes.Contains(out, []byte("::warning::")) {
			t.Logf("node %v output:\n%s", args, out)
		}
		return result
	}
	writeData := func(name string, content []byte) {
		t.Helper()
		os.MkdirAll(data, 0o755)
		if err := os.WriteFile(filepath.Join(data, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	readData := func(name string) []byte {
		b, _ := os.ReadFile(filepath.Join(data, name))
		return b
	}

	// A small archive goes up in one Put Blob request.
	writeData("small.txt", []byte("hello from the feature branch"))
	if res := node("sb-feature", "save", "npm-linux-abc"); res["id"] == float64(-1) {
		t.Fatalf("small save failed: %v", res)
	}
	if blocks.Load() != 0 {
		t.Errorf("small save used %d Put Block calls, want Put Blob", blocks.Load())
	}

	// An archive above 128 MiB goes up in 64 MiB blocks. Random bytes do not compress.
	big := make([]byte, 150<<20)
	rand.NewChaCha8([32]byte{1}).Read(big)
	bigSum := sha256.Sum256(big)
	os.RemoveAll(data)
	writeData("big.bin", big)
	big = nil
	if res := node("sb-feature", "save", "big-1"); res["id"] == float64(-1) {
		t.Fatalf("big save failed: %v", res)
	}
	if blocks.Load() < 3 || blockLists.Load() != 1 {
		t.Errorf("big save: %d Put Block and %d Put Block List calls, want at least 3 and 1", blocks.Load(), blockLists.Load())
	}

	os.RemoveAll(data)
	if res := node("sb-feature", "restore", "big-1"); res["matched"] != "big-1" {
		t.Fatalf("restore big-1: %v", res)
	}
	if sha256.Sum256(readData("big.bin")) != bigSum {
		t.Error("restored big.bin differs from the saved file")
	}

	os.RemoveAll(data)
	if res := node("sb-feature", "restore", "npm-linux-abc"); res["matched"] != "npm-linux-abc" {
		t.Errorf("exact restore: %v", res)
	}
	if got := string(readData("small.txt")); got != "hello from the feature branch" {
		t.Errorf("restored small.txt = %q", got)
	}

	os.RemoveAll(data)
	if res := node("sb-feature", "restore", "npm-linux-def", "npm-linux-"); res["matched"] != "npm-linux-abc" {
		t.Errorf("restore by restore key: %v", res)
	}

	// The feature branch entry is not visible from a pull request.
	if res := node("sb-pr1", "restore", "npm-linux-abc"); res["matched"] != nil {
		t.Errorf("pull request restored a feature branch entry: %v", res)
	}

	entries, _ := f.st.ListCacheEntries(context.Background(), "acme/app")
	for _, e := range entries {
		if e.Ref != "refs/heads/feature" || !e.Finalized {
			t.Errorf("entry %q: ref %q finalized %v", e.Key, e.Ref, e.Finalized)
		}
	}
	if len(entries) != 2 {
		t.Errorf("%d entries, want 2", len(entries))
	}
}

// TestBuildKitClient runs the client library of BuildKit's type=gha cache
// (github.com/tonistiigi/go-actions-cache, which uploads with the Azure SDK
// for Go) against the server. It builds a separate module, so it needs
// network access for the module download.
func TestBuildKitClient(t *testing.T) {
	if os.Getenv("RUNPLANE_CACHE_BUILDKIT_TEST") == "" {
		t.Skip("set RUNPLANE_CACHE_BUILDKIT_TEST=1 to run BuildKit's cache client against the server")
	}
	mod := t.TempDir()
	src, err := os.ReadFile(filepath.Join("testdata", "buildkit", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(mod, "main.go"), src, 0o644)
	run(t, mod, "go", "mod", "init", "buildkitclient")
	run(t, mod, "go", "get", "github.com/tonistiigi/go-actions-cache@latest")
	run(t, mod, "go", "build", "-o", "client", ".")

	f := newFixture(t, 0)
	f.srv.now = time.Now
	server := httptest.NewServer(f.srv)
	defer server.Close()

	cmd := exec.Command(filepath.Join(mod, "client"))
	cmd.Env = append(os.Environ(),
		"ACTIONS_RESULTS_URL="+server.URL+"/",
		"ACTIONS_CACHE_SERVICE_V2=true",
		"ACTIONS_RUNTIME_TOKEN="+token(scope{"refs/heads/main", 3}),
	)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "OK") {
		t.Fatalf("client: %v\n%s", err, out)
	}
	if entries, _ := f.st.ListCacheEntries(context.Background(), "acme/app"); len(entries) != 4 {
		t.Errorf("%d entries, want layer-small, layer-big, index#1, index#2", len(entries))
	}
}

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
