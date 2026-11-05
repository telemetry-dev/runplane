package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/atomicfile"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
	"github.com/runplane/runplane/internal/sandbox/guest"
	"github.com/runplane/runplane/internal/sandbox/netrules"
)

// timeLimitGrace lets runplane-init stop the runner at the time limit and
// report it before the driver stops the VM.
const timeLimitGrace = time.Minute

// keepLines is the count of console lines that a failure reason shows.
const keepLines = 10

// consoleFIFO in the sandbox folder takes the stdout and stderr of
// Firecracker: the serial console of the guest and the Firecracker log. A
// FIFO, unlike a file, does not grow when a job writes to the console
// without end, and Firecracker only gets write errors while runplane
// restarts.
const consoleFIFO = "console.fifo"

// vm is one microVM. It is the control.Handler of its control channel.
type vm struct {
	d     *Driver
	id    string
	lease netrules.Lease

	log      *control.LogFile
	ctl      *control.Server
	api      *apiClient
	console  *os.File      // read end of the console FIFO
	tailDone chan struct{} // closed when the console tail ends

	ready    chan struct{} // closed when Start ends, also on failure
	exited   chan struct{} // closed when the Firecracker process is gone
	exitOnce sync.Once
	done     chan struct{} // closed after the driver removed the VM

	heard atomic.Bool // the guest has connected

	mu         sync.Mutex
	record            // Spec and CreatedAt do not change, so they need no lock
	stopReason string // set when the driver stops the VM
	killed     bool   // the driver killed the VM; it did not power off
	timer      *time.Timer
	lastLines  []string // the end of the console, for failure reasons
	cpu        cpuSample
}

func newVM(d *Driver, spec sandbox.Spec, lease netrules.Lease, created time.Time) *vm {
	spec.JITConfig = ""
	return &vm{
		d:        d,
		id:       spec.ID,
		lease:    lease,
		record:   record{Spec: spec, Tap: lease.Index, CreatedAt: created, State: sandbox.StateStarting},
		tailDone: make(chan struct{}),
		ready:    make(chan struct{}),
		exited:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (v *vm) OnEvent(ev sandbox.RunnerEvent) {
	v.heard.Store(true)
	v.d.emit(sandbox.Event{SandboxID: v.id, Type: sandbox.EventRunner, Runner: &ev})
	switch ev.Kind {
	case sandbox.RunnerListening:
		v.setState(sandbox.StateIdle, "")
	case sandbox.RunnerJobStarted:
		v.mu.Lock()
		if v.JobStarted.IsZero() {
			v.JobStarted = time.Now().UTC()
		}
		v.mu.Unlock()
		v.setState(sandbox.StateBusy, ev.JobName)
		// The time limit now counts from the job start.
		v.armTimer()
	case sandbox.RunnerExited:
		code := ev.ExitCode
		v.mu.Lock()
		v.RunnerExit, v.RunnerErr = &code, ev.Error
		v.mu.Unlock()
		// A restart during a debug hold must still know that the runner
		// ended by itself.
		v.save()
	}
}

func (v *vm) OnLostConfig() {
	if err := v.d.stop(context.Background(), v, "runplane restarted before the VM got its config"); err != nil {
		v.d.log.Warn("stop sandbox", "sandbox", v.id, "err", err)
	}
}

func (v *vm) OnLog(line guest.LogLine) {
	v.heard.Store(true)
	if err := v.log.Write(line); err != nil {
		v.d.log.Debug("write sandbox log", "sandbox", v.id, "err", err)
	}
}

// setState moves the VM to a running state. A stopping VM stays stopping.
func (v *vm) setState(s sandbox.State, reason string) {
	v.mu.Lock()
	if v.State == s || v.State == sandbox.StateStopping {
		v.mu.Unlock()
		return
	}
	v.State = s
	v.mu.Unlock()
	v.save()
	v.d.emit(sandbox.Event{SandboxID: v.id, Type: sandbox.EventState, State: s, Reason: reason})
}

func (v *vm) getState() sandbox.State {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.State
}

func (v *vm) sandbox() sandbox.Sandbox {
	return sandbox.Sandbox{
		ID:        v.id,
		PoolID:    v.Spec.PoolID,
		State:     v.getState(),
		IP:        v.lease.Guest.String(),
		CreatedAt: v.CreatedAt,
		Spec:      v.Spec,
	}
}

func (v *vm) isReady() bool {
	select {
	case <-v.ready:
		return !isClosed(v.done)
	default:
		return false
	}
}

func (v *vm) markExited() { v.exitOnce.Do(func() { close(v.exited) }) }

// hold keeps the VM until the end of a debug hold.
func (v *vm) hold(until time.Time) {
	v.mu.Lock()
	v.HoldUntil = until
	v.mu.Unlock()
	v.save()
	v.armTimer()
}

// deadline is when the driver stops the VM: after the time limit of the
// job or the end of a debug hold, and a grace for runplane-init.
func (v *vm) deadline() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.Spec.TimeLimit <= 0 {
		return time.Time{}
	}
	end := sandbox.Deadline(v.CreatedAt, v.JobStarted, v.Spec.TimeLimit)
	if v.HoldUntil.After(end) {
		end = v.HoldUntil
	}
	return end.Add(timeLimitGrace)
}

// armTimer stops the VM at its deadline.
func (v *vm) armTimer() {
	end := v.deadline()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.timer != nil {
		v.timer.Stop()
	}
	if end.IsZero() {
		return
	}
	v.timer = time.AfterFunc(time.Until(end), func() {
		v.d.log.Info("sandbox reached its time limit", "sandbox", v.id)
		if err := v.d.stop(context.Background(), v, "time limit reached"); err != nil {
			v.d.log.Warn("stop sandbox", "sandbox", v.id, "err", err)
		}
	})
}

// outcome is the last state of the VM and the reason for it.
func (v *vm) outcome() (sandbox.State, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	switch {
	case v.RunnerErr != "":
		return sandbox.StateFailed, v.RunnerErr
	case v.stopReason != "":
		return sandbox.StateGone, v.stopReason
	case !v.heard.Load():
		return sandbox.StateFailed, "the VM stopped before runplane-init connected" + v.consoleEnd()
	case v.RunnerExit == nil:
		return sandbox.StateFailed, "the VM stopped unexpectedly" + v.consoleEnd()
	default:
		return sandbox.StateGone, fmt.Sprintf("the runner exited with code %d", *v.RunnerExit)
	}
}

// consoleEnd returns the last console lines for an error message. The
// caller holds v.mu.
func (v *vm) consoleEnd() string {
	if len(v.lastLines) == 0 {
		return ""
	}
	return ": " + strings.Join(v.lastLines, " | ")
}

// withConsole adds the end of the console to a start error, because the
// sandbox folder is gone afterwards.
func (v *vm) withConsole(err error) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return fmt.Errorf("%w%s", err, v.consoleEnd())
}

