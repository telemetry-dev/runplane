package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/tlsutil"
	"github.com/runplane/runplane/internal/version"
)

// ErrUpdated is the result of Run after the primary updated the binary of
// the agent. The process must exit, so that systemd starts the new binary.
var ErrUpdated = errors.New("agent binary updated, restart to run it")

type Config struct {
	Path string // agent.json
	Node Node
	// CacheProxy, if not nil, sends its requests through the connection
	// that Run keeps.
	CacheProxy *CacheProxy
	// Mirror returns the stats of the registry mirror of the agent. Nil:
	// zero stats.
	Mirror func() mirror.Stats
	Logger *slog.Logger
}

const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
	// maxQueued is the number of events that the agent keeps while it has
	// no connection.
	maxQueued = 4096
)

// Run connects the agent to the primary and serves the agent routes of
// section 5 from cfg.Node until ctx ends. It connects again after each
// disconnect, and the sandboxes keep operating in the meantime. Run
// returns nil when ctx ends, ErrUpdated after an update, or an error if
// agent.json is not valid.
func Run(ctx context.Context, cfg Config) error {
	f, err := readAgentFile(cfg.Path)
	if err != nil {
		return err
	}
	a := &agent{
		path:    cfg.Path,
		file:    f,
		saved:   f,
		node:    cfg.Node,
		proxy:   cfg.CacheProxy,
		mirror:  cfg.Mirror,
		log:     orDiscard(cfg.Logger),
		out:     &outbox{ready: make(chan struct{}, 1)},
		updated: make(chan struct{}),
	}
	a.routes = a.newRoutes()
	go a.pump(ctx)

	delay := minBackoff
	for {
		sess, err := a.connect(ctx)
		if err == nil {
			delay = minBackoff
			err = a.serve(ctx, sess)
		}
		switch {
		case errors.Is(err, ErrUpdated):
			return err
		case ctx.Err() != nil:
			return nil
		case err != nil:
			a.log.Warn("no connection to the primary", "primary", a.file.Primary, "retry_in", delay, "err", err)
		default:
			a.log.Warn("connection to the primary ended", "primary", a.file.Primary)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay = min(2*delay, maxBackoff)
	}
}

type agent struct {
	path   string
	file   agentFile // what the agent uses
	saved  agentFile // what agent.json holds
	node   Node
	proxy  *CacheProxy
	mirror func() mirror.Stats
	log    *slog.Logger
	out    *outbox
	routes http.Handler

	updateOnce sync.Once
	updated    chan struct{}
}

// connect dials the primary, runs the handshake, and starts yamux.
func (a *agent) connect(ctx context.Context) (*yamux.Session, error) {
	info, err := a.node.Facts(ctx)
	if err != nil {
		a.log.Warn("read host facts", "err", err)
	}
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: handshakeTimeout, KeepAlive: 15 * time.Second},
		Config:    tlsutil.PinnedClientConfig(a.file.Fingerprint),
	}
	conn, err := d.DialContext(ctx, "tcp", a.file.Primary)
	if err != nil {
		return nil, fmt.Errorf("dial primary: %w", err)
	}
	sess, err := a.handshake(conn, info)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return sess, nil
}

func (a *agent) handshake(conn net.Conn, info host.Info) (*yamux.Session, error) {
	conn.SetDeadline(time.Now().Add(handshakeTimeout))
	hello := a.file.credentials()
	hello.Version = version.Version
	hello.Mode = a.node.Driver().Mode()
	hello.Host = info
	if err := writeJSONLine(conn, hello); err != nil {
		return nil, fmt.Errorf("send hello: %w", err)
	}
	br := bufio.NewReaderSize(conn, maxLine)
	var r reply
	if err := readJSONLine(br, &r); err != nil {
		return nil, fmt.Errorf("read reply of the primary: %w", err)
	}
	if !r.OK {
		return nil, fmt.Errorf("primary refused the agent: %s", r.Error)
	}
	if err := a.remember(r); err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	sess, err := yamux.Server(&bufConn{conn, br}, yamuxConfig(a.log))
	if err != nil {
		return nil, fmt.Errorf("start yamux: %w", err)
	}
	return sess, nil
}

