package lifecycle

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/atomicfile"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// keepLines is the count of console lines that a failure reason shows.
const keepLines = 10

// Box is one sandbox of a driver. It is the control.Handler of the control
// channel of the sandbox.
type Box[T any] struct {
	ID   string
	Dir  string // the sandbox folder
	Host *T

	c       *Core[T]
	log     *slog.Logger
	spec    sandbox.Spec // without the JIT config
	created time.Time

	started  chan struct{} // closed when Start ends, also after an error
	exited   chan struct{} // closed by MarkExited
	exitOnce sync.Once
	removed  chan struct{} // closed when the sandbox is gone

	mu         sync.Mutex
	logs       *control.LogFile
	ctl        *control.Server
	ip         string
	state      sandbox.State
	jobStarted time.Time
	holdUntil  time.Time
	runnerExit *int
	runnerErr  string
	stopReason string // set when runplane stops the sandbox
	failure    string // set when runplane finds a fault
	heard      bool   // runplane-init has connected
	live       bool   // Start returned, so the events go out
	pending    []sandbox.Event
	attempt    *attempt
	killed     bool
	timer      *time.Timer
	lastLines  []string // the end of the console, for failure reasons
	cpu        Usage
}

// attempt is one run of a stop.
type attempt struct {
	done chan struct{}
	err  error
}

func (c *Core[T]) newBox(id string, spec sandbox.Spec, created time.Time) *Box[T] {
	spec.JITConfig = ""
	return &Box[T]{
		ID: id, Dir: filepath.Join(c.dir, id), Host: new(T),
		c: c, log: c.log.With("sandbox", id), spec: spec, created: created,
		started: make(chan struct{}), exited: make(chan struct{}), removed: make(chan struct{}),
		state: sandbox.StateStarting,
	}
}

func (c *Core[T]) fromRecord(id string, rec record[T]) *Box[T] {
	b := c.newBox(id, rec.Spec, rec.CreatedAt)
	if rec.Host != nil {
		b.Host = rec.Host
	}
	b.ip, b.state = rec.IP, rec.State
	b.jobStarted, b.holdUntil = rec.JobStarted, rec.HoldUntil
	b.runnerExit, b.runnerErr = rec.RunnerExit, rec.RunnerErr
	b.stopReason, b.failure = rec.StopReason, rec.Failure
	b.heard = rec.State != sandbox.StateStarting
	b.live = true
	close(b.started)
	return b
}

// Spec is the spec of the sandbox without the JIT config.
func (b *Box[T]) Spec() sandbox.Spec { return b.spec }

func (b *Box[T]) Log() *slog.Logger { return b.log }

func (b *Box[T]) State() sandbox.State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// SetControl gives the box its control channel. The box closes it.
func (b *Box[T]) SetControl(srv *control.Server) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ctl = srv
}

// SetIP sets the address of the sandbox and saves the record.
func (b *Box[T]) SetIP(ip string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ip = ip
	return b.saveLocked()
}

// Save writes the record of the sandbox.
func (b *Box[T]) Save() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.saveLocked()
}

// MarkExited tells the box that the process of the sandbox has ended. The
// core then removes the sandbox.
func (b *Box[T]) MarkExited() { b.exitOnce.Do(func() { close(b.exited) }) }

// Exited is closed when the process of the sandbox has ended.
func (b *Box[T]) Exited() <-chan struct{} { return b.exited }

// Ready reports if the sandbox has started and is not gone.
func (b *Box[T]) Ready() bool { return isClosed(b.started) && !isClosed(b.removed) }

// Console takes one line of the console of the sandbox: the output that does
// not go through the control channel, such as kernel messages. The line goes
// to the log, and the last lines explain a failure.
func (b *Box[T]) Console(line string) {
	b.mu.Lock()
	if len(b.lastLines) == keepLines {
		b.lastLines = b.lastLines[1:]
	}
	b.lastLines = append(b.lastLines, line)
	logs := b.logs
	b.mu.Unlock()
	if logs != nil {
		logs.Write(guest.LogLine{Time: time.Now().UTC(), Source: guest.SourceConsole, Text: line})
	}
}

// Fail stops the sandbox because runplane found a fault. It ends as failed.
func (b *Box[T]) Fail(reason string) {
	b.log.Warn("stop a sandbox with a fault", "reason", reason)
	b.halt("", reason)
}

