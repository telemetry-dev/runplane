package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// guestLink is the guest end of the control channel. It dials the host again
// each second after a drop, keeps events until the host has them, and streams
// the log queue on each connection.
type guestLink struct {
	dial func(context.Context) (net.Conn, error)
	log  *slog.Logger
	logs *logQueue
	mux  http.Handler // what the guest serves to the host

	mu         sync.Mutex
	conn       *guest.Client         // nil while disconnected
	up         chan struct{}         // closed when a connection opens
	events     []sandbox.RunnerEvent // not sent yet
	newEvent   chan struct{}         // closed and replaced on each new event
	haveConfig bool
	logsDone   chan struct{} // closed when the host has the full log
	logsOnce   sync.Once
}

func newGuestLink(dial func(context.Context) (net.Conn, error), log *slog.Logger, logs *logQueue, mux http.Handler) *guestLink {
	return &guestLink{
		dial:     dial,
		log:      log,
		logs:     logs,
		mux:      mux,
		up:       make(chan struct{}),
		newEvent: make(chan struct{}),
		logsDone: make(chan struct{}),
	}
}

// run keeps the connection to the host open until ctx ends.
func (l *guestLink) run(ctx context.Context) {
	failing := false
	for ctx.Err() == nil {
		conn, err := l.dial(ctx)
		if err != nil {
			if !failing {
				l.log.Warn("cannot reach the host, trying again each second", "err", err)
				failing = true
			}
			sleep(ctx, time.Second)
			continue
		}
		failing = false
		c, err := guest.NewClient(conn, l.mux, l.log)
		if err != nil {
			l.log.Warn("connect to the host", "err", err)
			sleep(ctx, time.Second)
			continue
		}
		l.serve(ctx, c)
		sleep(ctx, time.Second)
	}
}

func (l *guestLink) serve(ctx context.Context, c *guest.Client) {
	l.mu.Lock()
	if l.haveConfig {
		l.events = append([]sandbox.RunnerEvent{{Kind: sandbox.RunnerResume}}, l.events...)
		l.log.Info("connected to the host again")
	}
	l.conn = c
	close(l.up)
	l.mu.Unlock()

	go l.sendEvents(c)
	go l.streamLogs(c)
	select {
	case <-c.Done():
	case <-ctx.Done():
		c.Close()
	}

	l.mu.Lock()
	l.conn = nil
	l.up = make(chan struct{})
	l.mu.Unlock()
}

// config waits for a connection. When a request fails, it asks again after a
// second or on the next connection. Call it one time only.
func (l *guestLink) config(ctx context.Context) (guest.Config, error) {
	for {
		c, err := l.wait(ctx)
		if err != nil {
			return guest.Config{}, err
		}
		cfg, err := c.Config(ctx)
		if err == nil {
			l.mu.Lock()
			l.haveConfig = true
			l.mu.Unlock()
			return cfg, nil
		}
		l.log.Warn("get config", "err", err)
		select {
		case <-c.Done():
		case <-time.After(time.Second):
		case <-ctx.Done():
			return guest.Config{}, ctx.Err()
		}
	}
}

func (l *guestLink) wait(ctx context.Context) (*guest.Client, error) {
	for {
		l.mu.Lock()
		c, up := l.conn, l.up
		l.mu.Unlock()
		if c != nil {
			return c, nil
		}
		select {
		case <-up:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (l *guestLink) event(ev sandbox.RunnerEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
	close(l.newEvent)
	l.newEvent = make(chan struct{})
}

func (l *guestLink) sendEvents(c *guest.Client) {
	for {
		l.mu.Lock()
		if len(l.events) == 0 {
			wake := l.newEvent
			l.mu.Unlock()
			select {
			case <-wake:
				continue
			case <-c.Done():
				return
			}
		}
		ev := l.events[0]
		l.mu.Unlock()
		err := postEvent(c, ev)
		select {
		case <-c.Done():
			// The host may not have the event. Send it again on the next connection.
			return
		default:
		}
		switch {
		case errors.Is(err, guest.ErrRejected):
			l.log.Warn("the host rejected a runner event", "kind", ev.Kind, "err", err)
		case err != nil:
			l.log.Warn("send runner event", "kind", ev.Kind, "err", err)
			select {
			case <-time.After(time.Second):
			case <-c.Done():
			}
			continue
		}
		l.mu.Lock()
		if len(l.events) > 0 && l.events[0] == ev {
			l.events = l.events[1:]
		}
		l.mu.Unlock()
	}
}

func postEvent(c *guest.Client, ev sandbox.RunnerEvent) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.PostEvent(ctx, ev)
}

func (l *guestLink) flushEvents(ctx context.Context) {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		l.mu.Lock()
		n := len(l.events)
		l.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}

// streamLogs sends the log queue as one streamed request until the
// connection drops or the queue closes. The host answers 204 only after the
// end of the queue.
func (l *guestLink) streamLogs(c *guest.Client) {
	for {
		err := c.StreamLogs(context.Background(), l.logs.reader(c.Done()))
		if err == nil {
			l.logsOnce.Do(func() { close(l.logsDone) })
			return
		}
		select {
		case <-c.Done():
			return
		case <-time.After(time.Second):
			l.log.Warn("log stream broke, starting it again", "err", err)
		}
	}
}

func (l *guestLink) flushLogs(ctx context.Context) {
	l.logs.close()
	select {
	case <-l.logsDone:
	case <-ctx.Done():
	}
}

// dialHost connects to the host end of the control channel in container and
// dev mode: the socket in the ctl folder dir. In dev mode, dir has the
// guest.AddrFile instead: then the guest dials TCP and sends the hello line
// first. It reads the file at each dial, because the host writes a new token
// after a restart.
func dialHost(ctx context.Context, dir string) (net.Conn, error) {
	// A connect that hangs, for example in the TCP proxy of Docker on
	// macOS, would otherwise wait for the kernel limit of about 2 minutes.
	d := net.Dialer{Timeout: 5 * time.Second}
	b, err := os.ReadFile(filepath.Join(dir, guest.AddrFile))
	if errors.Is(err, fs.ErrNotExist) {
		return d.DialContext(ctx, "unix", filepath.Join(dir, guest.SocketFile))
	}
	if err != nil {
		return nil, err
	}
	addr, hello, err := guest.ParseAddr(b)
	if err != nil {
		return nil, err
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, hello); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetWriteDeadline(time.Time{})
	return conn, nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}
