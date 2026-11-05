package firecracker

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

func TestRecoverAfterRestart(t *testing.T) {
	root := shortTempDir(t)
	data := filepath.Join(root, "data")
	// A VM that still operates.
	writeVM(t, root, "live", 3, sandbox.StateBusy, time.Now().UTC())
	// A VM that stopped while runplane was down.
	writeVM(t, root, "dead", 5, sandbox.StateIdle, time.Now().UTC())
	writeFile(t, scopeProcs(root, "dead"), "")
	// A start that runplane did not complete: a scope and a chroot without a record.
	writeFile(t, scopeProcs(root, "lost"), "999\n")
	writeFile(t, filepath.Join(data, "jail", "firecracker", "lost", "root", "rootfs.ext4"), "")
	// A sandbox folder of another driver.
	writeFile(t, filepath.Join(data, "sandboxes", "other", "console.log"), "")

	sys := &fakeHost{}
	d := recovered(t, root, sys)
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
	c := dialGuest(t, filepath.Join(data, "jail", "firecracker", "live", "root", "v.sock_1024"), func() {})
	if err := c.PostEvent(context.Background(), sandbox.RunnerEvent{Kind: sandbox.RunnerExited}); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, d); ev.Type != sandbox.EventRunner || ev.Runner.Kind != sandbox.RunnerExited {
		t.Fatalf("got %+v, want the exited runner event", ev)
	}

	// The VM powers off: its scope empties.
	writeFile(t, scopeProcs(root, "live"), "")
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
	root := shortTempDir(t)
	// The warm VM waited 30 minutes for its job.
	writeVM(t, root, "vm1", 0, sandbox.StateIdle, time.Now().UTC().Add(-30*time.Minute))
	before := recovered(t, root, &fakeHost{})
	v := before.vms["vm1"]
	v.OnEvent(sandbox.RunnerEvent{Kind: sandbox.RunnerJobStarted, JobName: "build"})
	v.OnEvent(sandbox.RunnerEvent{Kind: sandbox.RunnerExited})
	before.Close()

	after := recovered(t, root, &fakeHost{})
	// The job has its full hour, not the 30 minutes that remain from the VM start.
	if end, min := after.vms["vm1"].deadline(), time.Now().Add(time.Hour); end.Before(min) {
		t.Errorf("deadline after the restart %v, want after %v", end, min)
	}
	writeFile(t, scopeProcs(root, "vm1"), "")
	if ev := nextEvent(t, after); ev.State != sandbox.StateGone || ev.Reason != "the runner exited with code 0" {
		t.Errorf("got %s %q, want gone after the runner exit", ev.State, ev.Reason)
	}
}

// A runner that exits before it connects to GitHub fails its VM, also with
// exit code 0 and after a restart of runplane before the VM powers off.
func TestRunnerErrorFailsTheVM(t *testing.T) {
	root := shortTempDir(t)
	writeVM(t, root, "vm1", 0, sandbox.StateStarting, time.Now().UTC())
	before := recovered(t, root, &fakeHost{})
	before.vms["vm1"].OnEvent(sandbox.RunnerEvent{Kind: sandbox.RunnerExited, Error: "the runner did not connect to GitHub"})
	before.Close()

	after := recovered(t, root, &fakeHost{})
	writeFile(t, scopeProcs(root, "vm1"), "")
	if ev := nextEvent(t, after); ev.State != sandbox.StateFailed || ev.Reason != "the runner did not connect to GitHub" {
		t.Errorf("got %s %q, want failed because the runner did not connect", ev.State, ev.Reason)
	}
}

// The JIT config is only in memory. A VM whose guest asks for its config
// after a restart can never run a job, so the driver stops it, and the VM
// ends as gone, not as failed.
func TestLostConfigStopsTheVM(t *testing.T) {
	root := shortTempDir(t)
	writeVM(t, root, "vm1", 0, sandbox.StateStarting, time.Now().UTC())
	d := recovered(t, root, &fakeHost{})

	c := dialGuest(t, filepath.Join(d.chroot("vm1"), controlSocket), func() {})
	if _, err := c.Config(context.Background()); !errors.Is(err, guest.ErrNoConfig) {
		t.Errorf("config after the restart: %v, want ErrNoConfig", err)
	}
	if ev := nextEvent(t, d); ev.State != sandbox.StateStopping {
		t.Fatalf("got %+v, want the stop of the VM", ev)
	}
	writeFile(t, scopeProcs(root, "vm1"), "") // the VM powers off
	if ev := nextEvent(t, d); ev.State != sandbox.StateGone || ev.Reason != "runplane restarted before the VM got its config" {
		t.Errorf("got %s %q, want gone because runplane restarted before the VM got its config", ev.State, ev.Reason)
	}
}
