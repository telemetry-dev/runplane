package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
	"github.com/runplane/runplane/internal/sandbox/netrules"
)

// fakeHost records the host changes of the driver. stopping holds the
// scopes that systemd is about to stop, and netErr fails the firewall
// writes.
type fakeHost struct {
	mu                           sync.Mutex
	opened, kept, closed, killed []string
	nets                         []netrules.Config
	netErr                       error
	stopping                     map[string]bool
	next                         int // the index of the next lease
}

func (h *fakeHost) setupNet(_ context.Context, cfg netrules.Config) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nets = append(h.nets, cfg)
	return h.netErr
}

func (h *fakeHost) tapLeases() int                     { return 16384 }
func (h *fakeHost) stopJobs() (map[string]bool, error) { return h.stopping, nil }
func (h *fakeHost) probe(context.Context, netrules.Lease, netip.Addr) error {
	return errors.ErrUnsupported
}

func (h *fakeHost) acquireTap() (netrules.Lease, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	return leaseOf(h.next - 1), nil
}

func (h *fakeHost) openTap(l netrules.Lease, _, _ int) error { return h.add(&h.opened, l.Tap) }
func (h *fakeHost) keepTap(l netrules.Lease) error           { return h.add(&h.kept, l.Tap) }
func (h *fakeHost) closeTap(l netrules.Lease) error          { return h.add(&h.closed, l.Tap) }
func (h *fakeHost) killScope(unit string) error              { return h.add(&h.killed, unit) }

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

// leaseOf is lease i of 10.214.0.0/16, for i below 64.
func leaseOf(i int) netrules.Lease {
	return netrules.Lease{
		Index: i, Tap: fmt.Sprintf("rp-%d", i),
		Host: netip.AddrFrom4([4]byte{10, 214, 0, byte(4*i + 1)}), Guest: netip.AddrFrom4([4]byte{10, 214, 0, byte(4*i + 2)}),
	}
}

// testDriver is a driver with its data folder and cgroup tree in root.
func testDriver(t *testing.T, root string, sys hostOps) *Driver {
	t.Helper()
	data := filepath.Join(root, "data")
	d := newDriver(Options{
		DataDir:          data,
		Sandboxes:        filepath.Join(data, "sandboxes"),
		ScratchTemplates: filepath.Join(data, "scratch-templates"),
		JailDir:          filepath.Join(data, "jail"),
		ServicePort:      8486,
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}, idRange{uid: os.Getuid(), gid: os.Getgid(), count: 16}, sys)
	d.cgroupRoot = filepath.Join(root, "cgroup")
	t.Cleanup(func() { d.Close() })
	return d
}

// recovered is a driver after a restart of runplane.
func recovered(t *testing.T, root string, sys hostOps) *Driver {
	t.Helper()
	d := testDriver(t, root, sys)
	if err := d.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

// writeVM leaves the files of a VM as runplane does before a restart: the
// record, the chroot, and a scope with a process.
func writeVM(t *testing.T, root, id string, tap int, state sandbox.State, created time.Time) {
	t.Helper()
	writeRecord(t, root, id, "vm", tap, state, created)
	writeFile(t, filepath.Join(root, "data", "jail", "firecracker", id, "root", scratchFile), "")
	writeFile(t, scopeProcs(root, id), "4242\n")
}

// writeRecord writes the record of a sandbox in the form of the lifecycle
// package.
func writeRecord(t *testing.T, root, id string, mode sandbox.Mode, tap int, state sandbox.State, created time.Time) {
	t.Helper()
	lease := leaseOf(tap)
	b, err := json.Marshal(map[string]any{
		"mode":       mode,
		"spec":       sandbox.Spec{ID: id, PoolID: "pool1", VCPU: 1, MemoryMB: 512, DiskGB: 1, TimeLimit: time.Hour},
		"created_at": created,
		"ip":         lease.Guest.String(),
		"state":      state,
		"host":       map[string]any{"lease": lease},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "data", "sandboxes", id, "sandbox.json"), string(b))
}

func scopeProcs(root, id string) string {
	return filepath.Join(root, "cgroup", "runplane.slice", "runplane-vm-"+id+".scope", "cgroup.procs")
}

// dialGuest connects to a control socket as runplane-init does through
// vsock. It calls onShutdown when the host asks the guest to power off.
func dialGuest(t *testing.T, socket string, onShutdown func()) *guest.Client {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+guest.PathShutdown, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		onShutdown()
	})
	c, err := guest.NewClient(conn, mux, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// connected waits until the host serves the guest of c: the host answers
// an empty log stream.
func connected(t *testing.T, c *guest.Client) {
	t.Helper()
	if err := c.StreamLogs(context.Background(), strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
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
		c := dialGuest(t, filepath.Join(d.chroot(id), controlSocket), func() {
			shutdowns <- id
			writeFile(t, scopeProcs(root, id), "") // the guest powers off
		})
		connected(t, c)
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
		if exists(filepath.Join(root, "data", "sandboxes", id)) != want || exists(d.jailDir(id)) != want {
			t.Errorf("the folders of %s exist: %v, want %v", id, !want, want)
		}
	}
}

// netrules keeps the old rules when a firewall write fails. The fleet sends
// the same settings again at the next connection, and that Configure must
// write the firewall again: else the VMs keep the old allowlist and DNS
// servers until these settings change again.
func TestConfigureWritesTheFirewallAgain(t *testing.T) {
	ctx := t.Context()
	sys := &fakeHost{netErr: errors.New("nft: netlink busy")}
	d := testDriver(t, t.TempDir(), sys)
	dns := []netip.Addr{netip.MustParseAddr("9.9.9.9")}
	s := sandbox.Settings{DNS: dns, EgressAllowlist: []string{"192.0.2.1"}}
	if err := d.Configure(ctx, s); err == nil {
		t.Fatal("Configure gave no error after a failed firewall write")
	}
	sys.mu.Lock()
	sys.netErr = nil
	sys.mu.Unlock()
	if err := d.Configure(ctx, s); err != nil {
		t.Fatal(err)
	}
	want := netrules.Config{ServicePort: 8486, DNS: dns, Allowlist: []string{"192.0.2.1"}}
	sys.mu.Lock()
	defer sys.mu.Unlock()
	if len(sys.nets) != 2 || !reflect.DeepEqual(sys.nets[1], want) {
		t.Errorf("firewall writes %+v, want two writes of %+v", sys.nets, want)
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
