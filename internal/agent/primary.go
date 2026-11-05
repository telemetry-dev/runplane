package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/runplane/runplane/internal/cache"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/runner"
	"github.com/runplane/runplane/internal/sandbox"
)

// ErrClosed means that the connection of the agent has ended.
var ErrClosed = errors.New("agent connection is closed")

// Listener accepts agent connections on the primary.
type Listener struct {
	Cert tls.Certificate // the agent certificate from tlsutil.AgentCert
	// Authenticate checks the hello of an agent and returns its server ID
	// and the secret that the agent must keep. For a known agent, an empty
	// secret keeps the secret that it has. An error refuses the agent, and
	// the agent logs the error text.
	Authenticate func(ctx context.Context, hello Hello) (serverID, secret string, err error)
	// OnSession gets each new session. If the server already has a
	// session, the Listener closes the old one first.
	OnSession func(*Session)
	// CacheHandler serves the Actions cache requests that agents send, with
	// /v1/cache cut from the path: the Proxied handler of the cache server.
	// Nil answers 503.
	CacheHandler http.Handler
	Logger       *slog.Logger

	mu       sync.Mutex
	sessions map[string]*Session
	stopped  bool
}

// Serve accepts TLS connections from agents on ln until ctx ends. Then it
// closes all sessions and returns nil.
func (l *Listener) Serve(ctx context.Context, ln net.Listener) error {
	tln := tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{l.Cert},
		MinVersion:   tls.VersionTLS13,
	})
	defer context.AfterFunc(ctx, func() { tln.Close() })()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := tln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				l.closeAll()
				return nil
			}
			l.Logger.Warn("accept agent connection", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		wg.Go(func() { l.accept(ctx, conn) })
	}
}

func (l *Listener) accept(ctx context.Context, conn net.Conn) {
	s, err := l.open(ctx, conn)
	if err != nil {
		conn.Close()
		l.Logger.Debug("agent connection failed", "remote", conn.RemoteAddr().String(), "err", err)
		return
	}
	old, ok := l.add(s)
	if !ok {
		s.Close()
		return
	}
	if old != nil {
		l.Logger.Info("agent connected again, closing its old connection", "server_id", s.ServerID())
		old.Close()
	}
	l.Logger.Info("agent connected", "server_id", s.ServerID(), "remote", s.hello.Addr, "version", s.hello.Version)
	go func() {
		<-s.Done()
		l.remove(s)
		l.Logger.Info("agent disconnected", "server_id", s.ServerID())
	}()
	l.OnSession(s)
}

// open runs the agent handshake and starts yamux.
func (l *Listener) open(ctx context.Context, conn net.Conn) (*Session, error) {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	conn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := conn.(*tls.Conn).HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("tls handshake: %w", err)
	}
	br := bufio.NewReaderSize(conn, maxLine)
	var hello Hello
	if err := readJSONLine(br, &hello); err != nil {
		return nil, fmt.Errorf("read hello: %w", err)
	}
	hello.Addr = remoteIP(conn.RemoteAddr())
	id, secret, err := l.Authenticate(ctx, hello)
	if err != nil {
		l.Logger.Warn("agent refused", "remote", hello.Addr, "server_id", hello.ServerID, "err", err)
		writeJSONLine(conn, reply{Error: err.Error()})
		return nil, err
	}
	if err := writeJSONLine(conn, reply{OK: true, ServerID: id, Secret: secret}); err != nil {
		return nil, fmt.Errorf("write reply: %w", err)
	}
	conn.SetDeadline(time.Time{})
	sess, err := yamux.Client(&bufConn{conn, br}, yamuxConfig(l.Logger))
	if err != nil {
		return nil, fmt.Errorf("start yamux: %w", err)
	}
	hello.ServerID, hello.Token, hello.Secret = id, "", ""
	return newSession(sess, hello, l.CacheHandler, l.Logger.With("server_id", id)), nil
}

// add keeps s as the session of its server and returns the old session.
// It returns false after Serve has stopped.
func (l *Listener) add(s *Session) (old *Session, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return nil, false
	}
	if l.sessions == nil {
		l.sessions = map[string]*Session{}
	}
	old = l.sessions[s.ServerID()]
	l.sessions[s.ServerID()] = s
	return old, true
}

func (l *Listener) remove(s *Session) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sessions[s.ServerID()] == s {
		delete(l.sessions, s.ServerID())
	}
}

func (l *Listener) closeAll() {
	l.mu.Lock()
	l.stopped = true
	sessions := l.sessions
	l.sessions = nil
	l.mu.Unlock()
	for _, s := range sessions {
		s.Close()
	}
}

// Session is the connection of one agent. It implements Node and the
// sandbox driver of the agent: each call is an HTTP request on a new yamux
// stream. A Session ends when the connection drops; the agent then connects
// again and the Listener makes a new Session.
type Session struct {
	hello  Hello
	sess   *yamux.Session
	client *http.Client
	log    *slog.Logger

	done   chan struct{}
	events chan sandbox.Event
	stats  chan host.Stats
	// Senders on events and stats hold mu for reading, so that the end of
	// the session can close the channels safely.
	mu     sync.RWMutex
	closed bool
}

