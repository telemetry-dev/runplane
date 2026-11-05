//go:build linux

package main

import (
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/sandbox"
)

func shellStarter(acct account, rp *reaper) func(sandbox.TermSize) (*shell, error) {
	return func(size sandbox.TermSize) (*shell, error) {
		ptmx, tty, err := pty.Open()
		if err != nil {
			return nil, err
		}
		defer tty.Close()
		if err := pty.Setsize(ptmx, &pty.Winsize{Rows: size.Rows, Cols: size.Cols}); err != nil {
			ptmx.Close()
			return nil, err
		}
		// A leading "-" in argv[0] makes a login shell.
		cmd := &exec.Cmd{
			Path:   acct.shell,
			Args:   []string{"-" + filepath.Base(acct.shell)},
			Dir:    acct.home,
			Env:    acct.env("TERM=xterm-256color"),
			Stdin:  tty,
			Stdout: tty,
			Stderr: tty,
			SysProcAttr: &syscall.SysProcAttr{
				Setsid:     true,
				Setctty:    true,
				Credential: acct.credential(),
			},
		}
		pid, exit, err := rp.start(cmd)
		if err != nil {
			ptmx.Close()
			return nil, err
		}
		return &shell{pty: ptmx, stop: func() {
			unix.Kill(-pid, unix.SIGHUP)
			ptmx.Close()
			select {
			case <-exit:
			case <-time.After(5 * time.Second):
				unix.Kill(-pid, unix.SIGKILL)
				<-exit
			}
		}}, nil
	}
}
