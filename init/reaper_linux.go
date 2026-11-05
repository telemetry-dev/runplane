//go:build linux

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"

	"golang.org/x/sys/unix"
)

// reaper is the only code in init that waits for child processes. As PID 1,
// init also inherits orphans and must reap them; a second waiter, such as
// exec.Cmd.Wait, would race with it for exit codes.
type reaper struct {
	mu      sync.Mutex
	waiters map[int]waiter
}

type waiter struct {
	proc *os.Process
	exit chan int
}

func newReaper() *reaper {
	r := &reaper{waiters: map[int]waiter{}}
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, unix.SIGCHLD)
	go func() {
		for range sigs {
			r.reap()
		}
	}()
	return r
}

// start starts cmd. It returns the process ID and a channel that gets the
// exit code: the exit status, or 128 plus the signal number. Use only
// *os.File values for the standard streams of cmd, since nothing calls
// cmd.Wait. Do not read cmd.Process.Pid later: the reaper releases the
// process when it exits.
func (r *reaper) start(cmd *exec.Cmd) (int, <-chan int, error) {
	// Hold the lock across the start, so that reap finds the waiter even
	// when the child exits at once.
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	pid := cmd.Process.Pid
	exit := make(chan int, 1)
	r.waiters[pid] = waiter{proc: cmd.Process, exit: exit}
	return pid, exit, nil
}

func (r *reaper) run(cmd *exec.Cmd) (int, error) {
	_, exit, err := r.start(cmd)
	if err != nil {
		return 0, err
	}
	return <-exit, nil
}

func (r *reaper) reap() {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil || pid <= 0 {
			return
		}
		r.mu.Lock()
		w, ok := r.waiters[pid]
		delete(r.waiters, pid)
		r.mu.Unlock()
		if !ok {
			continue // an orphan
		}
		w.proc.Release()
		code := ws.ExitStatus()
		if ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
		w.exit <- code
	}
}
