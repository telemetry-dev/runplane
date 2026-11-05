// Package control is the host side of the control channel between runplane
// and runplane-init in a sandbox.
//
// The guest dials a Unix socket (container mode), vsock (VM mode, which
// Firecracker forwards to a Unix socket on the host), or one shared TCP port
// (dev mode, see TCPListener). Both sides use yamux on
// the connection, and each side serves HTTP/1.1 on the streams that the other
// side opens.
package control

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
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/runplane/runplane/internal/sandbox"
)

// GuestConfig is the job config that runplane-init reads one time at start.
type GuestConfig struct {
	SandboxID        string            `json:"sandbox_id"`
	Hostname         string            `json:"hostname"`
	JITConfig        string            `json:"jit_config"`
	RunnerDir        string            `json:"runner_dir"` // read-only runner bundle in the guest
	Env              map[string]string `json:"env"`        // written to the runner .env
	DNS              []string          `json:"dns"`
	RegistryMirror   string            `json:"registry_mirror"` // http://<service-ip>:<port>, empty = none
	TimeLimitSeconds int               `json:"time_limit_seconds"`
	Debug            bool              `json:"debug"`
}

// LogLine is one line of sandbox output.
type LogLine struct {
	Time   time.Time
	Source string // runner, diag, init, or dockerd
	Text   string
}

// Handler gets what the guest sends. The server calls it from its own
// goroutines; OnLog calls come in the order of the lines.
type Handler interface {
	OnEvent(sandbox.RunnerEvent)
	OnLog(LogLine)
	// OnLostConfig comes one time when the guest asks for its config, but
	// the config has no JIT config. The JIT config is only in memory, so
	// runplane restarted before the guest asked, and the sandbox can never
	// run a job. The handler stops the sandbox.
	OnLostConfig()
}

// ErrNotConnected means that no guest connection is open at the moment.
var ErrNotConnected = errors.New("guest is not connected")

// MaxLine is the longest line that the log stream carries. Longer lines are split.
const MaxLine = 64 << 10

// Server is the host end of the control channel of one sandbox.
type Server struct {
	cfg GuestConfig
	h   Handler
	log *slog.Logger
	mux *http.ServeMux
	// stopListening closes the Unix socket, or takes the sandbox off the
	// shared TCP listener.
	stopListening func() error

	lost sync.Once // OnLostConfig

	mu     sync.Mutex
	guest  *guestConn // nil when no guest is connected
	closed bool
}

type guestConn struct {
	sess   *yamux.Session
	client *http.Client
}

// Listen serves the control channel on a Unix socket. It removes an old socket
// file first, so it also works after a runplane restart. A new guest
// connection replaces the old one.
func Listen(socketPath string, cfg GuestConfig, h Handler, log *slog.Logger) (*Server, error) {
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("remove old control socket: %w", err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on control socket: %w", err)
	}
	// The guest end connects with a different uid: a mapped root in container
	// mode and the jailer user in VM mode. The sandbox folder (0700) protects
	// the socket on the host.
	if err := os.Chmod(socketPath, 0o666); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod control socket: %w", err)
	}
	s := newServer(cfg, h, log)
	s.stopListening = ln.Close
	go s.accept(ln)
	return s, nil
}

func newServer(cfg GuestConfig, h Handler, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, h: h, log: log, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /v1/config", s.handleConfig)
	s.mux.HandleFunc("POST /v1/events", s.handleEvent)
	s.mux.HandleFunc("POST /v1/logs", s.handleLogs)
	return s
}

// Connected reports if a guest connection is open.
func (s *Server) Connected() bool {
	return s.current() != nil
}

// Hold tells the guest to wait until this time after the runner exits.
func (s *Server) Hold(ctx context.Context, until time.Time) error {
	body, err := json.Marshal(struct {
		Until time.Time `json:"until"`
	}{until.UTC()})
	if err != nil {
		return err
	}
	return s.call(ctx, "/v1/hold", body)
}

// Shutdown tells the guest to stop the runner and power off.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.call(ctx, "/v1/shutdown", nil)
}

// Shell opens a login shell as the runner user in the guest. ctx limits only
// the open; the terminal stays open until Close or until the shell exits.
func (s *Server) Shell(ctx context.Context, size sandbox.TermSize) (sandbox.Terminal, error) {
	g := s.current()
	if g == nil {
		return nil, ErrNotConnected
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://guest/v1/shell?"+sizeQuery(size).Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "runplane-shell")
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open shell: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		return nil, fmt.Errorf("open shell: %w", responseError(resp))
	}
	session := resp.Header.Get("X-Runplane-Session")
	return &terminal{
		ReadWriteCloser: resp.Body.(io.ReadWriteCloser),
		resize: func(size sandbox.TermSize) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			q := sizeQuery(size)
			q.Set("session", session)
			return g.call(ctx, "/v1/shell/resize?"+q.Encode(), nil)
		},
	}, nil
}

