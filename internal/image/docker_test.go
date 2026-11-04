package image

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox"
)

const testImageID = "sha256:2d1f2a8f6b0c7b2c6f4d4f9e3a1b5c7d9e0f1a2b3c4d5e6f708192a3b4c5d6e7"

func TestDockerPullReportsAllLayers(t *testing.T) {
	pull := strings.Join([]string{
		`{"status":"Pulling fs layer","id":"a"}`,
		`{"status":"Pulling fs layer","id":"b"}`,
		`{"status":"Downloading","progressDetail":{"current":50,"total":100},"id":"a"}`,
		`{"status":"Downloading","progressDetail":{"current":10,"total":200},"id":"b"}`,
		`{"status":"Download complete","id":"a"}`,
		`{"status":"Downloading","progressDetail":{"current":200,"total":200},"id":"b"}`,
		`{"status":"Pull complete","id":"a"}`,
		`{"status":"Pull complete","id":"b"}`,
	}, "\n")
	m := dockerManager(t, fakeDocker(t, pull, true))
	var steps []Progress
	img, err := m.Ensure(context.Background(), "example.com/ci:1", func(p Progress) { steps = append(steps, p) })
	if err != nil {
		t.Fatal(err)
	}
	if want := (Image{Ref: "example.com/ci:1", Digest: testImageID, Size: 4096}); img != want {
		t.Errorf("image = %+v, want %+v", img, want)
	}
	if p := lastOf(steps, StatusPulling); p.Done != 300 || p.Total != 300 {
		t.Errorf("last pulling step = %+v, want 300 of 300 bytes", p)
	}
}

func TestDockerPullErrorInStream(t *testing.T) {
	pull := `{"status":"Pulling fs layer","id":"a"}` + "\n" +
		`{"errorDetail":{"message":"toomanyrequests: rate limit"},"error":"toomanyrequests: rate limit"}`

	t.Run("no local image", func(t *testing.T) {
		m := dockerManager(t, fakeDocker(t, pull, false))
		_, err := m.Ensure(context.Background(), "example.com/ci:1", nil)
		if err == nil || !strings.Contains(err.Error(), "toomanyrequests") {
			t.Fatalf("Ensure error = %v, want the pull error", err)
		}
	})
	t.Run("local image", func(t *testing.T) {
		m := dockerManager(t, fakeDocker(t, pull, true))
		img, err := m.Ensure(context.Background(), "example.com/ci:1", nil)
		if err != nil || img.Digest != testImageID {
			t.Fatalf("Ensure = %+v, %v; want the local image", img, err)
		}
	})
}

// fakeDocker serves a Docker Engine API with one pull stream and, if
// local, one image.
func fakeDocker(t *testing.T, pull string, local bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/create"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, pull)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/images/example.com/ci:1/json") && local:
			io.WriteString(w, `{"Id":"`+testImageID+`","RepoTags":["example.com/ci:1"],"Size":4096}`)
		default:
			http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dockerManager(t *testing.T, srv *httptest.Server) *Manager {
	t.Helper()
	docker, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithAPIVersion("1.52"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { docker.Close() })
	m, err := New(Config{Mode: sandbox.ModeContainer, Docker: docker, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
