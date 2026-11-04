package github

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/store"
)

type fakeBundles struct{ versions []string }

func (f *fakeBundles) EnsureLatest(_ context.Context, version string) error {
	f.versions = append(f.versions, version)
	return nil
}

type fakeNotifier struct {
	mu   sync.Mutex
	sent []notify.Event
}

func (f *fakeNotifier) Notify(_ context.Context, e notify.Event) error {
	f.mu.Lock()
	f.sent = append(f.sent, e)
	f.mu.Unlock()
	return nil
}

func TestCheckReleases(t *testing.T) {
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
			e := newEnv(t)
			e.connect()
			e.installOrg(10, "acme")
			ctx := context.Background()
			bundles, notifier := &fakeBundles{}, &fakeNotifier{}
			e.svc.bundles, e.svc.notify = bundles, notifier
			for _, srv := range []struct{ id, arch string }{{"s1", "x64"}, {"s2", "arm64"}, {"s3", "x64"}} {
				err := e.st.CreateServer(ctx, store.Server{ID: srv.id, Name: srv.id, Kind: store.ServerAgent, Status: store.ServerOnline,
					ServerFacts: store.ServerFacts{Arch: srv.arch}, CreatedAt: time.Now(), UpdatedAt: time.Now()})
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, arch := range []string{"x64", "arm64"} {
				err := e.st.UpsertRunnerBundle(ctx, store.RunnerBundle{Version: "2.330.0", Arch: arch, SHA256: "sha-" + arch,
					Patched: true, Status: store.BundleReady, Path: "/runners/2.330.0-" + arch, CreatedAt: time.Now()})
				if err != nil {
					t.Fatal(err)
				}
			}
			runtimeEnd := time.Now().Add(tt.runtime).UTC().Truncate(time.Second)
			e.mux.HandleFunc("GET /api/v3/repos/actions/runner/releases/latest", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{"tag_name": "v2.331.0", "name": "v2.331.0",
					"published_at": "2026-11-01T12:00:00Z", "draft": false, "prerelease": false})
			})
			e.mux.HandleFunc("GET /api/v3/orgs/acme/actions/runners/deprecations/{version}", func(w http.ResponseWriter, r *http.Request) {
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
			published, unsubscribe := e.bus.Subscribe(16)
			defer unsubscribe()

			if err := e.svc.checkReleases(ctx); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(bundles.versions, []string{"2.331.0"}) {
				t.Errorf("EnsureLatest got %v", bundles.versions)
			}
			released := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			for _, arch := range []string{"x64", "arm64"} {
				latest, err := e.st.GetRunnerBundle(ctx, "2.331.0", arch)
				if err != nil {
					t.Fatalf("no row for 2.331.0 %s: %v", arch, err)
				}
				if latest.Status != store.BundlePending || latest.ReleasedAt == nil || !latest.ReleasedAt.Equal(released) || latest.EndDate != nil {
					t.Errorf("2.331.0 %s = %+v", arch, latest)
				}
				old, _ := e.st.GetRunnerBundle(ctx, "2.330.0", arch)
				if old.Status != store.BundleReady || old.SHA256 != "sha-"+arch || old.Path == "" || !old.Patched {
					t.Errorf("the end date write changed the build columns: %+v", old)
				}
				if old.EndDate == nil || !old.EndDate.Equal(runtimeEnd) {
					t.Errorf("2.330.0 %s end date = %v, want the runtime end %v", arch, old.EndDate, runtimeEnd)
				}
			}
			if n := countEvents(published); n != 4 {
				t.Errorf("published %d runner.version events, want 4", n)
			}
			if len(notifier.sent) != tt.wantWarns {
				t.Fatalf("warnings = %+v, want %d", notifier.sent, tt.wantWarns)
			}
			if tt.wantWarns > 0 && (notifier.sent[0].Type != notify.RunnerDeprecation || notifier.sent[0].Data["version"] != "2.330.0") {
				t.Errorf("warning = %+v", notifier.sent[0])
			}

			// The next hour changes nothing.
			if err := e.svc.checkReleases(ctx); err != nil {
				t.Fatal(err)
			}
			if len(notifier.sent) != tt.wantWarns {
				t.Errorf("warnings after the second check = %d, want %d", len(notifier.sent), tt.wantWarns)
			}
			if n := countEvents(published); n != 0 {
				t.Errorf("the second check published %d events, want 0", n)
			}
		})
	}
}

func countEvents(ch <-chan events.Event) int {
	n := 0
	for {
		select {
		case e := <-ch:
			if e.Type == events.RunnerVersion {
				n++
			}
		default:
			return n
		}
	}
}
