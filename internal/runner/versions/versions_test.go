package versions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/ghrest"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/runner"
	"github.com/runplane/runplane/internal/store"
)

func TestCheck(t *testing.T) {
	tests := []struct {
		name      string
		runtime   time.Duration // runtime end of the version in use, from now
		wantWarns int
	}{
		{"end in 10 days warns once", 10 * 24 * time.Hour, 1},
		{"end in 20 days does not warn", 20 * 24 * time.Hour, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			st := openStore(t)
			for i, arch := range []string{"x64", "arm64", "x64"} {
				id := string(rune('a' + i))
				err := st.CreateServer(ctx, store.Server{ID: id, Name: id, Kind: store.ServerAgent, Status: store.ServerOnline,
					ServerFacts: store.ServerFacts{Arch: arch}, CreatedAt: time.Now(), UpdatedAt: time.Now()})
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, arch := range []string{"x64", "arm64"} {
				err := st.SetRunnerBundleBuild(ctx, store.RunnerBundle{Version: "2.330.0", Arch: arch, SHA256: "sha-" + arch,
					Patched: true, Status: store.BundleReady}, time.Now())
				if err != nil {
					t.Fatal(err)
				}
			}
			runtimeEnd := time.Now().Add(tt.runtime).UTC().Truncate(time.Second)
			mux := http.NewServeMux()
			mux.HandleFunc("GET /repos/actions/runner/releases/latest", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{"tag_name": "v2.331.0", "published_at": "2026-11-01T12:00:00Z"})
			})
			mux.HandleFunc("GET /orgs/acme/actions/runners/deprecations/{version}", func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer ghs_acme" {
					writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
					return
				}
				switch r.PathValue("version") {
				case "2.330.0":
					writeJSON(w, http.StatusOK, map[string]any{"runner_version": "2.330.0",
						"registration_deprecates_at": runtimeEnd.Add(48 * time.Hour), "runtime_deprecates_at": runtimeEnd})
				case "2.331.0":
					writeJSON(w, http.StatusOK, map[string]any{"runner_version": "2.331.0",
						"registration_deprecates_at": nil, "runtime_deprecates_at": nil})
				default:
					writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
				}
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			bus := &events.Bus{}
			notes := &fakeNotifier{}
			cfg := Config{
				Store: st, Bus: bus, GitHub: fakeGitHub{}, Notify: notes.notify, API: ghrest.Client{URL: srv.URL},
				InUse: func() []string { return []string{"2.330.0"} },
			}
			published := events.Subscribe[events.RunnerVersion](t.Context(), bus)

			if err := New(cfg).check(ctx); err != nil {
				t.Fatal(err)
			}
			released := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			for _, arch := range []string{"x64", "arm64"} {
				latest, err := st.GetRunnerBundle(ctx, "2.331.0", arch)
				if err != nil || latest.Status != store.BundlePending || latest.ReleasedAt == nil || !latest.ReleasedAt.Equal(released) || latest.EndDate != nil {
					t.Errorf("2.331.0 %s = %+v, %v", arch, latest, err)
				}
				old, _ := st.GetRunnerBundle(ctx, "2.330.0", arch)
				if old.Status != store.BundleReady || old.SHA256 != "sha-"+arch || !old.Patched {
					t.Errorf("the end date write changed the build columns: %+v", old)
				}
				if old.EndDate == nil || !old.EndDate.Equal(runtimeEnd) {
					t.Errorf("2.330.0 %s end date = %v, want the runtime end %v", arch, old.EndDate, runtimeEnd)
				}
			}
			if n := countEvents(published); n != 4 {
				t.Errorf("published %d runner.version events, want 4", n)
			}
			sent := notes.sent
			if len(sent) != tt.wantWarns || len(sent) > 0 && (sent[0].Type != notify.RunnerDeprecation || sent[0].Data["version"] != "2.330.0") {
				t.Errorf("warnings = %+v, want %d for 2.330.0", sent, tt.wantWarns)
			}

			// The next hour changes nothing, and neither does a restart.
			if err := New(cfg).check(ctx); err != nil {
				t.Fatal(err)
			}
			if len(notes.sent) != tt.wantWarns || countEvents(published) != 0 {
				t.Errorf("the second check sent %d warnings and published events, want %d and none", len(notes.sent), tt.wantWarns)
			}
		})
	}
}

