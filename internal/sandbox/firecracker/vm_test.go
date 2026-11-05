package firecracker

import (
	"context"
	"io"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
)

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
		v.JobStarted, v.HoldUntil = tt.jobStarted, tt.hold
		if got := v.deadline(); !got.Equal(tt.want) {
			t.Errorf("%s: deadline %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A VM that dies before runplane-init connects fails with the end of its
// console as the reason, and its console is in the sandbox log.
func TestConsoleOfAFailedVM(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Go polls FIFOs on Linux only")
	}
	sys := &fakeHost{}
	d := testDriver(t, shortTempDir(t), sys)
	v := newVM(d, sandbox.Spec{ID: "vm1", VCPU: 1}, d.taps.Lease(7), time.Now())
	dir := d.sandboxDir(v.id)
	writeFile(t, filepath.Join(dir, recordFile), "{}")
	var err error
	if v.log, err = control.OpenLogFile(dir); err != nil {
		t.Fatal(err)
	}
	r, w, err := makeConsole(filepath.Join(dir, consoleFIFO))
	if err != nil {
		t.Fatal(err)
	}
	v.console = r
	close(v.ready)
	d.vms[v.id] = v
	go v.tailConsole(r)

	logs := v.log.Reader(context.Background(), true)
	defer logs.Close()
	w.WriteString("Linux version 6.1.0\r\nKernel panic - not syncing: VFS: Unable to mount root fs\r\n---[ end Kernel panic")
	w.Close() // Firecracker exits
	v.markExited()
	d.spawn(func() { d.watch(v) })

	ev := nextEvent(t, d)
	wantReason := "the VM stopped before runplane-init connected: Linux version 6.1.0 | Kernel panic - not syncing: VFS: Unable to mount root fs | ---[ end Kernel panic"
	if ev.State != sandbox.StateFailed || ev.Reason != wantReason {
		t.Errorf("got %s %q,\nwant failed %q", ev.State, ev.Reason, wantReason)
	}
	b, _ := io.ReadAll(logs)
	var texts []string
	for line := range strings.Lines(string(b)) {
		// "<time> console <text>"
		f := strings.SplitN(strings.TrimSuffix(line, "\n"), " ", 3)
		if len(f) != 3 || f[1] != "console" {
			t.Fatalf("log line %q has no console source", line)
		}
		texts = append(texts, f[2])
	}
	if want := []string{"Linux version 6.1.0", "Kernel panic - not syncing: VFS: Unable to mount root fs", "---[ end Kernel panic"}; !slices.Equal(texts, want) {
		t.Errorf("log %q, want %q", texts, want)
	}
	if exists(dir) {
		t.Error("the sandbox folder remains")
	}
	if got := sys.calls(&sys.dropped); !slices.Equal(got, []string{"rp-7"}) {
		t.Errorf("TAPs removed: %v, want rp-7", got)
	}
}
