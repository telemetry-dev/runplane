//go:build linux

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
)

func TestShell(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("the shell switches to a user, which needs root")
	}
	sock := socketPath(t)
	log := slog.New(slog.DiscardHandler)
	host, err := control.Listen(sock, control.GuestConfig{}, &recorder{}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	shells := newShellServer(log)
	acct := account{name: "root", home: t.TempDir(), shell: "/bin/sh"}
	shells.setStart(shellStarter(acct, newReaper()))
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}
	link := newGuestLink(dial, log, newLogQueue(1<<20), guestMux(newHostRequests(), shells))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go link.run(ctx)
	waitFor(t, "the guest", host.Connected)

	term, err := host.Shell(ctx, sandbox.TermSize{Rows: 30, Cols: 90})
	if err != nil {
		t.Fatal(err)
	}
	out := &output{}
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(out, term)
		readDone <- err
	}()

	io.WriteString(term, "stty size\n")
	waitFor(t, "the first size", func() bool { return out.has("30 90") })
	if err := term.Resize(sandbox.TermSize{Rows: 40, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	io.WriteString(term, "stty size; echo $HOME\n")
	waitFor(t, "the new size", func() bool { return out.has("40 100") && out.has(acct.home) })

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
	waitFor(t, "an open session", func() bool { return openSessions(shells) == 1 })
	term.Close()
	waitFor(t, "the session to end", func() bool { return openSessions(shells) == 0 })
}

func openSessions(s *shellServer) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open)
}

type output struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *output) has(s string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.Contains(o.buf.String(), s)
}
