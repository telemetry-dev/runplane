//go:build linux

package main

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox/control"
)

// After a runplane restart, the host has no JIT config for a guest that did
// not get its config yet, and it stops the sandbox. init must then stop
// asking for the config and end, or the VM never powers off.
func TestRunEndsWhenTheHostStopsBeforeTheConfig(t *testing.T) {
	sock := socketPath(t)
	log := slog.New(slog.DiscardHandler)
	var host *control.Server
	shutdown := make(chan error, 1)
	rec := &recorder{lost: func() { shutdown <- host.Shutdown(context.Background()) }}
	host, err := control.Listen(sock, nil, rec, log)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	g := &guestInit{log: log, logs: newLogQueue(1 << 20), req: newHostRequests(), shells: newShellServer(log)}
	g.link = newGuestLink(func(ctx context.Context) (net.Conn, error) { return dialHost(ctx, filepath.Dir(sock)) },
		log, g.logs, guestMux(g.req, g.shells))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.link.run(ctx)
	code := make(chan int, 1)
	go func() { code <- g.run() }()
	select {
	case c := <-code:
		if c != 0 {
			t.Errorf("run = %d, want 0 for a stop before the config", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("init still asks for its config after the host stopped the sandbox")
	}
	select {
	case err := <-shutdown:
		if err != nil {
			t.Errorf("shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the host never asked the guest to stop")
	}
}