// FailNow kills the sandbox because runplane found a fault that a shutdown
// cannot end. It ends as failed.
func (b *Box[T]) FailNow(reason string) {
	b.mu.Lock()
	if b.failure == "" {
		b.failure = reason
	}
	b.setState(sandbox.StateStopping, reason)
	b.saveLocked()
	b.mu.Unlock()
	b.c.Go(func(ctx context.Context) {
		if err := b.c.kill(ctx, b); err != nil {
			b.log.Warn("kill sandbox", "err", err)
		}
	})
}

// Stop stops the sandbox for reason and returns when it is gone. When ctx
// ends first, the stop goes on.
func (b *Box[T]) Stop(ctx context.Context, reason string) error {
	a := b.halt(reason, "")
	select {
	case <-b.removed:
		return nil
	case <-a.done:
		if isClosed(b.removed) {
			return nil
		}
		return a.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// halt starts a stop for reason, or because of failure, unless a stop runs.
// The first reason and the first failure stay.
func (b *Box[T]) halt(reason, failure string) *attempt {
	b.mu.Lock()
	b.stopReason = cmp.Or(b.stopReason, reason)
	b.failure = cmp.Or(b.failure, failure)
	b.setState(sandbox.StateStopping, cmp.Or(failure, reason))
	b.saveLocked()
	a := b.attempt
	if a != nil && !isClosed(a.done) {
		b.mu.Unlock()
		return a
	}
	a = &attempt{done: make(chan struct{})}
	b.attempt = a
	b.mu.Unlock()
	if !b.c.Go(func(ctx context.Context) {
		a.err = b.c.halt(ctx, b)
		close(a.done)
	}) {
		a.err = errClosed
		close(a.done)
	}
	return a
}

func (b *Box[T]) OnConnect() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.heard = true
}

func (b *Box[T]) OnEvent(ev sandbox.RunnerEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.emit(sandbox.Event{Type: sandbox.EventRunner, Runner: &ev})
	st, reason, ok := ev.State()
	if !ok {
		return
	}
	switch ev.Kind {
	case sandbox.RunnerJobStarted:
		if b.jobStarted.IsZero() {
			b.jobStarted = time.Now().UTC()
			// The time limit now counts from the job start.
			b.arm()
		}
	case sandbox.RunnerExited:
		code := ev.ExitCode
		b.runnerExit, b.runnerErr = &code, ev.Error
	}
	b.setState(st, reason)
	// A restart, for example during a debug hold, must still know that
	// the runner ended by itself.
	b.saveLocked()
}

func (b *Box[T]) OnLostConfig() {
	b.log.Warn("runplane restarted before the sandbox got its config; stopping it")
	b.halt("runplane restarted before the sandbox got its config", "")
}

func (b *Box[T]) OnLog(line guest.LogLine) {
	b.mu.Lock()
	logs := b.logs
	b.mu.Unlock()
	if logs == nil {
		return
	}
	if err := logs.Write(line); err != nil {
		b.log.Debug("write sandbox log", "err", err)
	}
}

// stateOrder is the order of the states before the end. A sandbox moves only
// forward in it, so a late event cannot undo a newer one.
var stateOrder = map[sandbox.State]int{
	sandbox.StateStarting: 1, sandbox.StateIdle: 2, sandbox.StateBusy: 3, sandbox.StateStopping: 4,
}

// setState moves the sandbox forward to st and sends the change. The caller
// holds b.mu and saves the record.
func (b *Box[T]) setState(st sandbox.State, reason string) {
	if stateOrder[st] <= stateOrder[b.state] || b.state.Done() {
		return
	}
	b.state = st
	b.emit(sandbox.Event{Type: sandbox.EventState, State: st, Reason: reason})
}

// emit sends an event of the sandbox, or keeps it until Start returns. The
// caller holds b.mu.
func (b *Box[T]) emit(ev sandbox.Event) {
	ev.SandboxID = b.ID
	ev.Time = time.Now().UTC()
	if !b.live {
		b.pending = append(b.pending, ev)
		return
	}
	b.c.events.push(ev)
}

// deadline is when the driver stops the sandbox: after the time limit of the
// job or the end of a debug hold, and limitGrace. The caller holds b.mu.
func (b *Box[T]) deadline() time.Time {
	if b.spec.TimeLimit <= 0 {
		return time.Time{}
	}
	end := sandbox.Deadline(b.created, b.jobStarted, b.spec.TimeLimit)
	if b.holdUntil.After(end) {
		end = b.holdUntil
	}
	return end.Add(limitGrace)
}

// arm sets the timer for the deadline. The caller holds b.mu.
func (b *Box[T]) arm() {
	if b.timer != nil {
		b.timer.Stop()
	}
	end := b.deadline()
	if end.IsZero() {
		return
	}
	b.timer = time.AfterFunc(time.Until(end), func() { b.Fail("the sandbox passed its time limit") })
}

// outcome is the last state of the sandbox and the reason for it. fault is
// what the backend found. The caller holds b.mu.
func (b *Box[T]) outcome(fault string) (sandbox.State, string) {
	switch {
	case b.failure != "":
		return sandbox.StateFailed, b.failure
	case b.runnerErr != "":
		return sandbox.StateFailed, b.runnerErr
	case b.stopReason != "":
		return sandbox.StateGone, b.stopReason
	case fault != "":
		return sandbox.StateFailed, fault
	case !b.heard:
		return sandbox.StateFailed, "the sandbox stopped before runplane-init connected" + b.consoleEndLocked()
	case b.runnerExit == nil:
		return sandbox.StateFailed, "the sandbox stopped unexpectedly" + b.consoleEndLocked()
	}
	return sandbox.StateGone, fmt.Sprintf("the runner exited with code %d", *b.runnerExit)
}

// consoleEnd returns the last console lines for an error message.
func (b *Box[T]) consoleEnd() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.consoleEndLocked()
}