// tailConsole copies the console lines to the sandbox log until Firecracker
// closes its end.
func (v *vm) tailConsole(r *os.File) {
	defer close(v.tailDone)
	guest.ReadLines(r, func(line string) {
		v.mu.Lock()
		if len(v.lastLines) == keepLines {
			v.lastLines = v.lastLines[1:]
		}
		v.lastLines = append(v.lastLines, line)
		v.mu.Unlock()
		v.log.Write(guest.LogLine{Time: time.Now().UTC(), Source: guest.SourceConsole, Text: line})
	})
}

// detach drops the connections of the VM and leaves it running.
func (v *vm) detach() {
	v.mu.Lock()
	if v.timer != nil {
		v.timer.Stop()
	}
	v.mu.Unlock()
	if v.ctl != nil {
		v.ctl.Close()
	}
	if v.console != nil {
		v.console.Close()
	}
	if v.log != nil {
		v.log.Close()
	}
}

// makeConsole makes the console FIFO. It returns the read end and the write
// end for Firecracker.
func makeConsole(path string) (r, w *os.File, err error) {
	os.Remove(path)
	if err := unix.Mkfifo(path, 0o600); err != nil {
		return nil, nil, fmt.Errorf("make console FIFO: %w", err)
	}
	// The read end opens first, so the write end does not wait.
	if r, err = openConsole(path); err != nil {
		return nil, nil, err
	}
	if w, err = os.OpenFile(path, os.O_WRONLY, 0); err != nil {
		r.Close()
		return nil, nil, fmt.Errorf("open console FIFO: %w", err)
	}
	return r, w, nil
}

// openConsole opens the read end of the console FIFO, also again after a
// restart.
func openConsole(path string) (*os.File, error) {
	r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open console FIFO: %w", err)
	}
	return r, nil
}

const recordFile = "vm.json"

// record is the part of a VM that the driver needs after a restart. It is
// in the sandbox folder.
type record struct {
	Spec       sandbox.Spec  `json:"spec"` // without the JIT config
	Tap        int           `json:"tap"`  // index of the TAP lease, also of the jailer IDs
	CreatedAt  time.Time     `json:"created_at"`
	State      sandbox.State `json:"state"`
	JobStarted time.Time     `json:"job_started,omitzero"` // zero before the job
	HoldUntil  time.Time     `json:"hold_until,omitzero"`
	RunnerExit *int          `json:"runner_exit,omitempty"`  // from the exited event
	RunnerErr  string        `json:"runner_error,omitempty"` // from the exited event: why the runner failed
}

// save writes the record of the VM, unless the VM is gone.
func (v *vm) save() {
	if isClosed(v.done) {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := writeRecord(v.d.sandboxDir(v.id), v.record); err != nil {
		v.d.log.Warn("save sandbox record", "sandbox", v.id, "err", err)
	}
}

func writeRecord(dir string, rec record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, recordFile), bytes.NewReader(b), 0o600)
}

func readRecord(dir string) (record, error) {
	var rec record
	b, err := os.ReadFile(filepath.Join(dir, recordFile))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return rec, fmt.Errorf("read %s: %w", filepath.Join(dir, recordFile), err)
	}
	return rec, nil
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
