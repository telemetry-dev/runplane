package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/runplane/runplane/internal/sandbox"
)

// guestClient dials a control socket as runplane-init does through vsock.
func guestClient(t *testing.T, socket string) *http.Client {
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
	return &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return sess.Open() },
	}}
}

func TestRecoverAfterRestart(t *testing.T) {
	defer func(old time.Duration) { scopePoll = old }(scopePoll)
	scopePoll = 10 * time.Millisecond

	root := shortTempDir(t)
	data := filepath.Join(root, "data")
	scope := func(id string) string {
		return filepath.Join(root, "cgroup", "runplane.slice", "runplane-vm-"+id+".scope", "cgroup.procs")
	}
	record := func(id string, tap int, state sandbox.State) {
		spec := sandbox.Spec{ID: id, PoolID: "pool1", VCPU: 2, MemoryMB: 2048, DiskGB: 10, TimeLimit: time.Hour}
		b, _ := json.Marshal(record{Spec: spec, Tap: tap, CreatedAt: time.Now().UTC(), State: state})
		writeFile(t, filepath.Join(data, "sandboxes", id, "vm.json"), string(b))
		writeFile(t, filepath.Join(data, "jail", "firecracker", id, "root", "scratch.ext4"), "")
	}
	// A VM that still operates.
	record("live", 3, sandbox.StateBusy)
	writeFile(t, scope("live"), "4242\n")
	// A VM that stopped while runplane was down.
	record("dead", 5, sandbox.StateIdle)
	// A start that runplane did not complete: a scope and a chroot without a record.
	writeFile(t, scope("lost"), "999\n")
	writeFile(t, filepath.Join(data, "jail", "firecracker", "lost", "root", "rootfs.ext4"), "")
	// A sandbox folder of another driver.
	writeFile(t, filepath.Join(data, "sandboxes", "other", "console.log"), "")

	sys := &fakeHost{}
	d := testDriver(t, root, sys)
	if err := d.recover(); err != nil {
		t.Fatal(err)
	}

	list, _ := d.List(context.Background())
	if len(list) != 1 {
		t.Fatalf("listed %d sandboxes, want only the live one: %+v", len(list), list)
	}
	if s := list[0]; s.ID != "live" || s.State != sandbox.StateBusy || s.IP != "10.214.0.14" || s.PoolID != "pool1" {
		t.Errorf("listed %+v", s)
	}
	if got := sys.calls(&sys.kept); !slices.Equal(got, []string{"rp-3"}) {
		t.Errorf("TAP rules added again for %v, want rp-3", got)
	}
	if got := sys.calls(&sys.dropped); !slices.Equal(got, []string{"rp-5"}) {
		t.Errorf("TAPs removed: %v, want rp-5", got)
	}
	if got := sys.calls(&sys.killed); !slices.Equal(got, []string{"runplane-vm-lost"}) {
		t.Errorf("scopes killed: %v, want runplane-vm-lost", got)
	}
	for path, want := range map[string]bool{
		filepath.Join(data, "sandboxes", "live"):           true,
		filepath.Join(data, "jail", "firecracker", "live"): true,
		filepath.Join(data, "sandboxes", "other"):          true,
		filepath.Join(data, "sandboxes", "dead"):           false,
		filepath.Join(data, "jail", "firecracker", "dead"): false,
		filepath.Join(data, "jail", "firecracker", "lost"): false,
	} {
		if exists(path) != want {
			t.Errorf("%s exists: %v, want %v", path, !want, want)
		}
	}

	// The guest of the live VM, which has its config, connects again on the
	// same socket.
	guest := guestClient(t, filepath.Join(data, "jail", "firecracker", "live", "root", "v.sock_1024"))
	resp, err := guest.Post("http://host/v1/events", "application/json", bytes.NewReader([]byte(`{"kind":"exited","exit_code":0}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ev := nextEvent(t, d); ev.Type != sandbox.EventRunner || ev.Runner.Kind != sandbox.RunnerExited {
		t.Fatalf("got %+v, want the exited runner event", ev)
	}

	// The VM powers off: its scope empties.
	writeFile(t, scope("live"), "")
	ev := nextEvent(t, d)
	if ev.SandboxID != "live" || ev.State != sandbox.StateGone || ev.Reason != "the runner exited with code 0" {
		t.Errorf("got %+v, want gone after the runner exit", ev)
	}
	if exists(filepath.Join(data, "sandboxes", "live")) || exists(filepath.Join(data, "jail", "firecracker", "live")) {
		t.Error("the folders of the stopped VM remain")
	}
	if got := sys.calls(&sys.dropped); !slices.Equal(got, []string{"rp-5", "rp-3"}) {
		t.Errorf("TAPs removed: %v, want rp-5 then rp-3", got)
	}
	if list, _ := d.List(context.Background()); len(list) != 0 {
		t.Errorf("still listed: %+v", list)
	}
}

// A restart keeps what the VM reported: the job start, from which the time
// limit counts, and the runner exit. Then a VM that powers off later, for
// example at the end of a debug hold, ends as gone and not as failed.
func TestRestartKeepsTheJobStartAndTheRunnerExit(t *testing.T) {
	defer func(old time.Duration) { scopePoll = old }(scopePoll)
	scopePoll = 10 * time.Millisecond

	root := shortTempDir(t)
	data := filepath.Join(root, "data")
	scope := filepath.Join(root, "cgroup", "runplane.slice", "runplane-vm-vm1.scope", "cgroup.procs")
	writeFile(t, scope, "4242\n")
	spec := sandbox.Spec{ID: "vm1", PoolID: "pool1", VCPU: 1, MemoryMB: 512, DiskGB: 1, TimeLimit: time.Hour}
	// The warm VM waited 30 minutes for its job.
	b, _ := json.Marshal(record{Spec: spec, CreatedAt: time.Now().UTC().Add(-30 * time.Minute), State: sandbox.StateIdle})
	writeFile(t, filepath.Join(data, "sandboxes", "vm1", recordFile), string(b))
	writeFile(t, filepath.Join(data, "jail", "firecracker", "vm1", "root", scratchFile), "")

	before := testDriver(t, root, &fakeHost{})
	if err := before.recover(); err != nil {
		t.Fatal(err)
	}
	v := before.vms["vm1"]
	v.OnEvent(sandbox.RunnerEvent{Kind: sandbox.RunnerJobStarted, JobName: "build"})
	v.OnEvent(sandbox.RunnerEvent{Kind: sandbox.RunnerExited})
	before.Close()

	after := testDriver(t, root, &fakeHost{})
	if err := after.recover(); err != nil {
		t.Fatal(err)
	}
	// The job has its full hour, not the 30 minutes that remain from the VM start.
	if end, min := after.vms["vm1"].deadline(), time.Now().Add(time.Hour); end.Before(min) {
		t.Errorf("deadline after the restart %v, want after %v", end, min)
	}
	writeFile(t, scope, "")
	if ev := nextEvent(t, after); ev.State != sandbox.StateGone || ev.Reason != "the runner exited with code 0" {
		t.Errorf("got %s %q, want gone after the runner exit", ev.State, ev.Reason)
	}
}

// A runner that exits before it connects to GitHub fails its VM, also with
// exit code 0 and after a restart of runplane before the VM powers off.
func TestRunnerErrorFailsTheVM(t *testing.T) {
	defer func(old time.Duration) { scopePoll = old }(scopePoll)
	scopePoll = 10 * time.Millisecond

	root := shortTempDir(t)
	data := filepath.Join(root, "data")
	scope := filepath.Join(root, "cgroup", "runplane.slice", "runplane-vm-vm1.scope", "cgroup.procs")
	writeFile(t, scope, "4242\n")
	spec := sandbox.Spec{ID: "vm1", PoolID: "pool1", VCPU: 1, MemoryMB: 512, DiskGB: 1, TimeLimit: time.Hour}
	b, _ := json.Marshal(record{Spec: spec, CreatedAt: time.Now().UTC(), State: sandbox.StateStarting})
	writeFile(t, filepath.Join(data, "sandboxes", "vm1", recordFile), string(b))
	writeFile(t, filepath.Join(data, "jail", "firecracker", "vm1", "root", scratchFile), "")

	before := testDriver(t, root, &fakeHost{})
	if err := before.recover(); err != nil {
		t.Fatal(err)
	}
	before.vms["vm1"].OnEvent(sandbox.RunnerEvent{Kind: sandbox.RunnerExited, Error: "the runner did not connect to GitHub"})
	before.Close()

	after := testDriver(t, root, &fakeHost{})
	if err := after.recover(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, scope, "")
	if ev := nextEvent(t, after); ev.State != sandbox.StateFailed || ev.Reason != "the runner did not connect to GitHub" {
		t.Errorf("got %s %q, want failed because the runner did not connect", ev.State, ev.Reason)
	}
}

// The JIT config is only in memory. A VM whose guest asks for its config
// after a restart can never run a job, so the driver stops it, and the VM
// ends as gone, not as failed.
func TestLostConfigStopsTheVM(t *testing.T) {
	defer func(old time.Duration) { scopePoll = old }(scopePoll)
	scopePoll = 10 * time.Millisecond

	root := shortTempDir(t)
	data := filepath.Join(root, "data")
	scope := filepath.Join(root, "cgroup", "runplane.slice", "runplane-vm-vm1.scope", "cgroup.procs")
	writeFile(t, scope, "4242\n")
	spec := sandbox.Spec{ID: "vm1", PoolID: "pool1", VCPU: 1, MemoryMB: 512, DiskGB: 1, TimeLimit: time.Hour}
	b, _ := json.Marshal(record{Spec: spec, CreatedAt: time.Now().UTC(), State: sandbox.StateStarting})
	writeFile(t, filepath.Join(data, "sandboxes", "vm1", recordFile), string(b))
	writeFile(t, filepath.Join(data, "jail", "firecracker", "vm1", "root", scratchFile), "")
	d := testDriver(t, root, &fakeHost{})
	if err := d.recover(); err != nil {
		t.Fatal(err)
	}

	guest := guestClient(t, filepath.Join(data, "jail", "firecracker", "vm1", "root", "v.sock_1024"))
	resp, err := guest.Get("http://host/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("config after the restart: status %d, want 409", resp.StatusCode)
	}
	if ev := nextEvent(t, d); ev.State != sandbox.StateStopping {
		t.Fatalf("got %+v, want the stop of the VM", ev)
	}
	writeFile(t, scope, "") // the VM powers off
	if ev := nextEvent(t, d); ev.State != sandbox.StateGone || ev.Reason != "runplane restarted before the VM got its config" {
		t.Errorf("got %s %q, want gone because runplane restarted before the VM got its config", ev.State, ev.Reason)
	}
}
