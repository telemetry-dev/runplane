package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
)

func TestControlChannel(t *testing.T) {
	sock := socketPath(t)
	log := slog.New(slog.DiscardHandler)
	cfg := control.GuestConfig{
		SandboxID:        "01jbz3x0000000000000000000",
		Hostname:         "ubuntu-24-04-01jbz3x0",
		JITConfig:        "eyJqaXQiOiJjb25maWcifQ==",
		RunnerDir:        "/opt/runplane/runner",
		Env:              map[string]string{"ACTIONS_RESULTS_URL": "http://10.0.0.1:8486/"},
		DNS:              []string{"1.1.1.1", "8.8.8.8"},
		RegistryMirror:   "http://10.0.0.1:8486",
		TimeLimitSeconds: 3600,
		Debug:            true,
	}
	rec1 := &recorder{}
	host1, err := control.Listen(sock, cfg, rec1, log)
	if err != nil {
		t.Fatal(err)
	}

	sent := &syncBuffer{}
	dial := func(ctx context.Context) (net.Conn, error) {
		c, err := dialHost(ctx, sock)
		if err != nil {
			return nil, err
		}
		return &recordConn{Conn: c, sent: sent}, nil
	}
	logs := newLogQueue(1 << 20)
	req := newHostRequests()
	link := newGuestLink(dial, log, logs, guestMux(req, newShellServer(log)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go link.run(ctx)

	got, err := link.config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("config = %+v, want %+v", got, cfg)
	}

	link.event(sandbox.RunnerEvent{Kind: "listening"})
	logs.push("runner", "√ Connected to GitHub")
	logs.push("dockerd", "")
	long := strings.Repeat("x", control.MaxLine+10)
	logs.push("diag", long)
	waitFor(t, "first events and logs", func() bool {
		return len(rec1.eventList()) == 1 && len(rec1.logList()) == 4
	})
	if ev := rec1.eventList(); ev[0] != (sandbox.RunnerEvent{Kind: "listening"}) {
		t.Errorf("events = %+v", ev)
	}
	lines := rec1.logList()
	if l := lines[0]; l.Source != "runner" || l.Text != "√ Connected to GitHub" || time.Since(l.Time) > time.Minute || l.Time.Location() != time.UTC {
		t.Errorf("first log line = %+v", l)
	}
	if l := lines[1]; l.Source != "dockerd" || l.Text != "" {
		t.Errorf("empty log line = %+v", l)
	}
	if lines[2].Source != "diag" || lines[3].Source != "diag" || lines[2].Text+lines[3].Text != long {
		t.Errorf("a long line did not arrive as two diag lines: sources %q %q, lengths %d %d",
			lines[2].Source, lines[3].Source, len(lines[2].Text), len(lines[3].Text))
	}

	// runplane restarts: the guest keeps events and logs, connects again,
	// sends resume first, and does not ask for the config again.
	host1.Close()
	waitFor(t, "the guest to see the host leave", func() bool {
		link.mu.Lock()
		defer link.mu.Unlock()
		return link.conn == nil
	})
	link.event(sandbox.RunnerEvent{Kind: "job.started", JobName: "build"})
	logs.push("runner", "while the host was away")
	rec2 := &recorder{}
	host2, err := control.Listen(sock, control.GuestConfig{}, rec2, log)
	if err != nil {
		t.Fatal(err)
	}
	defer host2.Close()
	waitFor(t, "events after the restart", func() bool { return len(rec2.eventList()) == 2 })
	want := []sandbox.RunnerEvent{{Kind: "resume"}, {Kind: "job.started", JobName: "build"}}
	if ev := rec2.eventList(); !slices.Equal(ev, want) {
		t.Errorf("events after the restart = %+v, want %+v", ev, want)
	}
	waitFor(t, "the log line from the outage", func() bool {
		return slices.ContainsFunc(rec2.logList(), func(l control.LogLine) bool { return l.Text == "while the host was away" })
	})
	// HTTP requests travel as plain text inside yamux frames.
	if n := strings.Count(sent.String(), "GET /v1/config "); n != 1 {
		t.Errorf("the guest asked for the config %d times, want 1", n)
	}
	if !host2.Connected() {
		t.Error("Connected() = false with a guest on the line")
	}

	// A hold keeps init after the runner exits, until a shutdown.
	if err := host2.Hold(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	go func() {
		req.waitHold(false, 0)
		close(held)
	}()
	select {
	case <-held:
		t.Fatal("init did not wait for the hold")
	case <-time.After(100 * time.Millisecond):
	}
	if err := host2.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the shutdown did not end the hold")
	}

	// At the end, init waits until the host has the last event and line.
	link.event(sandbox.RunnerEvent{Kind: "exited", ExitCode: 3})
	logs.push("init", "stopping the sandbox")
	flushCtx, flushCancel := context.WithTimeout(ctx, 5*time.Second)
	defer flushCancel()
	link.flushEvents(flushCtx)
	link.flushLogs(flushCtx)
	if flushCtx.Err() != nil {
		t.Fatal("flush timed out")
	}
	if ev := rec2.eventList(); ev[len(ev)-1] != (sandbox.RunnerEvent{Kind: "exited", ExitCode: 3}) {
		t.Errorf("last event = %+v", ev[len(ev)-1])
	}
	if l := rec2.logList(); l[len(l)-1].Text != "stopping the sandbox" {
		t.Errorf("last log line = %+v", l[len(l)-1])
	}

	cancel()
	waitFor(t, "the host to see the guest leave", func() bool { return !host2.Connected() })
	if err := host2.Hold(context.Background(), time.Now()); err != control.ErrNotConnected {
		t.Errorf("Hold without a guest = %v, want ErrNotConnected", err)
	}
}

// In dev mode the guest dials the TCP address in control.addr. After a host
// restart the address and token change, and the guest must get in again.
func TestDialHostTCPAfterRestart(t *testing.T) {
	sock := socketPath(t)
	log := slog.New(slog.DiscardHandler)
	link := newGuestLink(func(ctx context.Context) (net.Conn, error) { return dialHost(ctx, sock) },
		log, newLogQueue(1<<20), guestMux(newHostRequests(), newShellServer(log)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := range 2 {
		tcp, err := control.ListenTCP("127.0.0.1:0", "127.0.0.1", log)
		if err != nil {
			t.Fatal(err)
		}
		host, err := tcp.Listen(filepath.Dir(sock), control.GuestConfig{SandboxID: "01jbz3x0000000000000000000"}, &recorder{}, log)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			go link.run(ctx)
		}
		waitFor(t, fmt.Sprintf("connection %d over TCP", i+1), host.Connected)
		host.Close()
		tcp.Close()
	}
}

func TestLogQueueDropsOldest(t *testing.T) {
	// Each line is "<13 digits> runner line-N\n", 29 bytes, so 3 lines fit.
	q := newLogQueue(100)
	for i := range 10 {
		q.push("runner", fmt.Sprintf("line-%02d", i))
	}
	b, err := q.take(nil)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for line := range strings.Lines(string(b)) {
		_, rest, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		texts = append(texts, rest)
	}
	want := []string{
		"init dropped 7 older log lines: the log buffer was full",
		"runner line-07",
		"runner line-08",
		"runner line-09",
	}
	if !slices.Equal(texts, want) {
		t.Errorf("queue = %q, want %q", texts, want)
	}
}

func TestWaitHoldInDebugWaitsForALateHold(t *testing.T) {
	req := newHostRequests()
	go func() {
		time.Sleep(20 * time.Millisecond)
		req.setHold(time.Now().Add(300 * time.Millisecond))
	}()
	start := time.Now()
	req.waitHold(true, 100*time.Millisecond)
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("waitHold returned after %v; the hold that came during the grace time is 300ms", d)
	}
}

// socketPath returns a short path: macOS limits Unix socket paths to 104 bytes.
func socketPath(t *testing.T) string {
	dir, err := os.MkdirTemp("", "rp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "control.sock")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// After a runplane restart, the host has no JIT config for a guest that did
// not get its config yet, and it stops the sandbox. The guest must then stop
// asking for the config, or the VM never powers off.
func TestShutdownWhileWaitingForConfig(t *testing.T) {
	sock := socketPath(t)
	log := slog.New(slog.DiscardHandler)
	var host *control.Server
	shutdown := make(chan error, 1)
	rec := &recorder{lost: func() { shutdown <- host.Shutdown(context.Background()) }}
	host, err := control.Listen(sock, control.GuestConfig{SandboxID: "s"}, rec, log)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	req := newHostRequests()
	link := newGuestLink(func(ctx context.Context) (net.Conn, error) { return dialHost(ctx, sock) },
		log, newLogQueue(1<<20), guestMux(req, newShellServer(log)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go link.run(ctx)
	// As guest.run does: a shutdown ends the wait for the config.
	configCtx, stop := context.WithCancel(ctx)
	go func() {
		<-req.stop
		stop()
	}()
	done := make(chan error, 1)
	go func() {
		_, err := link.config(configCtx)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("the guest got a config without a JIT config")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the guest still asks for its config after the host stopped the sandbox")
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

type recorder struct {
	mu     sync.Mutex
	events []sandbox.RunnerEvent
	logs   []control.LogLine
	lost   func() // OnLostConfig, if set
}

func (r *recorder) OnEvent(ev sandbox.RunnerEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) OnLostConfig() {
	if r.lost != nil {
		r.lost()
	}
}

func (r *recorder) OnLog(l control.LogLine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, l)
}

func (r *recorder) eventList() []sandbox.RunnerEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *recorder) logList() []control.LogLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.logs)
}

// recordConn copies what the guest writes to sent.
type recordConn struct {
	net.Conn
	sent io.Writer
}

func (c *recordConn) Write(p []byte) (int, error) {
	c.sent.Write(p)
	return c.Conn.Write(p)
}

// syncBuffer is a bytes.Buffer that goroutines can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