// On a new install, the first check can run before the primary records the
// arch of its server. The first runner bundle must not wait for the next
// check one hour later.
func TestVersionsBeforeTheServersHaveAnArch(t *testing.T) {
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/actions/runner/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"tag_name": "v2.331.0", "published_at": "2026-11-01T12:00:00Z"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := New(Config{
		Store: openStore(t), Bus: &events.Bus{}, GitHub: fakeGitHub{}, Notify: (&fakeNotifier{}).notify, API: ghrest.Client{URL: srv.URL},
		InUse: func() []string { return nil },
	})

	if got, err := s.Versions(ctx, "x64"); err != nil || got != nil {
		t.Errorf("versions before the first check: %v, %v; want none", got, err)
	}
	if err := s.check(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Versions(ctx, "x64"); err != nil || !slices.Equal(got, []string{"2.331.0"}) {
		t.Errorf("versions after the check: %v, %v; want the latest release 2.331.0", got, err)
	}
}

// The warning names a version that new sandboxes get, also when a newer
// version is ready on another server.
func TestWarnNamesTheVersionsInUse(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	soon := time.Now().Add(24 * time.Hour)
	for _, v := range []string{"2.330.0", "2.331.0"} {
		if err := st.SetRunnerBundleBuild(ctx, store.RunnerBundle{Version: v, Arch: "x64", Status: store.BundleReady}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := st.SetRunnerBundleDates(ctx, v, "x64", nil, &soon, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	notes := &fakeNotifier{}
	s := New(Config{Store: st, Bus: &events.Bus{}, Notify: notes.notify, InUse: func() []string { return []string{"2.330.0"} }})
	if err := s.warnEnds(ctx); err != nil {
		t.Fatal(err)
	}
	if len(notes.sent) != 1 || notes.sent[0].Data["version"] != "2.330.0" {
		t.Errorf("warnings = %+v, want one for 2.330.0", notes.sent)
	}
}

func TestBuiltKeepsAReadyBundle(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	bus := &events.Bus{}
	published := events.Subscribe[events.RunnerVersion](t.Context(), bus)
	s := New(Config{Store: st, Bus: bus})

	s.Built(ctx, store.RunnerBundle{Version: "2.330.0", Arch: "x64", SHA256: "9e2f", Patched: true, Status: store.BundleReady})
	// Another server cannot download the version.
	s.Built(ctx, store.RunnerBundle{Version: "2.330.0", Arch: "x64", Status: store.BundleFailed, Error: errNotFound.Error()})
	b, err := st.GetRunnerBundle(ctx, "2.330.0", "x64")
	if err != nil || b.Status != store.BundleReady || b.SHA256 != "9e2f" || !b.Patched {
		t.Errorf("bundle = %+v, %v; want ready with the first build", b, err)
	}
	if n := countEvents(published); n != 1 {
		t.Errorf("published %d runner.version events, want 1", n)
	}
	s.Built(ctx, store.RunnerBundle{Version: "2.331.0", Arch: "x64", Status: store.BundleFailed, Error: errNotFound.Error()})
	if b, _ := st.GetRunnerBundle(ctx, "2.331.0", "x64"); b.Status != store.BundleFailed || b.Error != errNotFound.Error() {
		t.Errorf("bundle = %+v, want failed with the error", b)
	}
}

var errNotFound = &ghrest.APIError{Method: http.MethodGet, Path: "/download", Status: http.StatusNotFound}

type fakeGitHub struct{}

func (fakeGitHub) Token(context.Context) (string, error) { return "ghs_any", nil }

func (fakeGitHub) ReadScope(context.Context) (string, ghrest.Auth, bool) {
	return "/orgs/acme", runner.TokenAuth(func(context.Context) (string, error) { return "ghs_acme", nil }), true
}

type fakeNotifier struct{ sent []notify.Event }

func (f *fakeNotifier) notify(e notify.Event) { f.sent = append(f.sent, e) }

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// countEvents counts the events on ch so far.
func countEvents[E any](ch <-chan E) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}
