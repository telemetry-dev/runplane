package control

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AddrFile is the file in the ctl folder that sends the guest to TCP. Its
// one line is "tcp://<host:port> <sandbox-id> <token>".
const AddrFile = "control.addr"

// TCPListener serves the control channels of all sandboxes on one TCP port.
// Dev mode uses it, because a Unix socket in a bind mount does not cross the
// VM of Docker Desktop or OrbStack. Each connection starts with the line
// "RUNPLANE <sandbox-id> <token>\n"; a wrong token closes it.
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

// Listen serves the control channel of the sandbox cfg.SandboxID. It writes
// a new token to <ctlDir>/control.addr, so after a runplane restart the guest
// gets in with the new token. Server.Close takes the sandbox off the listener.
func (t *TCPListener) Listen(ctlDir string, cfg GuestConfig, h Handler, log *slog.Logger) (*Server, error) {
	id := cfg.SandboxID
	if id == "" || strings.ContainsAny(id, " \n") {
		return nil, fmt.Errorf("listen for sandbox %q: bad sandbox ID", id)
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	line := "tcp://" + t.dial + " " + id + " " + token + "\n"
	if err := writeFileAtomic(filepath.Join(ctlDir, AddrFile), []byte(line), 0o644); err != nil {
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

// check finds the sandbox of a hello line "RUNPLANE <sandbox-id> <token>".
func (t *TCPListener) check(line string) (*Server, error) {
	f := strings.Fields(line)
	if len(f) != 3 || f[0] != "RUNPLANE" {
		return nil, errors.New("bad hello line")
	}
	t.mu.Lock()
	sb, ok := t.sandboxes[f[1]]
	t.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown sandbox %q", f[1])
	}
	if subtle.ConstantTimeCompare([]byte(f[2]), []byte(sb.token)) != 1 {
		return nil, fmt.Errorf("wrong token for sandbox %s", f[1])
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

// ParseAddr reads the line of a control.addr file. It returns the TCP
// address to dial and the hello line to send first.
func ParseAddr(content []byte) (addr, hello string, err error) {
	f := strings.Fields(string(bytes.TrimSpace(content)))
	if len(f) != 3 || !strings.HasPrefix(f[0], "tcp://") {
		return "", "", fmt.Errorf("bad control address %q", content)
	}
	return strings.TrimPrefix(f[0], "tcp://"), "RUNPLANE " + f[1] + " " + f[2] + "\n", nil
}

// writeFileAtomic replaces path, so that a reader never sees half a file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), mode)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	return err
}