// remember keeps the server ID and secret from the reply, and writes
// agent.json without the token when they change.
func (a *agent) remember(r reply) error {
	f := a.file
	if r.ServerID != "" {
		f.ServerID = r.ServerID
	}
	if r.Secret != "" {
		f.Secret = r.Secret
	}
	if !f.registered() {
		return errors.New("primary sent no server ID or secret")
	}
	f.Token = ""
	if f != a.file {
		a.log.Info("registered with the primary", "server_id", f.ServerID)
	}
	a.file = f
	if a.saved == f {
		return nil
	}
	// Keep the identity in memory when the write fails: the token is used,
	// so the agent cannot register again. The next connection tries again.
	if err := f.write(a.path); err != nil {
		a.log.Error("save the server ID and secret; the agent cannot connect after a restart until this works", "err", err)
		return nil
	}
	a.saved = f
	return nil
}

// serve operates one connection until it ends.
func (a *agent) serve(ctx context.Context, sess *yamux.Session) error {
	defer sess.Close()
	a.log.Info("connected to the primary", "primary", a.file.Primary, "server_id", a.file.ServerID)
	if a.proxy != nil {
		a.proxy.attach(sess)
		defer a.proxy.detach(sess)
	}
	go serveStreams(sess, a.routes, a.log)
	go a.sendEvents(sess)
	select {
	case <-ctx.Done():
		return nil
	case <-sess.CloseChan():
		return nil
	case <-a.updated:
		return ErrUpdated
	}
}

// pump moves the driver events and host stats to the outbox for the life
// of the agent, also while it has no connection.
func (a *agent) pump(ctx context.Context) {
	events, stats := a.node.Driver().Events(), a.node.Stats()
	for events != nil || stats != nil {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			a.out.addEvent(ev)
		case st, ok := <-stats:
			if !ok {
				stats = nil
				continue
			}
			a.out.setStats(st)
		}
	}
}

// sendEvents streams the outbox to POST /v1/events. When the stream
// breaks, it closes the session, because the primary would miss state
// changes; the agent then connects again.
func (a *agent) sendEvents(sess *yamux.Session) {
	defer sess.Close()
	if n := a.out.takeDropped(); n > 0 {
		a.log.Warn("dropped events while not connected", "count", n)
	}
	pr, pw := io.Pipe()
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://primary/v1/events", pr)
		req.Header.Set("Content-Type", "application/x-ndjson")
		resp, err := streamClient(sess).Do(req)
		if err == nil {
			resp.Body.Close()
			err = errors.New("primary ended the event stream")
		}
		pr.CloseWithError(err)
	}()
	enc := json.NewEncoder(pw)
	for {
		it, ok := a.out.take(sess.CloseChan())
		if !ok {
			pw.Close()
			return
		}
		if err := enc.Encode(it); err != nil {
			a.out.putBack(it)
			a.log.Debug("event stream ended", "err", err)
			return
		}
	}
}

// outbox holds the items for the event stream: the events in order, and
// the latest host stats. When it holds maxQueued events, it drops the
// oldest.
type outbox struct {
	mu      sync.Mutex
	events  []sandbox.Event
	stats   *host.Stats
	dropped int
	ready   chan struct{} // has a value after each change
}

func (o *outbox) addEvent(ev sandbox.Event) {
	o.mu.Lock()
	if len(o.events) >= maxQueued {
		o.events = o.events[1:]
		o.dropped++
	}
	o.events = append(o.events, ev)
	o.mu.Unlock()
	o.signal()
}

func (o *outbox) setStats(st host.Stats) {
	o.mu.Lock()
	o.stats = &st
	o.mu.Unlock()
	o.signal()
}

// putBack returns an event that did not go out to the front of the queue.
func (o *outbox) putBack(it streamItem) {
	if it.Event == nil {
		return
	}
	o.mu.Lock()
	o.events = append([]sandbox.Event{*it.Event}, o.events...)
	o.mu.Unlock()
	o.signal()
}

// take waits for the next item, events first. It returns false when done
// closes, also when items are waiting, so that no item goes to a stream
// that has ended.
func (o *outbox) take(done <-chan struct{}) (streamItem, bool) {
	for {
		select {
		case <-done:
			return streamItem{}, false
		default:
		}
		o.mu.Lock()
		var it streamItem
		switch {
		case len(o.events) > 0:
			ev := o.events[0]
			o.events = o.events[1:]
			it.Event = &ev
		case o.stats != nil:
			it.Stats, o.stats = o.stats, nil
		}
		o.mu.Unlock()
		if it.Event != nil || it.Stats != nil {
			return it, true
		}
		select {
		case <-o.ready:
		case <-done:
		}
	}
}

func (o *outbox) takeDropped() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := o.dropped
	o.dropped = 0
	return n
}

func (o *outbox) signal() {
	select {
	case o.ready <- struct{}{}:
	default:
	}
}
