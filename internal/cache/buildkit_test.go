package cache

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	gocmd(t, mod, "mod", "init", "buildkitclient")
	gocmd(t, mod, "get", "github.com/tonistiigi/go-actions-cache@latest")
	gocmd(t, mod, "build", "-o", "client", ".")

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
	if entries, _ := f.index.List(context.Background(), "acme/app"); len(entries) != 4 {
		t.Errorf("%d entries, want layer-small, layer-big, index#1, index#2", len(entries))
	}
}

func gocmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, out)
	}
}
