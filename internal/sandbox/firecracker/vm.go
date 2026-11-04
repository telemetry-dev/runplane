package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
	"github.com/runplane/runplane/internal/sandbox/netrules"
)

// timeLimitGrace lets runplane-init stop the runner at the time limit and
// report it before the driver stops the VM.
const timeLimitGrace = time.Minute

// vm is one microVM. It is the control.Handler of its control channel.
type vm struct {
	d       *Driver
	id      string
	spec    sandbox.Spec // without the JIT config
	lease   netrules.Lease
	created time.Time

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
	state      sandbox.State
	jobStarted time.Time // zero before the job
	holdUntil  time.Time
	runnerExit *int   // from the exited event
	runnerErr  string // from the exited event: why the runner failed
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
		spec:     spec,
		lease:    lease,
		created:  created,
		tailDone: make(chan struct{}),
		ready:    make(chan struct{}),
		exited:   make(chan struct{}),
		done:     make(chan struct{}),
		state:    sandbox.StateStarting,
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
		if v.jobStarted.IsZero() {
			v.jobStarted = time.Now().UTC()
		}
		v.mu.Unlock()
		v.setState(sandbox.StateBusy, ev.JobName)
		// The time limit now counts from the job start.
		v.armTimer()
	case sandbox.RunnerExited:
		code := ev.ExitCode
		v.mu.Lock()
		v.runnerExit, v.runnerErr = &code, ev.Error
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

func (v *vm) OnLog(line control.LogLine) {
	v.heard.Store(true)
	if err := v.log.Write(line); err != nil {
		v.d.log.Debug("write sandbox log", "sandbox", v.id, "err", err)
	}
}

// setState moves the VM to a running state. A stopping VM stays stopping.
func (v *vm) setState(s sandbox.State, reason string) {
	v.mu.Lock()
	if v.state == s || v.state == sandbox.StateStopping {
		v.mu.Unlock()
		return
	}
	v.state = s
	v.mu.Unlock()
	v.save()
	v.d.emit(sandbox.Event{SandboxID: v.id, Type: sandbox.EventState, State: s, Reason: reason})
}

func (v *vm) getState() sandbox.State {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state
}

func (v *vm) sandbox() sandbox.Sandbox {
	return sandbox.Sandbox{
		ID:        v.id,
		PoolID:    v.spec.PoolID,
		State:     v.getState(),
		IP:        v.lease.Guest.String(),
		CreatedAt: v.created,
		Spec:      v.spec,
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
	v.holdUntil = until
	v.mu.Unlock()
	v.save()
	v.armTimer()
}

// deadline is when the driver stops the VM: after the time limit of the
// job or the end of a debug hold, and a grace for runplane-init.
func (v *vm) deadline() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.spec.TimeLimit <= 0 {
		return time.Time{}
	}
	end := sandbox.Deadline(v.created, v.jobStarted, v.spec.TimeLimit)
	if v.holdUntil.After(end) {
		end = v.holdUntil
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
	case v.runnerErr != "":
		return sandbox.StateFailed, v.runnerErr
	case v.stopReason != "":
		return sandbox.StateGone, v.stopReason
	case !v.heard.Load():
		return sandbox.StateFailed, "the VM stopped before runplane-init connected" + v.consoleEnd()
	case v.runnerExit == nil:
		return sandbox.StateFailed, "the VM stopped unexpectedly" + v.consoleEnd()
	default:
		return sandbox.StateGone, fmt.Sprintf("the runner exited with code %d", *v.runnerExit)
	}
}

const keepLines = 10

func (v *vm) keepLine(line string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.lastLines) == keepLines {
		v.lastLines = v.lastLines[1:]
	}
	v.lastLines = append(v.lastLines, line)
}

// consoleEnd returns the last console lines for an error message. The
// caller holds v.mu.
func (v *vm) consoleEnd() string {
	if len(v.lastLines) == 0 {
		return ""
	}
	return ": " + strings.Join(v.lastLines, " | ")
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

const recordFile = "vm.json"

// record is the part of a VM that the driver needs after a restart. It is
// in the sandbox folder.
type record struct {
	Spec       sandbox.Spec  `json:"spec"`
	Tap        int           `json:"tap"` // index of the TAP lease, also of the jailer IDs
	CreatedAt  time.Time     `json:"created_at"`
	State      sandbox.State `json:"state"`
	JobStarted time.Time     `json:"job_started,omitzero"`
	HoldUntil  time.Time     `json:"hold_until,omitzero"`
	RunnerExit *int          `json:"runner_exit,omitempty"`
	RunnerErr  string        `json:"runner_error,omitempty"`
}

// save writes the record of the VM, unless the VM is gone.
func (v *vm) save() {
	if isClosed(v.done) {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	rec := record{
		Spec:       v.spec,
		Tap:        v.lease.Index,
		CreatedAt:  v.created,
		State:      v.state,
		JobStarted: v.jobStarted,
		HoldUntil:  v.holdUntil,
		RunnerExit: v.runnerExit,
		RunnerErr:  v.runnerErr,
	}
	if err := writeRecord(v.d.sandboxDir(v.id), rec); err != nil {
		v.d.log.Warn("save sandbox record", "sandbox", v.id, "err", err)
	}
}

func writeRecord(dir string, rec record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, recordFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, recordFile))
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
