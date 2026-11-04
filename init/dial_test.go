package main

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"testing"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
)

// In dev mode the guest dials the TCP address in control.addr. After a host
// restart the address and token change, and the guest must get in again.
func TestDialHostTCPAfterRestart(t *testing.T) {
	sock := socketPath(t)
	dir := filepath.Dir(sock)
	log := slog.New(slog.DiscardHandler)
	cfg := control.GuestConfig{SandboxID: "01jbz3x0000000000000000000", JITConfig: "jit", RunnerDir: "/r"}

	tcp1, err := control.ListenTCP("127.0.0.1:0", "127.0.0.1", log)
	if err != nil {
		t.Fatal(err)
	}
	rec1 := &recorder{}
	host1, err := tcp1.Listen(dir, cfg, rec1, log)
	if err != nil {
		t.Fatal(err)
	}

	link := newGuestLink(func(ctx context.Context) (net.Conn, error) { return dialHost(ctx, sock) },
		log, newLogQueue(1<<20), guestMux(newHostRequests(), newShellServer(log)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go link.run(ctx)
	got, err := link.config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.JITConfig != "jit" {
		t.Fatalf("config = %+v", got)
	}
	link.event(sandbox.RunnerEvent{Kind: "listening"})
	waitFor(t, "the first event", func() bool { return len(rec1.eventList()) == 1 })

	host1.Close()
	tcp1.Close()
	tcp2, err := control.ListenTCP("127.0.0.1:0", "127.0.0.1", log)
	if err != nil {
		t.Fatal(err)
	}
	defer tcp2.Close()
	rec2 := &recorder{}
	host2, err := tcp2.Listen(dir, control.GuestConfig{SandboxID: cfg.SandboxID}, rec2, log)
	if err != nil {
		t.Fatal(err)
	}
	defer host2.Close()
	link.event(sandbox.RunnerEvent{Kind: "job.started", JobName: "build"})
	waitFor(t, "events after the restart", func() bool { return len(rec2.eventList()) == 2 })
	want := []sandbox.RunnerEvent{{Kind: "resume"}, {Kind: "job.started", JobName: "build"}}
	if ev := rec2.eventList(); !slices.Equal(ev, want) {
		t.Errorf("events after the restart = %+v, want %+v", ev, want)
	}
}

// Without control.addr the guest dials the Unix socket (container mode).
func TestDialHostUnix(t *testing.T) {
	sock := socketPath(t)
	log := slog.New(slog.DiscardHandler)
	host, err := control.Listen(sock, control.GuestConfig{SandboxID: "s"}, &recorder{}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	conn, err := dialHost(context.Background(), sock)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}
