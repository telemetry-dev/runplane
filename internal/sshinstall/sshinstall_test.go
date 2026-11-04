package sshinstall

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// server is an in-process SSH server. It accepts one client key, runs no
// real commands, and records the exec command and stdin of each session.
type server struct {
	addr    string
	hostKey ssh.Signer

	// reply runs for each exec request: it gets stdin and writes output.
	reply func(stdin []byte, stdout, stderr io.Writer) uint32

	mu       sync.Mutex
	commands []string
	stdins   [][]byte
}

func newServer(t *testing.T, clientKey ssh.PublicKey) *server {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hostKey, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{hostKey: hostKey}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() == "deploy" && bytes.Equal(key.Marshal(), clientKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	config.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s.addr = ln.Addr().String()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn, config)
		}
	}()
	return s
}

func (s *server) handle(conn net.Conn, config *ssh.ServerConfig) {
	defer conn.Close()
	_, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			return
		}
		go s.session(ch, reqs)
	}
}

func (s *server) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		ssh.Unmarshal(req.Payload, &payload)
		req.Reply(true, nil)

		stdin, _ := io.ReadAll(ch)
		s.mu.Lock()
		s.commands = append(s.commands, payload.Command)
		s.stdins = append(s.stdins, stdin)
		s.mu.Unlock()

		status := s.reply(stdin, ch, ch.Stderr())
		ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, status))
		return
	}
}

func (s *server) target() Target {
	host, port, _ := net.SplitHostPort(s.addr)
	p, _ := strconv.Atoi(port)
	return Target{Host: host, Port: p, User: "deploy"}
}

func (s *server) hostKeyLine() string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.hostKey.PublicKey())))
}

func testKey(t *testing.T) ssh.Signer {
	t.Helper()
	signer, _, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestInstall(t *testing.T) {
	key := testKey(t)
	srv := newServer(t, key.PublicKey())
	srv.reply = func(stdin []byte, stdout, stderr io.Writer) uint32 {
		io.WriteString(stdout, "Installing Docker\nInstalling Sysbox\r\n")
		io.WriteString(stderr, "warning: swap is on\n")
		io.WriteString(stdout, "Agent started") // no final newline
		return 0
	}

	var lines []string
	script := "#!/bin/sh\necho install\n"
	hostKey, err := Install(context.Background(), srv.target(), key, "", "sudo -n sh -s -- --token rp_srv_x", strings.NewReader(script), func(l string) {
		lines = append(lines, l)
	})
	if err != nil {
		t.Fatal(err)
	}
	if hostKey != srv.hostKeyLine() {
		t.Fatalf("host key %q, want %q", hostKey, srv.hostKeyLine())
	}
	if want := []string{"sudo -n sh -s -- --token rp_srv_x"}; !slices.Equal(srv.commands, want) {
		t.Fatalf("server ran %q", srv.commands)
	}
	if string(srv.stdins[0]) != script {
		t.Fatalf("server got stdin %q", srv.stdins[0])
	}
	// Stdout and stderr arrive on separate streams, so only the order inside
	// each stream is fixed.
	slices.Sort(lines)
	want := []string{"Agent started", "Installing Docker", "Installing Sysbox", "warning: swap is on"}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines %q, want %q", lines, want)
	}
}

func TestInstallExitStatus(t *testing.T) {
	key := testKey(t)
	srv := newServer(t, key.PublicKey())
	srv.reply = func(_ []byte, _, stderr io.Writer) uint32 {
		io.WriteString(stderr, "this host has no systemd\n")
		return 3
	}
	var lines []string
	_, err := Install(context.Background(), srv.target(), key, "", "sh -s", strings.NewReader(""), func(l string) { lines = append(lines, l) })
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 3 {
		t.Fatalf("err = %v, want exit status 3", err)
	}
	if !slices.Equal(lines, []string{"this host has no systemd"}) {
		t.Fatalf("lines %q", lines)
	}
}

func TestInstallHostKey(t *testing.T) {
	key := testKey(t)
	srv := newServer(t, key.PublicKey())
	srv.reply = func([]byte, io.Writer, io.Writer) uint32 { return 0 }

	other := newServer(t, key.PublicKey()).hostKeyLine()
	tests := []struct {
		name    string
		known   string
		wantKey string
		wantErr bool
	}{
		{"first use", "", srv.hostKeyLine(), false},
		{"same key", srv.hostKeyLine(), srv.hostKeyLine(), false},
		{"changed key", other, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv.mu.Lock()
			ran := len(srv.commands)
			srv.mu.Unlock()

			hostKey, err := Install(context.Background(), srv.target(), key, tt.known, "true", nil, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if hostKey != tt.wantKey {
				t.Fatalf("host key %q, want %q", hostKey, tt.wantKey)
			}
			srv.mu.Lock()
			defer srv.mu.Unlock()
			if tt.wantErr && len(srv.commands) != ran {
				t.Fatal("command ran on a server with a changed host key")
			}
		})
	}
}

func TestInstallWrongClientKey(t *testing.T) {
	srv := newServer(t, testKey(t).PublicKey())
	srv.reply = func([]byte, io.Writer, io.Writer) uint32 { return 0 }
	if _, err := Install(context.Background(), srv.target(), testKey(t), "", "true", nil, nil); err == nil {
		t.Fatal("Install with a key that the server does not accept succeeded")
	}
}

func TestInstallCancel(t *testing.T) {
	key := testKey(t)
	srv := newServer(t, key.PublicKey())
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	srv.reply = func([]byte, io.Writer, io.Writer) uint32 {
		<-block
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := Install(ctx, srv.target(), key, "", "sleep 600", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh", "id_ed25519")
	signer1, line, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	signer2, _, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(signer1.PublicKey().Marshal(), signer2.PublicKey().Marshal()) {
		t.Fatal("second load made a new key; servers with the old authorized_keys line would refuse it")
	}
	if !strings.HasPrefix(line, "ssh-ed25519 AAAA") || !strings.HasSuffix(line, " runplane") {
		t.Fatalf("authorized_keys line %q", line)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || !bytes.Equal(pub.Marshal(), signer1.PublicKey().Marshal()) {
		t.Fatalf("authorized_keys line does not parse to the key: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("key mode %o, want 600", mode)
	}
}

func TestLoadOrCreateKeyConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")
	keys := make([][]byte, 8)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Go(func() {
			signer, _, err := LoadOrCreateKey(path)
			if err != nil {
				t.Error(err)
				return
			}
			keys[i] = signer.PublicKey().Marshal()
		})
	}
	wg.Wait()
	for _, k := range keys[1:] {
		if !bytes.Equal(k, keys[0]) {
			t.Fatal("concurrent callers got different keys")
		}
	}
}