// Close stops listening and drops the guest connection. The guest keeps
// trying to connect, so a new Listen on the same socket picks it up again.
func (s *Server) Close() error {
	s.mu.Lock()
	g := s.guest
	s.guest = nil
	s.closed = true
	s.mu.Unlock()
	if g != nil {
		g.sess.Close()
	}
	err := s.stopListening()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (s *Server) accept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			s.log.Warn("accept guest connection", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.serve(conn)
	}
}

// serve starts yamux on a new guest connection.
func (s *Server) serve(conn net.Conn) {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = nil
	cfg.Logger = slog.NewLogLogger(s.log.Handler(), slog.LevelDebug)
	sess, err := yamux.Server(conn, cfg)
	if err != nil {
		conn.Close()
		s.log.Warn("start yamux session", "err", err)
		return
	}
	s.attach(sess)
}

func (s *Server) attach(sess *yamux.Session) {
	g := &guestConn{sess: sess, client: &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return sess.Open() },
	}}}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		sess.Close()
		return
	}
	old := s.guest
	s.guest = g
	s.mu.Unlock()
	if old != nil {
		s.log.Info("guest connected again, dropping the old connection")
		old.sess.Close()
	} else {
		s.log.Debug("guest connected")
	}
	go func() {
		srv := &http.Server{
			Handler:           s.mux,
			ReadHeaderTimeout: 10 * time.Second,
			MaxHeaderBytes:    16 << 10,
			ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
		}
		srv.Serve(sess) // returns when the session ends
		s.mu.Lock()
		if s.guest == g {
			s.guest = nil
		}
		s.mu.Unlock()
		s.log.Debug("guest disconnected")
	}()
}

func (s *Server) current() *guestConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.guest == nil || s.guest.sess.IsClosed() {
		return nil
	}
	return s.guest
}

func (s *Server) call(ctx context.Context, path string, body []byte) error {
	g := s.current()
	if g == nil {
		return ErrNotConnected
	}
	return g.call(ctx, path, body)
}

// call sends a POST to the guest and expects 204.
func (g *guestConn) call(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://guest"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("call guest %s: %w", req.URL.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("call guest %s: %w", req.URL.Path, responseError(resp))
	}
	return nil
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.JITConfig == "" {
		// The guest asks again each second until the handler stops it.
		s.lost.Do(func() { go s.h.OnLostConfig() })
		http.Error(w, "runplane restarted before this sandbox got its config", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.cfg)
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	var ev sandbox.RunnerEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&ev); err != nil || ev.Kind == "" {
		http.Error(w, "bad runner event", http.StatusBadRequest)
		return
	}
	s.h.OnEvent(ev)
	w.WriteHeader(http.StatusNoContent)
}

// handleLogs reads the log stream until the guest ends it. The stream breaks
// when the connection drops; the guest then streams again on a new connection.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	// The prefix "<unix-ms> <source> " comes on top of MaxLine.
	err := readLines(r.Body, MaxLine+64, func(line string) { s.h.OnLog(parseLogLine(line)) })
	if err != nil {
		s.log.Debug("log stream ended", "err", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseLogLine reads "<unix-ms> <source> <text>". It keeps a line in another
// form as text with no source.
func parseLogLine(line string) LogLine {
	ms, rest, ok := strings.Cut(line, " ")
	source, text, _ := strings.Cut(rest, " ")
	n, err := strconv.ParseInt(ms, 10, 64)
	if !ok || err != nil || source == "" {
		return LogLine{Time: time.Now().UTC(), Text: line}
	}
	return LogLine{Time: time.UnixMilli(n).UTC(), Source: source, Text: text}
}

type terminal struct {
	io.ReadWriteCloser
	resize func(sandbox.TermSize) error
}

func (t *terminal) Resize(size sandbox.TermSize) error { return t.resize(size) }

func sizeQuery(size sandbox.TermSize) url.Values {
	return url.Values{
		"rows": {strconv.Itoa(int(size.Rows))},
		"cols": {strconv.Itoa(int(size.Cols))},
	}
}

func responseError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
}
