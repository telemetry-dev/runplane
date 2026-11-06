package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The dashboard reads the update status on each visit. A repository with no
// release has no update, and GitHub sees one read in checkTTL.
func TestServiceCheck(t *testing.T) {
	var reads, status atomic.Int32
	status.Store(http.StatusNotFound)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	s := NewService(Config{
		Updater: &Updater{Repo: "runplane/runplane", Current: "v1.0.0", APIURL: srv.URL},
		Now:     func() time.Time { return now },
	})
	ctx := context.Background()

	for range 2 {
		st, err := s.Check(ctx)
		if err != nil || st.Available || st.Current != "v1.0.0" {
			t.Fatalf("Check = %+v, %v; want v1.0.0 without an update", st, err)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("%d release reads for two checks, want 1", n)
	}

	// A GitHub fault is not kept, so the next visit reads again. A 403
	// fault, because the client tries a 5xx again.
	now = now.Add(checkTTL)
	status.Store(http.StatusForbidden)
	if _, err := s.Check(ctx); err == nil {
		t.Fatal("Check with a GitHub fault returned no error")
	}
	s.Check(ctx)
	if n := reads.Load(); n != 3 {
		t.Errorf("%d release reads after two failed checks, want 3", n)
	}
}

// The dashboard shows an update while it installs and while runplane
// restarts. A second update during the first gets ErrBusy.
func TestServiceAppliesOneUpdate(t *testing.T) {
	downloading, release := make(chan struct{}), make(chan struct{})
	var downloads atomic.Int32
	files := serve(t, loadRelease(t))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/runplane/releases/latest":
			w.Write([]byte(`{"tag_name":"v0.5.0","html_url":"https://github.com/acme/runplane/releases/tag/v0.5.0"}`))
			return
		case "/acme/runplane/releases/download/v0.5.0/SHA256SUMS":
			// The first update waits here. A second one must not get here.
			if downloads.Add(1) > 1 {
				http.Error(w, "a second update downloads", http.StatusConflict)
				return
			}
			close(downloading)
			<-release
		}
		files.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	releaseFirst := sync.OnceFunc(func() { close(release) })
	defer releaseFirst() // before srv.Close, which waits for the first update
	exe := filepath.Join(t.TempDir(), "runplane")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	s := NewService(Config{
		Updater: &Updater{Repo: "acme/runplane", Current: "v0.4.2", PublicKey: releaseKey, APIURL: srv.URL, DownloadURL: srv.URL, Exe: exe},
		Restart: func() { restarts++ },
	})
	ctx := context.Background()

	if st := s.State(); st != Idle {
		t.Fatalf("state before the update = %q, want idle", st)
	}
	done := make(chan error)
	go func() {
		_, err := s.Apply(ctx)
		done <- err
	}()
	<-downloading
	if st := s.State(); st != Applying {
		t.Fatalf("state during the update = %q, want applying", st)
	}
	if _, err := s.Apply(ctx); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second update during the first: %v, want ErrBusy", err)
	}
	releaseFirst()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st != Restarting || restarts != 1 {
		t.Fatalf("after the update: state %q, %d restarts; want restarting and 1", st, restarts)
	}
	if got, _ := os.ReadFile(exe); string(got) != "#!/bin/sh\necho runplane v0.5.0\n" {
		t.Errorf("executable has %q, want the release", got)
	}
}

func TestServiceAppliesOnlyANewerRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"v0.5.0"}`))
	}))
	defer srv.Close()
	s := NewService(Config{
		Updater: &Updater{Repo: "acme/runplane", Current: "v0.5.0", PublicKey: releaseKey, APIURL: srv.URL, DownloadURL: srv.URL},
		Restart: func() { t.Error("restart without an update") },
	})
	if _, err := s.Apply(context.Background()); !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("Apply = %v, want ErrNoUpdate", err)
	}
	if st := s.State(); st != Idle {
		t.Errorf("state %q, want idle", st)
	}
}