func (b *Box[T]) consoleEndLocked() string {
	if len(b.lastLines) == 0 {
		return ""
	}
	return ": " + strings.Join(b.lastLines, " | ")
}

func (b *Box[T]) sandbox() sandbox.Sandbox {
	b.mu.Lock()
	defer b.mu.Unlock()
	return sandbox.Sandbox{ID: b.ID, PoolID: b.spec.PoolID, State: b.state, IP: b.ip, CreatedAt: b.created, Spec: b.spec}
}

// detach closes the control channel and the log, and stops the timer. The
// sandbox goes on.
func (b *Box[T]) detach() {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
	}
	ctl, logs := b.ctl, b.logs
	b.ctl, b.logs = nil, nil
	b.mu.Unlock()
	if ctl != nil {
		ctl.Close()
	}
	if logs != nil {
		logs.Close()
	}
}

const recordFile = "sandbox.json"

// record is the file of a sandbox that the driver needs after a restart. It
// never holds the JIT config.
type record[T any] struct {
	Mode       sandbox.Mode  `json:"mode"`
	Spec       sandbox.Spec  `json:"spec"`
	CreatedAt  time.Time     `json:"created_at"`
	IP         string        `json:"ip,omitempty"`
	State      sandbox.State `json:"state"`
	JobStarted time.Time     `json:"job_started,omitzero"`
	HoldUntil  time.Time     `json:"hold_until,omitzero"`
	RunnerExit *int          `json:"runner_exit,omitempty"`
	RunnerErr  string        `json:"runner_error,omitempty"`
	StopReason string        `json:"stop_reason,omitempty"`
	Failure    string        `json:"failure,omitempty"`
	Host       *T            `json:"host"`
}

// saveLocked writes the record, unless the sandbox is gone. The caller
// holds b.mu.
func (b *Box[T]) saveLocked() error {
	if b.state.Done() || isClosed(b.removed) {
		return nil
	}
	data, err := json.Marshal(record[T]{
		Mode: b.c.mode, Spec: b.spec, CreatedAt: b.created, IP: b.ip, State: b.state,
		JobStarted: b.jobStarted, HoldUntil: b.holdUntil, RunnerExit: b.runnerExit, RunnerErr: b.runnerErr,
		StopReason: b.stopReason, Failure: b.failure, Host: b.Host,
	})
	if err == nil {
		err = atomicfile.Write(filepath.Join(b.Dir, recordFile), bytes.NewReader(data), 0o600)
	}
	if err != nil {
		b.log.Warn("save the sandbox record", "err", err)
		return fmt.Errorf("save the sandbox record: %w", err)
	}
	return nil
}

func readRecord[T any](dir string) (record[T], error) {
	var rec record[T]
	data, err := os.ReadFile(filepath.Join(dir, recordFile))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("read %s: %w", filepath.Join(dir, recordFile), err)
	}
	return rec, nil
}
