package sshinstall

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/runplane/runplane/internal/sshinstall/sshtest"
)

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
	srv := sshtest.NewServer(t, key.PublicKey(), func(_ string, stdin []byte, stdout, stderr io.Writer) uint32 {
		stdout.Write(stdin)
		io.WriteString(stdout, "Installing Docker\nInstalling Sysbox\r\n")
		io.WriteString(stderr, "warning: swap is on\n")
		io.WriteString(stdout, "Agent started") // no final newline
		return 0
	})
	var lines []string
	_, err := Install(t.Context(), Target{srv.Addr, "deploy"}, key, nil, "sudo -n sh -s -- --token rp_srv_x", strings.NewReader("#!/bin/sh\necho install\n"), func(l string) {
		lines = append(lines, l)
	})
	if err != nil {
		t.Fatal(err)
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
	_, err := Install(t.Context(), Target{srv.Addr, "deploy"}, key, nil, "sh -s", strings.NewReader(""), func(l string) { lines = append(lines, l) })
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
		known   ssh.PublicKey
		wantKey ssh.PublicKey
		wantRun bool
	}{
		{"first use", key, nil, srv.HostKey, true},
		{"same host key", key, srv.HostKey, srv.HostKey, true},
		{"changed host key", key, testKey(t).PublicKey(), nil, false},
		{"unknown client key", testKey(t), nil, srv.HostKey, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ran := len(srv.Commands())
			hostKey, err := Install(t.Context(), Target{srv.Addr, "deploy"}, tt.signer, tt.known, "true", nil, func(string) {})
			if (err == nil) != tt.wantRun {
				t.Errorf("err = %v, want the command to run: %v", err, tt.wantRun)
			}
			if fingerprint(hostKey) != fingerprint(tt.wantKey) {
				t.Errorf("host key %s, want %s", fingerprint(hostKey), fingerprint(tt.wantKey))
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

func TestInstallCancel(t *testing.T) {
	key := testKey(t)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	srv := sshtest.NewServer(t, key.PublicKey(), func(string, []byte, io.Writer, io.Writer) uint32 {
		<-block
		return 0
	})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := Install(ctx, Target{srv.Addr, "deploy"}, key, nil, "sleep 600", nil, func(string) {}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

// The servers have the authorized_keys line of the first key, so all
// callers must get that key.
func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh", "id_ed25519")
	lines := make([]string, 8)
	var wg sync.WaitGroup
	for i := range lines {
		wg.Go(func() {
			_, line, err := LoadOrCreateKey(path)
			if err != nil {
				t.Error(err)
			}
			lines[i] = line
		})
	}
	wg.Wait()
	signer, line, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if l != line {
			t.Fatalf("callers got different keys: %q", lines)
		}
	}
	pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || pub.Type() != ssh.KeyAlgoED25519 || comment != "runplane" || !bytes.Equal(pub.Marshal(), signer.PublicKey().Marshal()) {
		t.Errorf("authorized_keys line %q is not the ed25519 key with the comment runplane: %v", line, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v, %v; want mode 600", info, err)
	}
}
