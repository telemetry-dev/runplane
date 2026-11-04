package firecracker

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/runplane/runplane/internal/sandbox"
)

// At a host shutdown, systemd stops the VM scopes after runplane. Close
// asks those VMs to power off, so that their runners cancel the jobs, and
// leaves the other VMs running for the next start of runplane.
func TestCloseStopsTheVMsThatSystemdStops(t *testing.T) {
	defer func(old time.Duration) { scopePoll = old }(scopePoll)
	scopePoll = 10 * time.Millisecond

	root := shortTempDir(t)
	data := filepath.Join(root, "data")
	scope := func(id string) string {
		return filepath.Join(root, "cgroup", "runplane.slice", "runplane-vm-"+id+".scope", "cgroup.procs")
	}
	ids := []string{"vm1", "vm2"}
	for i, id := range ids {
		spec := sandbox.Spec{ID: id, VCPU: 1, MemoryMB: 512, DiskGB: 1, TimeLimit: time.Hour}
		b, _ := json.Marshal(record{Spec: spec, Tap: i, CreatedAt: time.Now().UTC(), State: sandbox.StateBusy})
		writeFile(t, filepath.Join(data, "sandboxes", id, recordFile), string(b))
		writeFile(t, filepath.Join(data, "jail", "firecracker", id, "root", scratchFile), "")
		writeFile(t, scope(id), "4242\n")
	}
	sys := &fakeHost{stopping: map[string]bool{"runplane-vm-vm1.scope": true, "runplane.service": true}}
	d := testDriver(t, root, sys)
	if err := d.recover(); err != nil {
		t.Fatal(err)
	}
	shutdowns := make(chan string, len(ids))
	for _, id := range ids {
		fakeGuest(t, filepath.Join(d.chroot(id), controlSocket), func() {
			shutdowns <- id
			writeFile(t, scope(id), "") // the guest powers off
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

// fakeGuest connects to a control socket as runplane-init does, and calls
// onShutdown when the host asks it to power off.
func fakeGuest(t *testing.T, socket string, onShutdown func()) {
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

func TestScopeIsOrderedBeforeTheService(t *testing.T) {
	tests := []struct {
		cgroup, before string
	}{
		{"0::/system.slice/runplane.service\n", "--property=Before=runplane.service"},
		{"0::/system.slice/runplane-agent.service\n", "--property=Before=runplane-agent.service"},
		// A test or a shell: no service to order the scopes against.
		{"0::/user.slice/user-1000.slice/session-3.scope\n", ""},
	}
	d := testDriver(t, t.TempDir(), &fakeHost{})
	for _, tt := range tests {
		d.unit = serviceUnit([]byte(tt.cgroup))
		args := d.jailerCommand("vm1", 1879048195, 1879048195).Args
		var before []string
		for _, a := range args[:slices.Index(args, "--")] {
			if strings.HasPrefix(a, "--property=Before=") {
				before = append(before, a)
			}
		}
		if strings.Join(before, " ") != tt.before {
			t.Errorf("%q: systemd-run orders the scope with %q, want %q", tt.cgroup, before, tt.before)
		}
	}
}
