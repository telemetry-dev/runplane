package control

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/atomicfile"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// TCPListener serves the control channels of all sandboxes on one TCP port.
// Dev mode uses it (see guest.AddrFile). Each connection starts with the
// hello line of a sandbox; a wrong token closes it.
type TCPListener struct {
	ln   net.Listener
	dial string // the address that guests dial
	log  *slog.Logger

	mu        sync.Mutex
	sandboxes map[string]tcpSandbox
}

type tcpSandbox struct {
	token string
	srv   *Server
}

// ListenTCP listens on addr. Guests reach the listener at dialHost and the
// port that it got, for example host.docker.internal:8487.
func ListenTCP(addr, dialHost string, log *slog.Logger) (*TCPListener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for control connections: %w", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	t := &TCPListener{
		ln:        ln,
		dial:      net.JoinHostPort(dialHost, port),
		log:       log,
		sandboxes: map[string]tcpSandbox{},
	}
	go t.accept()
	return t, nil
}

// Listen serves the control channel of the sandbox id, as the function
// Listen does. It writes a new token to the guest.AddrFile in ctlDir, so
// after a runplane restart the guest gets in with the new token.
// Server.Close takes the sandbox off the listener.
func (t *TCPListener) Listen(ctlDir, id string, cfg *guest.Config, h Handler, log *slog.Logger) (*Server, error) {
	if id == "" || strings.ContainsAny(id, " \n") {
		return nil, fmt.Errorf("listen for sandbox %q: bad sandbox ID", id)
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	line := guest.FormatAddr(t.dial, id, token)
	if err := atomicfile.Write(filepath.Join(ctlDir, guest.AddrFile), strings.NewReader(line), 0o644); err != nil {
		return nil, fmt.Errorf("write control address: %w", err)
	}

	s := newServer(cfg, h, log)
	s.stopListening = func() error {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.sandboxes[id].srv == s {
			delete(t.sandboxes, id)
		}
		return nil
	}
	t.mu.Lock()
	t.sandboxes[id] = tcpSandbox{token: token, srv: s}
	t.mu.Unlock()
	return s, nil
}

// Close stops new connections. Connections that a Server took stay until
// that Server closes.
func (t *TCPListener) Close() error {
	err := t.ln.Close()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (t *TCPListener) accept() {
	for {
		conn, err := t.ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			t.log.Warn("accept control connection", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go t.handshake(conn)
	}
}

func (t *TCPListener) handshake(conn net.Conn) {
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := readHello(conn)
	var srv *Server
	if err == nil {
		srv, err = t.check(line)
	}
	if err != nil {
		t.log.Warn("refuse control connection", "remote", conn.RemoteAddr().String(), "err", err)
		conn.Close()
		return
	}
	conn.SetDeadline(time.Time{})
	srv.serve(conn)
}

// check finds the sandbox of a hello line.
func (t *TCPListener) check(line string) (*Server, error) {
	id, token, err := guest.ParseHello(line)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	sb, ok := t.sandboxes[id]
	t.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown sandbox %q", id)
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(sb.token)) != 1 {
		return nil, fmt.Errorf("wrong token for sandbox %s", id)
	}
	return sb.srv, nil
}

// readHello reads one line byte by byte, so that no yamux bytes after it
// stay in a buffer.
func readHello(r io.Reader) (string, error) {
	var line []byte
	b := make([]byte, 1)
	for len(line) < 256 {
		if _, err := io.ReadFull(r, b); err != nil {
			return "", fmt.Errorf("read hello line: %w", err)
		}
		if b[0] == '\n' {
			return string(line), nil
		}
		line = append(line, b[0])
	}
	return "", errors.New("hello line too long")
}
