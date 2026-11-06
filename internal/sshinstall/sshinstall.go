// Package sshinstall installs the runplane agent on a server over SSH.
package sshinstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type Target struct {
	Addr string // host:port
	User string
}

// KnownHosts is a known_hosts file with the host keys of the servers that
// Install connected to.
type KnownHosts struct {
	Path string
	mu   sync.Mutex
}

// HostKeyError means that a server showed a host key that is not the key
// that it showed at its first connection.
type HostKeyError struct {
	Addr       string
	Key, Known ssh.PublicKey
	File       string // the known_hosts file
}

func (e *HostKeyError) Error() string {
	return fmt.Sprintf("host key %s of %s does not match the known key %s. If the server was reinstalled, remove its line from %s",
		ssh.FingerprintSHA256(e.Key), e.Addr, ssh.FingerprintSHA256(e.Known), e.File)
}

// key returns the host key of addr, or nil if Install has not connected to
// addr before.
func (k *KnownHosts) key(addr string) (ssh.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	data, err := os.ReadFile(k.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read known hosts: %w", err)
	}
	want := knownhosts.Normalize(addr)
	for {
		_, hosts, key, _, rest, err := ssh.ParseKnownHosts(data)
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read known hosts %s: %w", k.Path, err)
		}
		if slices.Contains(hosts, want) {
			return key, nil
		}
		data = rest
	}
}

func (k *KnownHosts) add(addr string, key ssh.PublicKey) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	file, err := os.OpenFile(k.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("save the host key: %w", err)
	}
	_, err = file.WriteString(knownhosts.Line([]string{addr}, key) + "\n")
	if err = errors.Join(err, file.Close()); err != nil {
		return fmt.Errorf("save the host key: %w", err)
	}
	return nil
}

// Install connects to the target, runs command with stdin as its input, and
// calls onLine for each line of stdout and stderr. The onLine calls never
// overlap, and they end before Install returns.
//
// The server must show the host key that known has for it. At the first
// connection, known has no key: Install accepts the key that the server
// shows (trust on first use), and adds it to known before the command runs.
// A key that does not match gives a *HostKeyError.
//
// A command that exits with a status other than 0 gives an error that wraps
// *ssh.ExitError.
func Install(ctx context.Context, t Target, signer ssh.Signer, known *KnownHosts, command string, stdin io.Reader, onLine func(string)) error {
	knownKey, err := known.key(t.Addr)
	if err != nil {
		return err
	}
	config := &ssh.ClientConfig{
		User: t.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if knownKey == nil {
				return known.add(t.Addr, key)
			}
			if !bytes.Equal(key.Marshal(), knownKey.Marshal()) {
				return &HostKeyError{Addr: t.Addr, Key: key, Known: knownKey, File: known.Path}
			}
			return nil
		},
	}
	if knownKey != nil {
		// Ask for the known key type; else a server with several host keys
		// can show a different one and fail the check.
		config.HostKeyAlgorithms = hostKeyAlgorithms(knownKey.Type())
	}

	dialer := net.Dialer{Timeout: 15 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", t.Addr)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", t.Addr, err)
	}
	// Closing the connection stops the handshake or the command when ctx ends.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	c, chans, reqs, err := ssh.NewClientConn(conn, t.Addr, config)
	if err != nil {
		conn.Close()
		return fmt.Errorf("ssh handshake with %s: %w", t.Addr, ctxErr(ctx, err))
	}
	conn.SetDeadline(time.Time{})
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	done := make(chan struct{})
	defer close(done)
	go keepAlive(client, done)

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open ssh session: %w", ctxErr(ctx, err))
	}
	defer session.Close()

	var mu sync.Mutex
	stdout, stderr := &lineWriter{mu: &mu, onLine: onLine}, &lineWriter{mu: &mu, onLine: onLine}
	session.Stdin, session.Stdout, session.Stderr = stdin, stdout, stderr
	err = session.Run(command)
	stdout.flush()
	stderr.flush()
	if err != nil {
		return fmt.Errorf("run install command: %w", ctxErr(ctx, err))
	}
	return nil
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

// lineWriter splits a stream into lines for onLine. The writers of one
// session share mu, so that the onLine calls do not overlap.
type lineWriter struct {
	mu     *sync.Mutex
	onLine func(string)
	buf    bytes.Buffer
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			return len(p), nil
		}
		w.emit(w.buf.Next(i + 1))
	}
}

// flush sends the last line if it has no newline.
func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.emit(w.buf.Next(w.buf.Len()))
	}
}

func (w *lineWriter) emit(line []byte) {
	w.onLine(strings.TrimRight(string(line), "\r\n"))
}
