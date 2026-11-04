//go:build integration

package mirror

import (
	"io"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// TestPullAlpineFromDockerHub pulls a real image from Docker Hub through the
// mirror two times. go-containerregistry checks each digest. The second pull
// must come from the cache only.
func TestPullAlpineFromDockerHub(t *testing.T) {
	m, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)

	ref, err := name.ParseReference(strings.TrimPrefix(srv.URL, "http://")+"/library/alpine:latest", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	pull := func() v1.Hash {
		img, err := remote.Image(ref, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
		if err != nil {
			t.Fatalf("pull: %v", err)
		}
		if _, err := img.RawConfigFile(); err != nil {
			t.Fatalf("read config: %v", err)
		}
		layers, err := img.Layers()
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range layers {
			rc, err := l.Compressed()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, rc); err != nil {
				t.Fatalf("read layer: %v", err)
			}
			if err := rc.Close(); err != nil {
				t.Fatalf("verify layer: %v", err)
			}
		}
		digest, err := img.Digest()
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}

	first := pull()
	settle(t, m)
	cold := m.Stats()
	second := pull()
	warm := m.Stats()
	t.Logf("image %s; first pull %+v; after second pull %+v", first, cold, warm)

	if second != first {
		t.Errorf("second pull got image %s, want %s", second, first)
	}
	if warm.Misses != cold.Misses || warm.MissBytes != cold.MissBytes {
		t.Errorf("second pull went upstream: misses %d to %d", cold.Misses, warm.Misses)
	}
	if warm.Hits <= cold.Hits {
		t.Errorf("second pull had no cache hits")
	}
}
