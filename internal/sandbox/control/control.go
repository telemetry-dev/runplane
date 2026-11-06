// Package control is the host side of the control channel between runplane
// and runplane-init in a sandbox.
//
// The guest dials a Unix socket (container mode), vsock (VM mode, which
// Firecracker forwards to a Unix socket on the host), or one shared TCP port
// (dev mode, see TCPListener). The guest package has the protocol.
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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// Handler gets what the guest sends. The server calls it from its own
// goroutines; OnLog calls come in the order of the lines.
type Handler interface {
	// OnConnect comes with each new connection of the guest.
	OnConnect()
	OnEvent(sandbox.RunnerEvent)
	OnLog(guest.LogLine)
	// OnLostConfig comes one time when the guest asks for its config, and
	// the server has none (see Listen). The handler stops the sandbox.
	OnLostConfig()
}

var ErrNotConnected = errors.New("guest is not connected")

// Server is the host end of the control channel of one sandbox.
type Server struct {
	cfg *guest.Config
	h   Handler
	log *slog.Logger
	mux *http.ServeMux
	// stopListening closes the Unix socket, or takes the sandbox off the
	// shared TCP listener.
	stopListening func() error
	lost          sync.Once // OnLostConfig

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
//
// cfg is nil for a sandbox from before a runplane restart, because the JIT
// config was only in memory. Its guest asks for its config only one time, so
// it usually has it. A guest that asks then can never run a job: the server
// calls OnLostConfig.
func Listen(socketPath string, cfg *guest.Config, h Handler, log *slog.Logger) (*Server, error) {
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

func newServer(cfg *guest.Config, h Handler, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, h: h, log: log, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET "+guest.PathConfig, s.handleConfig)
	s.mux.HandleFunc("POST "+guest.PathEvents, s.handleEvent)
	s.mux.HandleFunc("POST "+guest.PathLogs, s.handleLogs)
	return s
}

// Hold tells the guest to wait until this time after the runner exits.
func (s *Server) Hold(ctx context.Context, until time.Time) error {
	body, err := json.Marshal(guest.Hold{Until: until.UTC()})
	if err != nil {
		return err
	}
	return s.call(ctx, guest.PathHold, body)
}

// Shutdown tells the guest to stop the runner and power off.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.call(ctx, guest.PathShutdown, nil)
}

// Shell opens a login shell as the runner user in the guest. ctx limits only
// the open; the terminal stays open until Close or until the shell exits.
func (s *Server) Shell(ctx context.Context, size sandbox.TermSize) (sandbox.Terminal, error) {
	g := s.current()
	if g == nil {
		return nil, ErrNotConnected
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://guest"+guest.PathShell+"?"+guest.SizeQuery(size).Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", guest.ShellProtocol)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open shell: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		return nil, fmt.Errorf("open shell: %w", responseError(resp))
	}
	session := resp.Header.Get(guest.SessionHeader)
	return &terminal{
		ReadWriteCloser: resp.Body.(io.ReadWriteCloser),
		resize: func(size sandbox.TermSize) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			q := guest.SizeQuery(size)
			q.Set(guest.QuerySession, session)
			return g.call(ctx, guest.PathShellResize+"?"+q.Encode(), nil)
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

func (s *Server) serve(conn net.Conn) {
	sess, err := yamux.Server(conn, guest.YamuxConfig(s.log))
	if err != nil {
		conn.Close()
		s.log.Warn("start yamux session", "err", err)
		return
	}
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
	s.h.OnConnect()
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
	if s.cfg == nil {
		// The guest asks again each second until the handler stops it.
		s.lost.Do(func() { go s.h.OnLostConfig() })
		http.Error(w, guest.ErrNoConfig.Error(), guest.StatusNoConfig)
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
	if err := guest.ReadLogLines(r.Body, s.h.OnLog); err != nil {
		s.log.Debug("log stream ended", "err", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type terminal struct {
	io.ReadWriteCloser
	resize func(sandbox.TermSize) error
}

func (t *terminal) Resize(size sandbox.TermSize) error { return t.resize(size) }

func responseError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
}
