package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/runplane/runplane/internal/atomicfile"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/tlsutil"
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
	Version    string // of runplane on this server
	Logger     *slog.Logger
}

const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
	// maxQueued is the number of events that the agent keeps while it has
	// no connection.
	maxQueued = 4096
	// updateWait is how long the agent waits after an update for the
	// primary to close the connection, which it does when it has the
	// answer.
	updateWait = 10 * time.Second
)

// Run connects the agent to the primary and serves the agent routes from
// cfg.Node until ctx ends. It connects again after each disconnect, and the
// sandboxes keep operating in the meantime. Run returns nil when ctx ends,
// ErrUpdated after an update, or an error if agent.json is not valid.
func Run(ctx context.Context, cfg Config) error {
	f, err := readAgentFile(cfg.Path)
	if err != nil {
		return err
	}
	a := &agent{
		Config:  cfg,
		file:    f,
		saved:   f,
		out:     &outbox{ready: make(chan struct{}, 1)},
		updated: make(chan struct{}),
	}
	mux := http.NewServeMux()
	for _, h := range handlers {
		mux.HandleFunc(h.pattern, h.serve(a))
	}
	a.routes = mux
	go Forward(ctx, cfg.Node, a.out.addEvent, a.out.setStats)

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
			a.Logger.Warn("no connection to the primary", "primary", a.file.Primary, "retry_in", delay, "err", err)
		default:
			a.Logger.Warn("connection to the primary ended", "primary", a.file.Primary)
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
	Config
	file   agentFile // what the agent uses
	saved  agentFile // what agent.json holds
	out    *outbox
	routes http.Handler

	updateOnce sync.Once
	updated    chan struct{}
}

// connect dials the primary, runs the handshake, and starts yamux.
func (a *agent) connect(ctx context.Context) (*yamux.Session, error) {
	info, err := a.Node.Facts(ctx)
	if err != nil {
		a.Logger.Warn("read host facts", "err", err)
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
	hello := helloLine{Version: a.Version, Mode: a.Node.Driver().Mode(), Host: wireInfo(info)}
	if a.file.registered() {
		hello.ServerID, hello.Secret = a.file.ServerID, a.file.Secret
	} else {
		hello.Token = a.file.Token
	}
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
	sess, err := yamux.Server(&bufConn{conn, br}, yamuxConfig(a.Logger))
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
		a.Logger.Info("registered with the primary", "server_id", f.ServerID)
	}
	a.file = f
	if a.saved == f {
		return nil
	}
	// Keep the identity in memory when the write fails: the token is used,
	// so the agent cannot register again. The next connection tries again.
	if err := f.write(a.Path); err != nil {
		a.Logger.Error("save the server ID and secret; the agent cannot connect after a restart until this works", "err", err)
		return nil
	}
	a.saved = f
	return nil
}

// serve operates one connection until it ends. After an update, the
// agent does not close the connection: the answer of the update could be
// lost. It waits for the primary to close it.
func (a *agent) serve(ctx context.Context, sess *yamux.Session) error {
	defer sess.Close()
	a.Logger.Info("connected to the primary", "primary", a.file.Primary, "server_id", a.file.ServerID)
	if a.CacheProxy != nil {
		a.CacheProxy.attach(sess)
		defer a.CacheProxy.detach(sess)
	}
	go serveStreams(sess, a.routes, a.Logger)
	go a.sendEvents(sess)
	select {
	case <-ctx.Done():
		return nil
	case <-sess.CloseChan():
		return nil
	case <-a.updated:
	}
	select {
	case <-ctx.Done():
	case <-sess.CloseChan():
	case <-time.After(updateWait):
	}
	return ErrUpdated
}

// sendEvents streams the outbox to POST /v1/events. When the stream
// breaks, it closes the session, because the primary would miss state
// changes; the agent then connects again.
func (a *agent) sendEvents(sess *yamux.Session) {
	defer sess.Close()
	if n := a.out.takeDropped(); n > 0 {
		a.Logger.Warn("dropped events while not connected", "count", n)
	}
	pr, pw := io.Pipe()
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://primary"+eventsPath, pr)
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
			a.Logger.Debug("event stream ended", "err", err)
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
	stats   *hostStats
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
	ws := wireStats(st)
	o.mu.Lock()
	o.stats = &ws
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

// agentFile is /etc/runplane/agent.json. install.sh writes the primary,
// the fingerprint, and the token. After the agent registers, the file has
// the server ID and the secret, and no token.
type agentFile struct {
	Primary     string `json:"primary"`     // host:port of the agent listener
	Fingerprint string `json:"fingerprint"` // sha256:<hex> of the primary certificate
	Token       string `json:"token,omitempty"`
	ServerID    string `json:"server_id"`
	Secret      string `json:"secret"`
}

func readAgentFile(path string) (agentFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return agentFile{}, fmt.Errorf("read agent config: %w", err)
	}
	var f agentFile
	if err := json.Unmarshal(b, &f); err != nil {
		return agentFile{}, fmt.Errorf("read agent config %s: %w", path, err)
	}
	if err := f.validate(); err != nil {
		return agentFile{}, fmt.Errorf("agent config %s: %w", path, err)
	}
	return f, nil
}

func (f agentFile) validate() error {
	if _, _, err := net.SplitHostPort(f.Primary); err != nil {
		return fmt.Errorf("primary %q is not host:port", f.Primary)
	}
	digest, ok := strings.CutPrefix(f.Fingerprint, "sha256:")
	if b, err := hex.DecodeString(digest); !ok || err != nil || len(b) != sha256.Size {
		return fmt.Errorf("fingerprint %q is not sha256:<64 hex digits>", f.Fingerprint)
	}
	if !f.registered() && f.Token == "" {
		return errors.New("no token, and no server_id and secret")
	}
	return nil
}

func (f agentFile) registered() bool { return f.ServerID != "" && f.Secret != "" }

// write replaces the file atomically, with mode 0600.
func (f agentFile) write(path string) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(path, bytes.NewReader(append(b, '\n')), 0o600); err != nil {
		return fmt.Errorf("write agent config: %w", err)
	}
	// The used token cannot register again, so make the rename durable.
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
