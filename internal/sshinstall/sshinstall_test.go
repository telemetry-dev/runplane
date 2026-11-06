package sshinstall

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/runplane/runplane/internal/sshinstall/sshtest"
	"github.com/runplane/runplane/internal/sshkey"
)

func testKey(t *testing.T) ssh.Signer {
	t.Helper()
	signer, _, err := sshkey.LoadOrCreate(filepath.Join(t.TempDir(), "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func newKnownHosts(t *testing.T) *KnownHosts {
	return &KnownHosts{Path: filepath.Join(t.TempDir(), "known_hosts")}
}

func TestInstall(t *testing.T) {
	key := testKey(t)
	known := newKnownHosts(t)
	// The command starts the agent, which can connect before Install
	// returns. Then runplane must already trust the server.
	var trustedAtRun atomic.Bool
	srv := sshtest.NewServer(t, key.PublicKey(), func(_ string, stdin []byte, stdout, stderr io.Writer) uint32 {
		b, _ := os.ReadFile(known.Path)
		trustedAtRun.Store(len(b) > 0)
		stdout.Write(stdin)
		io.WriteString(stdout, "Installing Docker\nInstalling Sysbox\r\n")
		io.WriteString(stderr, "warning: swap is on\n")
		io.WriteString(stdout, "Agent started") // no final newline
		return 0
	})
	var lines []string
	err := Install(t.Context(), Target{srv.Addr, "deploy"}, key, known, "sudo -n sh -s -- --token rp_srv_x", strings.NewReader("#!/bin/sh\necho install\n"), func(l string) {
		lines = append(lines, l)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !trustedAtRun.Load() {
		t.Error("the host key was not in known_hosts when the command ran")
	}
	if got := srv.Commands(); !slices.Equal(got, []string{"sudo -n sh -s -- --token rp_srv_x"}) {
		t.Errorf("server ran %q", got)
	}
	// Stdout and stderr arrive on separate streams, so only the order inside
	// each stream is fixed.
	slices.Sort(lines)
	want := []string{"#!/bin/sh", "Agent started", "Installing Docker", "Installing Sysbox", "echo install", "warning: swap is on"}
	if !slices.Equal(lines, want) {
		t.Errorf("lines %q, want %q", lines, want)
	}
}

func TestInstallExitStatus(t *testing.T) {
	key := testKey(t)
	srv := sshtest.NewServer(t, key.PublicKey(), func(_ string, _ []byte, _, stderr io.Writer) uint32 {
		io.WriteString(stderr, "this host has no systemd\n")
		return 3
	})
	var lines []string
	err := Install(t.Context(), Target{srv.Addr, "deploy"}, key, newKnownHosts(t), "sh -s", strings.NewReader(""), func(l string) { lines = append(lines, l) })
	if exit, ok := errors.AsType[*ssh.ExitError](err); !ok || exit.ExitStatus() != 3 {
		t.Errorf("err = %v, want exit status 3", err)
	}
	if !slices.Equal(lines, []string{"this host has no systemd"}) {
		t.Errorf("lines %q", lines)
	}
}

func TestInstallKeys(t *testing.T) {
	key := testKey(t)
	srv := sshtest.NewServer(t, key.PublicKey(), func(string, []byte, io.Writer, io.Writer) uint32 { return 0 })
	tests := []struct {
		name    string
		signer  ssh.Signer
		known   ssh.PublicKey // the key in known_hosts before
		wantKey ssh.PublicKey // the key in known_hosts after
		wantRun bool
	}{
		{"first use", key, nil, srv.HostKey, true},
		{"same host key", key, srv.HostKey, srv.HostKey, true},
		{"changed host key", key, testKey(t).PublicKey(), nil, false},
		{"unknown client key", testKey(t), nil, srv.HostKey, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			known := newKnownHosts(t)
			if tt.known != nil {
				if err := os.WriteFile(known.Path, []byte(knownhosts.Line([]string{srv.Addr}, tt.known)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ran := len(srv.Commands())
			err := Install(t.Context(), Target{srv.Addr, "deploy"}, tt.signer, known, "true", nil, func(string) {})
			if (err == nil) != tt.wantRun {
				t.Errorf("err = %v, want the command to run: %v", err, tt.wantRun)
			}
			if tt.wantKey == nil {
				if _, ok := errors.AsType[*HostKeyError](err); !ok || !strings.Contains(err.Error(), known.Path) {
					t.Errorf("err = %v, want a HostKeyError that names %s", err, known.Path)
				}
			} else if err := trusts(known.Path, srv.Addr, tt.wantKey); err != nil {
				t.Errorf("known_hosts does not trust %s for %s: %v", fingerprint(tt.wantKey), srv.Addr, err)
			}
			if got := len(srv.Commands()) > ran; got != tt.wantRun {
				t.Errorf("command ran: %v, want %v", got, tt.wantRun)
			}
		})
	}
}

func fingerprint(key ssh.PublicKey) string {
	if key == nil {
		return "none"
	}
	return ssh.FingerprintSHA256(key)
}

// trusts reads the known_hosts file as OpenSSH does, and reports if it
// trusts key for addr.
func trusts(path, addr string, key ssh.PublicKey) error {
	check, err := knownhosts.New(path)
	if err != nil {
		return err
	}
	remote, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return err
	}
	return check(addr, remote, key)
}

// The end of ctx stops a command that runs.
func TestInstallCancel(t *testing.T) {
	key := testKey(t)
	running, block := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(block) })
	srv := sshtest.NewServer(t, key.PublicKey(), func(string, []byte, io.Writer, io.Writer) uint32 {
		close(running)
		<-block
		return 0
	})
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-running
		cancel()
	}()
	if err := Install(ctx, Target{srv.Addr, "deploy"}, key, newKnownHosts(t), "sleep 600", nil, func(string) {}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
