package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
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
	conn       *linkConn             // nil while disconnected
	up         chan struct{}         // closed when a connection opens
	events     []sandbox.RunnerEvent // not sent yet
	newEvent   chan struct{}         // closed and replaced on each new event
	haveConfig bool
	logsDone   chan struct{} // closed when the host has the full log
	logsOnce   sync.Once
}

type linkConn struct {
	client *http.Client
	done   <-chan struct{} // closed when the connection drops
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
		cfg := yamux.DefaultConfig()
		cfg.LogOutput = nil
		cfg.Logger = slog.NewLogLogger(l.log.Handler(), slog.LevelDebug)
		sess, err := yamux.Client(conn, cfg)
		if err != nil {
			conn.Close()
			l.log.Warn("start yamux session", "err", err)
			sleep(ctx, time.Second)
			continue
		}
		l.serve(ctx, sess)
		sleep(ctx, time.Second)
	}
}

func (l *guestLink) serve(ctx context.Context, sess *yamux.Session) {
	srv := &http.Server{Handler: l.mux, ErrorLog: slog.NewLogLogger(l.log.Handler(), slog.LevelDebug)}
	go srv.Serve(sess)
	c := &linkConn{
		client: &http.Client{Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) { return sess.Open() },
		}},
		done: sess.CloseChan(),
	}

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
	case <-c.done:
	case <-ctx.Done():
		sess.Close()
	}

	l.mu.Lock()
	l.conn = nil
	l.up = make(chan struct{})
	l.mu.Unlock()
}

// config gets the GuestConfig. It waits for a connection and asks again on
// the next connection if a request fails. Call it one time only.
func (l *guestLink) config(ctx context.Context) (control.GuestConfig, error) {
	for {
		c, err := l.wait(ctx)
		if err != nil {
			return control.GuestConfig{}, err
		}
		cfg, err := getConfig(ctx, c.client)
		if err == nil {
			l.mu.Lock()
			l.haveConfig = true
			l.mu.Unlock()
			return cfg, nil
		}
		l.log.Warn("get config", "err", err)
		select {
		case <-c.done:
		case <-time.After(time.Second):
		case <-ctx.Done():
			return control.GuestConfig{}, ctx.Err()
		}
	}
}

func getConfig(ctx context.Context, client *http.Client) (control.GuestConfig, error) {
	var cfg control.GuestConfig
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://host/v1/config", nil)
	if err != nil {
		return cfg, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return cfg, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return cfg, fmt.Errorf("host answered %s", resp.Status)
	}
	err = json.NewDecoder(resp.Body).Decode(&cfg)
	return cfg, err
}

func (l *guestLink) wait(ctx context.Context) (*linkConn, error) {
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

func (l *guestLink) sendEvents(c *linkConn) {
	for {
		l.mu.Lock()
		if len(l.events) == 0 {
			wake := l.newEvent
			l.mu.Unlock()
			select {
			case <-wake:
				continue
			case <-c.done:
				return
			}
		}
		ev := l.events[0]
		l.mu.Unlock()
		status, err := postEvent(c.client, ev)
		select {
		case <-c.done:
			// The host may not have the event. Send it again on the next connection.
			return
		default:
		}
		if err != nil {
			l.log.Warn("send runner event", "kind", ev.Kind, "err", err)
			select {
			case <-time.After(time.Second):
			case <-c.done:
			}
			continue
		}
		if status != http.StatusNoContent {
			l.log.Warn("the host rejected a runner event", "kind", ev.Kind, "status", status)
		}
		l.mu.Lock()
		if len(l.events) > 0 && l.events[0] == ev {
			l.events = l.events[1:]
		}
		l.mu.Unlock()
	}
}

func postEvent(client *http.Client, ev sandbox.RunnerEvent) (int, error) {
	body, _ := json.Marshal(ev) // only strings and an int
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://host/v1/events", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
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
func (l *guestLink) streamLogs(c *linkConn) {
	for {
		req, err := http.NewRequest(http.MethodPost, "http://host/v1/logs", l.logs.reader(c.done))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "text/plain")
		resp, err := c.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				l.logsOnce.Do(func() { close(l.logsDone) })
				return
			}
			err = fmt.Errorf("host answered %s", resp.Status)
		}
		select {
		case <-c.done:
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
// dev mode. In dev mode the ctl folder also holds control.addr: then the
// guest dials TCP and sends the hello line first. It reads the file at each
// dial, because the host writes a new token after a restart.
func dialHost(ctx context.Context, socket string) (net.Conn, error) {
	// A connect that hangs, for example in the TCP proxy of Docker on
	// macOS, would otherwise wait for the kernel limit of about 2 minutes.
	d := net.Dialer{Timeout: 5 * time.Second}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(socket), control.AddrFile))
	if errors.Is(err, fs.ErrNotExist) {
		return d.DialContext(ctx, "unix", socket)
	}
	if err != nil {
		return nil, err
	}
	addr, hello, err := control.ParseAddr(b)
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
