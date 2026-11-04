package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/creack/pty"

	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/sandbox"
)

// shellServer serves debug shells. Each shell is a login shell on a PTY.
type shellServer struct {
	log *slog.Logger

	mu    sync.Mutex
	start func(sandbox.TermSize) (*shell, error) // nil until init knows the runner user
	open  map[string]*os.File                    // session ID to PTY
}

type shell struct {
	pty  *os.File
	stop func() // hangs up the shell and waits until it exits
}

func newShellServer(log *slog.Logger) *shellServer {
	return &shellServer{log: log, open: map[string]*os.File{}}
}

func (s *shellServer) setStart(start func(sandbox.TermSize) (*shell, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.start = start
}

// handleOpen starts a shell and switches the stream to raw PTY bytes. The
// stream ends when the shell exits; the shell ends when the host closes the stream.
func (s *shellServer) handleOpen(w http.ResponseWriter, r *http.Request) {
	size, err := termSize(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	start := s.start
	s.mu.Unlock()
	if start == nil {
		http.Error(w, "the shell is not ready yet", http.StatusServiceUnavailable)
		return
	}
	sh, err := start(size)
	if err != nil {
		s.log.Warn("start debug shell", "err", err)
		http.Error(w, "start shell: "+err.Error(), http.StatusInternalServerError)
		return
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		sh.stop()
		http.Error(w, "hijack: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	id := ids.New()
	s.mu.Lock()
	s.open[id] = sh.pty
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.open, id)
		s.mu.Unlock()
		sh.stop()
	}()
	s.log.Info("debug shell opened", "session", id)

	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nX-Runplane-Session: %s\r\nConnection: Upgrade\r\nUpgrade: runplane-shell\r\n\r\n", id)
	if err := rw.Flush(); err != nil {
		return
	}
	go func() {
		io.Copy(conn, sh.pty) // ends when the shell exits
		conn.Close()
	}()
	io.Copy(sh.pty, rw.Reader) // ends when the host closes the stream
	s.log.Info("debug shell closed", "session", id)
}

func (s *shellServer) handleResize(w http.ResponseWriter, r *http.Request) {
	size, err := termSize(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	p := s.open[r.URL.Query().Get("session")]
	s.mu.Unlock()
	if p == nil {
		http.Error(w, "no such shell session", http.StatusNotFound)
		return
	}
	if err := pty.Setsize(p, &pty.Winsize{Rows: size.Rows, Cols: size.Cols}); err != nil {
		http.Error(w, "resize: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// termSize reads rows and cols from the query. A missing or zero value
// gives 24 rows and 80 columns.
func termSize(r *http.Request) (sandbox.TermSize, error) {
	size := sandbox.TermSize{Rows: 24, Cols: 80}
	for name, v := range map[string]*uint16{"rows": &size.Rows, "cols": &size.Cols} {
		q := r.URL.Query().Get(name)
		if q == "" {
			continue
		}
		n, err := strconv.ParseUint(q, 10, 16)
		if err != nil {
			return size, fmt.Errorf("bad %s %q", name, q)
		}
		if n > 0 {
			*v = uint16(n)
		}
	}
	return size, nil
}