func newSession(sess *yamux.Session, hello Hello, cache http.Handler, log *slog.Logger) *Session {
	s := &Session{
		hello:  hello,
		sess:   sess,
		client: streamClient(sess),
		log:    log,
		done:   make(chan struct{}),
		events: make(chan sandbox.Event, 256),
		stats:  make(chan host.Stats, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/events", s.handleEvents)
	mux.Handle("/v1/cache/", cacheHandler(cache, hello.ServerID))
	go serveStreams(sess, mux, log)
	go func() {
		<-sess.CloseChan()
		s.finish()
	}()
	return s
}

func (s *Session) ServerID() string { return s.hello.ServerID }

// Hello returns the hello of the agent, without its credentials.
func (s *Session) Hello() Hello { return s.hello }

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Close ends the session. The sandboxes on the agent keep operating.
func (s *Session) Close() error {
	s.sess.Close()
	<-s.done
	return nil
}

func (s *Session) Driver() sandbox.Driver { return s }

// Stats delivers the host samples of the agent. It closes when the session
// ends.
func (s *Session) Stats() <-chan host.Stats { return s.stats }

func (s *Session) Facts(ctx context.Context) (host.Info, error) {
	var rep hostReport
	err := s.call(ctx, http.MethodGet, "/v1/host", nil, &rep)
	return rep.Info, err
}

func (s *Session) Doctor(ctx context.Context, minFreeDiskPercent float64) ([]host.Check, error) {
	var rep hostReport
	q := url.Values{"doctor": {"1"}, "min_free_disk_percent": {strconv.FormatFloat(minFreeDiskPercent, 'g', -1, 64)}}
	err := s.call(ctx, http.MethodGet, "/v1/host?"+q.Encode(), nil, &rep)
	return rep.Doctor, err
}

func (s *Session) EnsureImage(ctx context.Context, ref string, progress func(image.Progress)) (sandbox.ImageRef, error) {
	resp, err := s.send(ctx, http.MethodPost, "/v1/images/ensure", sandbox.ImageRef{Ref: ref})
	if err != nil {
		return sandbox.ImageRef{}, err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var m imageMessage
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return sandbox.ImageRef{}, fmt.Errorf("ensure image %s on server %s: %w", ref, s.ServerID(), s.closedOr(err))
		}
		switch {
		case m.Error != "":
			return sandbox.ImageRef{}, fmt.Errorf("server %s: %s", s.ServerID(), m.Error)
		case m.Image != nil:
			return *m.Image, nil
		case m.Progress != nil && progress != nil:
			progress(*m.Progress)
		}
	}
}

func (s *Session) EnsureRunner(ctx context.Context, version, arch string) (runner.Bundle, error) {
	var b runner.Bundle
	err := s.call(ctx, http.MethodPost, "/v1/runners/ensure", runnerRequest{Version: version, Arch: arch}, &b)
	return b, err
}

func (s *Session) GCImages(ctx context.Context, keep []string) error {
	return s.call(ctx, http.MethodPost, "/v1/images/gc", gcRequest{Keep: keep}, nil)
}

func (s *Session) PruneRunners(ctx context.Context, keep []string) error {
	return s.call(ctx, http.MethodPost, "/v1/runners/prune", gcRequest{Keep: keep}, nil)
}

// MirrorStats returns the stats of the registry mirror of the agent.
func (s *Session) MirrorStats(ctx context.Context) (mirror.Stats, error) {
	var m mirror.Stats
	err := s.call(ctx, http.MethodGet, "/v1/mirror/stats", nil, &m)
	return m, err
}

// Update makes the agent replace its binary and restart. The session then
// ends, and the agent connects again with the new version.
func (s *Session) Update(ctx context.Context, version string) error {
	return s.call(ctx, http.MethodPost, "/v1/update", updateRequest{Version: version}, nil)
}

// call sends a request and decodes the JSON result into out, if out is not nil.
func (s *Session) call(ctx context.Context, method, path string, in, out any) error {
	resp, err := s.send(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s on server %s: read result: %w", method, path, s.ServerID(), s.closedOr(err))
	}
	return nil
}

// send sends a request with in as the JSON body, if in is not nil.
func (s *Session) send(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.do(req)
}

// do sends req to the agent. It returns an error for a status of 300 or more.
func (s *Session) do(req *http.Request) (*http.Response, error) {
	fail := func(err error) error {
		return fmt.Errorf("%s %s on server %s: %w", req.Method, req.URL.Path, s.ServerID(), err)
	}
	if s.isDone() {
		return nil, fail(ErrClosed)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fail(s.closedOr(err))
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		err := responseError(resp)
		if resp.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%w: %w", sandbox.ErrNotFound, err)
		}
		return nil, fail(err)
	}
	return resp, nil
}

// closedOr returns ErrClosed if the session has ended, else err.
func (s *Session) closedOr(err error) error {
	if s.isDone() {
		return ErrClosed
	}
	return err
}

func (s *Session) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// handleEvents reads the event stream of the agent.
func (s *Session) handleEvents(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(r.Body)
	for {
		var it streamItem
		if err := dec.Decode(&it); err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Debug("event stream ended", "err", err)
			}
			return
		}
		if !s.deliver(it) {
			return
		}
	}
}

// deliver passes an item of the event stream on. It waits while the events
// channel is full, so that no event gets lost, and returns false when the
// session ends.
func (s *Session) deliver(it streamItem) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false
	}
	if it.Stats != nil {
		sendLatest(s.stats, *it.Stats)
	}
	if it.Event != nil {
		select {
		case s.events <- *it.Event:
		case <-s.done:
			return false
		}
	}
	return true
}

func (s *Session) finish() {
	close(s.done)
	s.mu.Lock()
	s.closed = true
	close(s.events)
	close(s.stats)
	s.mu.Unlock()
	s.client.CloseIdleConnections()
}

// cacheHandler serves ANY /v1/cache/* for one agent.
func cacheHandler(h http.Handler, serverID string) http.Handler {
	if h == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "the Actions cache is not available", http.StatusServiceUnavailable)
		})
	}
	h = http.StripPrefix("/v1/cache", h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(cache.ServerHeader, serverID)
		h.ServeHTTP(w, r)
	})
}
