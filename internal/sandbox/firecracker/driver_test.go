package firecracker

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/netrules"
)

// fakeHost records the host changes of the driver. stopping holds the
// scopes that systemd is about to stop.
type fakeHost struct {
	mu                           sync.Mutex
	added, kept, dropped, killed []string
	stopping                     map[string]bool
}

func (h *fakeHost) setupNet(context.Context, netrules.Config) error { return nil }
func (h *fakeHost) close() error                                    { return nil }
func (h *fakeHost) stopJobs() (map[string]bool, error)              { return h.stopping, nil }
func (h *fakeHost) probe(context.Context, netrules.Lease, netip.Addr, string) error {
	return errors.ErrUnsupported
}

func (h *fakeHost) addTap(l netrules.Lease, _, _ int) error { return h.add(&h.added, l.Tap) }
func (h *fakeHost) keepTap(l netrules.Lease) error          { return h.add(&h.kept, l.Tap) }
func (h *fakeHost) dropTap(l netrules.Lease) error          { return h.add(&h.dropped, l.Tap) }
func (h *fakeHost) killScope(unit string) error             { return h.add(&h.killed, unit) }

func (h *fakeHost) add(list *[]string, s string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*list = append(*list, s)
	return nil
}

func (h *fakeHost) calls(list *[]string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(*list)
}

// testDriver is a driver with its data folder and cgroup tree in root.
func testDriver(t *testing.T, root string, sys hostOps) *Driver {
	t.Helper()
	opts := withDefaults(Options{
		DataDir: filepath.Join(root, "data"),
		Logger:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	d, err := newDriver(opts, idRange{uid: os.Getuid(), gid: os.Getgid(), count: 16}, sys)
	if err != nil {
		t.Fatal(err)
	}
	d.cgroupRoot = filepath.Join(root, "cgroup")
	t.Cleanup(func() { d.Close() })
	return d
}

func nextEvent(t *testing.T, d *Driver) sandbox.Event {
	t.Helper()
	select {
	case ev := <-d.Events():
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return sandbox.Event{}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestGuestConfig(t *testing.T) {
	d := testDriver(t, t.TempDir(), &fakeHost{})
	d.opts.CacheEnv = func(url string) map[string]string {
		return map[string]string{"ACTIONS_RESULTS_URL": url + "/"}
	}
	lease := netrules.Lease{Index: 3, Tap: "rp-3", Host: netip.MustParseAddr("10.214.0.13"), Guest: netip.MustParseAddr("10.214.0.14")}
	spec := sandbox.Spec{ID: "01m6jwd94yx8aj3sdamaedysaw", JITConfig: "jit", TimeLimit: 90 * time.Minute, Env: map[string]string{"A": "1"}, Debug: true}

	all := sandbox.Settings{ActionsCache: true, RegistryMirror: true, DNS: []string{"1.1.1.1", "2606:4700::1111", "9.9.9.9"}}
	cfg := d.guestConfig(spec, lease, all)
	// The services listen on the host end of the TAP, the gateway of the guest.
	if want := map[string]string{"A": "1", "ACTIONS_RESULTS_URL": "http://10.214.0.13:8486/"}; !mapsEqual(cfg.Env, want) {
		t.Errorf("env %v, want %v", cfg.Env, want)
	}
	if cfg.RegistryMirror != "http://10.214.0.13:8486" {
		t.Errorf("registry mirror %q", cfg.RegistryMirror)
	}
	if !slices.Equal(cfg.DNS, []string{"1.1.1.1", "9.9.9.9"}) {
		t.Errorf("DNS %v, want the IPv4 servers only", cfg.DNS)
	}
	if cfg.JITConfig != "jit" || cfg.TimeLimitSeconds != 5400 || !cfg.Debug || cfg.RunnerDir != "/opt/runplane/runner" {
		t.Errorf("config %+v", cfg)
	}
	// The first ULID characters hold the time, which VMs of the same
	// second share; the host name takes the random end, as the runner name does.
	if cfg.Hostname != "runplane-maedysaw" {
		t.Errorf("host name %q, want runplane-maedysaw", cfg.Hostname)
	}

	none := d.guestConfig(spec, lease, sandbox.Settings{})
	if !mapsEqual(none.Env, map[string]string{"A": "1"}) || none.RegistryMirror != "" {
		t.Errorf("with the cache and mirror off: env %v, mirror %q", none.Env, none.RegistryMirror)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func TestDeadline(t *testing.T) {
	d := testDriver(t, t.TempDir(), &fakeHost{})
	created := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		limit      time.Duration
		jobStarted time.Time
		hold       time.Time
		want       time.Time
	}{
		// Without a job: the limit, the idle grace of 5 minutes, and the 1 minute for init.
		{"a warm VM", time.Hour, time.Time{}, time.Time{}, created.Add(66 * time.Minute)},
		// A VM that waited 3 hours for its job gets the full hour for it.
		{"a job after a long wait", time.Hour, created.Add(3 * time.Hour), time.Time{}, created.Add(4*time.Hour + time.Minute)},
		{"a debug hold past the limit", time.Hour, created.Add(10 * time.Minute), created.Add(2 * time.Hour), created.Add(121 * time.Minute)},
		{"a hold inside the limit", time.Hour, created.Add(10 * time.Minute), created.Add(20 * time.Minute), created.Add(71 * time.Minute)},
		{"no time limit", 0, created.Add(10 * time.Minute), time.Time{}, time.Time{}},
	}
	for _, tt := range tests {
		v := newVM(d, sandbox.Spec{ID: "vm1", TimeLimit: tt.limit}, d.taps.Lease(0), created)
		v.jobStarted, v.holdUntil = tt.jobStarted, tt.hold
		if got := v.deadline(); !got.Equal(tt.want) {
			t.Errorf("%s: deadline %v, want %v", tt.name, got, tt.want)
		}
	}
}
