//go:build linux

package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
)

func TestShell(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("the shell switches to a user, which needs root")
	}
	sock := socketPath(t)
	log := slog.New(slog.DiscardHandler)
	rec := &recorder{}
	host, err := control.Listen(sock, nil, rec, log)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	shells := newShellServer(log)
	acct := account{name: "root", home: t.TempDir(), shell: "/bin/sh"}
	shells.setStart(shellStarter(acct, newReaper()))
	link := newGuestLink(func(ctx context.Context) (net.Conn, error) { return dialHost(ctx, filepath.Dir(sock)) },
		log, newLogQueue(1<<20), guestMux(newHostRequests(), shells))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go link.run(ctx)
	waitFor(t, "the guest", func() bool { return rec.connects.Load() > 0 })

	term, err := host.Shell(ctx, sandbox.TermSize{Rows: 30, Cols: 90})
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	has := func(s string) func() bool {
		return func() bool { return strings.Contains(out.String(), s) }
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(out, term)
		readDone <- err
	}()

	io.WriteString(term, "stty size\n")
	waitFor(t, "the first size", has("30 90"))
	if err := term.Resize(sandbox.TermSize{Rows: 40, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	io.WriteString(term, "stty size; echo $HOME\n")
	waitFor(t, "the new size", has("40 100"))
	waitFor(t, "the home folder", has(acct.home))

	// When the shell exits, the stream ends.
	io.WriteString(term, "exit\n")
	select {
	case <-readDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream stayed open after the shell exited")
	}
	term.Close()

	// When the host closes the stream, the shell stops and its session ends.
	term, err = host.Shell(ctx, sandbox.TermSize{})
	if err != nil {
		t.Fatal(err)
	}
	out = &syncBuffer{}
	go io.Copy(out, term)
	io.WriteString(term, "echo pid=$$\n")
	pidLine := regexp.MustCompile(`pid=(\d+)`)
	waitFor(t, "the shell PID", func() bool { return pidLine.MatchString(out.String()) })
	pid, _ := strconv.Atoi(pidLine.FindStringSubmatch(out.String())[1])
	term.Close()
	waitFor(t, "the shell to exit", func() bool { return unix.Kill(pid, 0) == unix.ESRCH })
	waitFor(t, "the session to end", func() bool {
		err := term.Resize(sandbox.TermSize{Rows: 10, Cols: 10})
		return err != nil && strings.Contains(err.Error(), "404 Not Found")
	})
}
