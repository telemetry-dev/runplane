// Package sshinstall installs the runplane agent on a server over SSH.
package sshinstall

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// LoadOrCreateKey loads the runplane SSH key from path, or makes a new
// ed25519 key there (mode 0600). It returns the signer and the line for
// the authorized_keys file of the target servers.
func LoadOrCreateKey(path string) (ssh.Signer, string, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		if data, err = newKey(path); err != nil {
			return nil, "", err
		}
	default:
		return nil, "", fmt.Errorf("read ssh key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, "", fmt.Errorf("parse ssh key %s: %w", path, err)
	}
	return signer, authorizedKey(signer.PublicKey()) + " runplane", nil
}

func newKey(path string) ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ssh key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "runplane")
	if err != nil {
		return nil, fmt.Errorf("encode ssh key: %w", err)
	}
	data := pem.EncodeToMemory(block)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("make ssh key dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".ssh-key-*")
	if err != nil {
		return nil, fmt.Errorf("write ssh key: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("write ssh key: %w", err)
	}
	// A hard link puts the complete file in place and fails if a key is
	// already there. If another caller made the key first, use that key.
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return os.ReadFile(path)
		}
		return nil, fmt.Errorf("write ssh key: %w", err)
	}
	return data, nil
}

// Target is the SSH address of a server.
type Target struct {
	Host string
	Port int    // 22 if zero
	User string // root if empty
}

// Install connects to the target, runs command with stdin as its input, and
// calls onLine for each line of stdout and stderr. It returns the host key of
// the server in authorized_keys format, or "" if the key was not accepted.
//
// If knownHostKey is empty, Install accepts the key that the server shows
// (trust on first use). Otherwise the server must show the same key.
//
// A command that exits with a status other than 0 gives an error that wraps
// *ssh.ExitError.
func Install(ctx context.Context, t Target, signer ssh.Signer, knownHostKey string, command string, stdin io.Reader, onLine func(string)) (hostKey string, err error) {
	if t.Port == 0 {
		t.Port = 22
	}
	if t.User == "" {
		t.User = "root"
	}
	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))

	config := &ssh.ClientConfig{
		User: t.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			hostKey = authorizedKey(key)
			return nil
		},
	}
	if knownHostKey != "" {
		known, _, _, _, err := ssh.ParseAuthorizedKey([]byte(knownHostKey))
		if err != nil {
			return "", fmt.Errorf("parse known host key: %w", err)
		}
		// Ask for the known key type; else a server with several host keys
		// can show a different one and fail the check.
		config.HostKeyAlgorithms = hostKeyAlgorithms(known.Type())
		config.HostKeyCallback = func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if !bytes.Equal(key.Marshal(), known.Marshal()) {
				return fmt.Errorf("host key %s does not match the known key %s", ssh.FingerprintSHA256(key), ssh.FingerprintSHA256(known))
			}
			hostKey = authorizedKey(key)
			return nil
		}
	}

	dialer := net.Dialer{Timeout: 15 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("connect to %s: %w", addr, err)
	}
	// Closing the connection stops the handshake or the command when ctx ends.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return hostKey, fmt.Errorf("ssh handshake with %s: %w", addr, ctxErr(ctx, err))
	}
	conn.SetDeadline(time.Time{})
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	done := make(chan struct{})
	defer close(done)
	go keepAlive(client, done)

	session, err := client.NewSession()
	if err != nil {
		return hostKey, fmt.Errorf("open ssh session: %w", ctxErr(ctx, err))
	}
	defer session.Close()

	lines := &lineSplitter{onLine: onLine}
	session.Stdin = stdin
	session.Stdout = lines.writer()
	session.Stderr = lines.writer()
	err = session.Run(command)
	lines.flush()
	if err != nil {
		return hostKey, fmt.Errorf("run install command: %w", ctxErr(ctx, err))
	}
	return hostKey, nil
}

// keepAlive sends a request every 30 seconds, so a NAT or firewall does not
// drop the connection while the install script waits for downloads. It
// closes the client if the server does not answer.
func keepAlive(client *ssh.Client, done <-chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				client.Close()
				return
			}
		}
	}
}

func authorizedKey(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func hostKeyAlgorithms(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}

// ctxErr returns the context error if the context ended, because the
// closed connection then gives only "use of closed network connection".
func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// lineSplitter turns stdout and stderr into lines. Each stream has its own
// partial line; onLine calls never overlap.
type lineSplitter struct {
	mu      sync.Mutex
	onLine  func(string)
	streams []*bytes.Buffer
}

func (s *lineSplitter) writer() io.Writer {
	buf := &bytes.Buffer{}
	s.streams = append(s.streams, buf)
	return writerFunc(func(p []byte) (int, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		buf.Write(p)
		for {
			i := bytes.IndexByte(buf.Bytes(), '\n')
			if i < 0 {
				break
			}
			s.emit(buf.Next(i + 1))
		}
		return len(p), nil
	})
}

// flush sends the last line of each stream if it has no newline.
func (s *lineSplitter) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, buf := range s.streams {
		if buf.Len() > 0 {
			s.emit(buf.Next(buf.Len()))
		}
	}
}

func (s *lineSplitter) emit(line []byte) {
	if s.onLine != nil {
		s.onLine(strings.TrimRight(string(line), "\r\n"))
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
