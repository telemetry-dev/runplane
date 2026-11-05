package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

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
	d := newDriver(Options{
		DataDir:     filepath.Join(root, "data"),
		ServicePort: 8486,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}, idRange{uid: os.Getuid(), gid: os.Getgid(), count: 16}, sys)
	d.cgroupRoot = filepath.Join(root, "cgroup")
	t.Cleanup(func() { d.Close() })
	return d
}

// recovered is a driver after a restart of runplane.
func recovered(t *testing.T, root string, sys hostOps) *Driver {
	t.Helper()
	d := testDriver(t, root, sys)
	if err := d.recover(); err != nil {
		t.Fatal(err)
	}
	return d
}

// writeVM leaves the files of a VM as runplane does before a restart: the
// record, the chroot, and a scope with a process.
func writeVM(t *testing.T, root, id string, tap int, state sandbox.State, created time.Time) {
	t.Helper()
	spec := sandbox.Spec{ID: id, PoolID: "pool1", VCPU: 1, MemoryMB: 512, DiskGB: 1, TimeLimit: time.Hour}
	b, err := json.Marshal(record{Spec: spec, Tap: tap, CreatedAt: created, State: state})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "data", "sandboxes", id, recordFile), string(b))
	writeFile(t, filepath.Join(root, "data", "jail", "firecracker", id, "root", scratchFile), "")
	writeFile(t, scopeProcs(root, id), "4242\n")
}

func scopeProcs(root, id string) string {
	return filepath.Join(root, "cgroup", "runplane.slice", "runplane-vm-"+id+".scope", "cgroup.procs")
}

// dialGuest connects to a control socket as runplane-init does through
// vsock. It returns a client for the host, and calls onShutdown when the
// host asks the guest to power off.
func dialGuest(t *testing.T, socket string, onShutdown func()) *http.Client {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := yamux.Client(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		onShutdown()
	})
	go http.Serve(sess, mux)
	return &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return sess.Open() },
	}}
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

// shortTempDir is a temp folder with a short path, because Unix socket
// paths have a limit of about 100 bytes.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rpfc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// At a host shutdown, systemd stops the VM scopes after runplane. Close
// asks those VMs to power off, so that their runners cancel the jobs, and
// leaves the other VMs running for the next start of runplane.
func TestCloseStopsTheVMsThatSystemdStops(t *testing.T) {
	root := shortTempDir(t)
	ids := []string{"vm1", "vm2"}
	for i, id := range ids {
		writeVM(t, root, id, i, sandbox.StateBusy, time.Now().UTC())
	}
	sys := &fakeHost{stopping: map[string]bool{"runplane-vm-vm1.scope": true, "runplane.service": true}}
	d := recovered(t, root, sys)
	shutdowns := make(chan string, len(ids))
	for _, id := range ids {
		dialGuest(t, filepath.Join(d.chroot(id), controlSocket), func() {
			shutdowns <- id
			writeFile(t, scopeProcs(root, id), "") // the guest powers off
		})
		v := d.vms[id]
		for start := time.Now(); !v.ctl.Connected(); time.Sleep(time.Millisecond) {
			if time.Since(start) > 5*time.Second {
				t.Fatalf("the guest of %s does not connect", id)
			}
		}
	}

	d.Close()
	close(shutdowns)
	var got []string
	for id := range shutdowns {
		got = append(got, id)
	}
	if !slices.Equal(got, []string{"vm1"}) {
		t.Errorf("shutdown requests for %v, want vm1 only", got)
	}
	if killed := sys.calls(&sys.killed); len(killed) != 0 {
		t.Errorf("scopes killed: %v, want none", killed)
	}
	for id, want := range map[string]bool{"vm1": false, "vm2": true} {
		if exists(d.sandboxDir(id)) != want || exists(d.jailDir(id)) != want {
			t.Errorf("the folders of %s exist: %v, want %v", id, !want, want)
		}
	}
}

func TestParseStopJobs(t *testing.T) {
	out := " 1512 runplane-vm-01jc8q5w1p.scope stop    waiting\n" +
		" 1513 runplane-vm-01jc8q5w2x.scope start   waiting\n" +
		" 1490 runplane.service             restart running\n"
	got := parseStopJobs([]byte(out))
	if len(got) != 1 || !got["runplane-vm-01jc8q5w1p.scope"] {
		t.Errorf("got %v, want the stop job of runplane-vm-01jc8q5w1p.scope only", got)
	}
	if got := parseStopJobs([]byte("No jobs running.\n")); len(got) != 0 {
		t.Errorf("got %v from no jobs", got)
	}
}
