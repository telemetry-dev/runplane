package shell

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sshkey"
)

const (
	debugID   = "01jbd5x7k2m4n6p8q9r0s1t2v3"
	plainID   = "01jbd6aaaaaaaaaaaaaaaaaaaa"
	expiredID = "01jbd7bbbbbbbbbbbbbbbbbbbb"
	noActorID = "01jbd8cccccccccccccccccccc"
)

// fakeTerm is the gateway end of a pipe. The test holds the other end.
type fakeTerm struct {
	net.Conn
	resized chan sandbox.TermSize
}

func (f *fakeTerm) Resize(size sandbox.TermSize) error {
	f.resized <- size
	return nil
}

// opened is one Open call of the gateway.
type opened struct {
	sandboxID string
	size      sandbox.TermSize
	peer      net.Conn // reads what the client types, writes the shell output
	resized   chan sandbox.TermSize
}

type fakeOpener chan opened

func (o fakeOpener) Open(_ context.Context, id string, size sandbox.TermSize) (sandbox.Terminal, error) {
	term, peer := net.Pipe()
	resized := make(chan sandbox.TermSize, 8)
	o <- opened{sandboxID: id, size: size, peer: peer, resized: resized}
	return &fakeTerm{Conn: term, resized: resized}, nil
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// auditEntry is one Record call of the gateway.
type auditEntry struct {
	by             audit.Actor
	action, target string
	detail         map[string]any
}

// gateway serves debugID (also by its first 8 characters) to the actor
// octocat, plainID (not a debug sandbox), expiredID (its hold ended), and
// noActorID (its job has no actor yet).
type gateway struct {
	addr                   string
	hostKey                ssh.PublicKey
	opens                  fakeOpener
	audits                 chan auditEntry
	actor, admin, stranger ssh.Signer
}

func startGateway(t *testing.T) *gateway {
	t.Helper()
	g := &gateway{opens: make(fakeOpener, 4), audits: make(chan auditEntry, 4),
		actor: newSigner(t), admin: newSigner(t), stranger: newSigner(t)}
	debug := Target{SandboxID: debugID, State: sandbox.StateBusy, Debug: true, Actor: "octocat", Repository: "octo/api", Job: "build"}
	plain, expired, noActor := debug, debug, debug
	plain.SandboxID, plain.Debug = plainID, false
	ended := time.Now().Add(-time.Minute)
	expired.SandboxID, expired.State, expired.HoldUntil = expiredID, sandbox.StateStopping, &ended
	noActor.SandboxID, noActor.Actor = noActorID, ""
	targets := map[string]Target{debugID: debug, debugID[:8]: debug, plainID: plain, expiredID: expired, noActorID: noActor}

	hostKeyPath := filepath.Join(t.TempDir(), "ssh_host_ed25519_key")
	srv, err := New(Config{
		Addr:        "127.0.0.1:0",
		HostKeyPath: hostKeyPath,
		Opener:      g.opens,
		Lookup: func(_ context.Context, name string) (Target, error) {
			if t, ok := targets[name]; ok {
				return t, nil
			}
			return Target{}, ErrNotFound
		},
		Keys: func(_ context.Context, login string) ([]ssh.PublicKey, error) {
			if login == "octocat" {
				return []ssh.PublicKey{g.actor.PublicKey()}, nil
			}
			return nil, nil
		},
		AdminKeys: func(context.Context) []ssh.PublicKey { return []ssh.PublicKey{g.admin.PublicKey()} },
		Audit: func(_ context.Context, by audit.Actor, action, target string, detail map[string]any) {
			g.audits <- auditEntry{by, action, target, detail}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(context.Background()) }()
	t.Cleanup(func() {
		srv.Close()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	hostKey, _, err := sshkey.LoadOrCreate(hostKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	g.addr, g.hostKey = srv.Addr().String(), hostKey.PublicKey()
	return g
}

// dial connects as user and returns the auth banners that the client got.
func (g *gateway) dial(user string, signer ssh.Signer) (*ssh.Client, []string, error) {
	var banners []string
	c, err := ssh.Dial("tcp", g.addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(g.hostKey),
		BannerCallback:  func(msg string) error { banners = append(banners, msg); return nil },
		Timeout:         5 * time.Second,
	})
	return c, banners, err
}

// shell is an open interactive session.
type shell struct {
	session *ssh.Session
	stdin   io.WriteCloser
	stdout  io.Reader
	term    opened
}

func (g *gateway) openShell(t *testing.T, user string, signer ssh.Signer) shell {
	t.Helper()
	c, _, err := g.dial(user, signer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	session, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RequestPty("xterm-256color", 40, 120, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	term := recv(t, g.opens)
	term.peer.SetDeadline(time.Now().Add(5 * time.Second))
	return shell{session: session, stdin: stdin, stdout: stdout, term: term}
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		panic("unreachable")
	}
}

// readUntil reads r until the text has want and returns the text.
func readUntil(t *testing.T, r io.Reader, want string) string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		var text []byte
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			text = append(text, buf[:n]...)
			if strings.Contains(string(text), want) || err != nil {
				done <- string(text)
				return
			}
		}
	}()
	text := recv(t, done)
	if !strings.Contains(text, want) {
		t.Fatalf("output %q has no %q", text, want)
	}
	return text
}

func TestShellWithActorKey(t *testing.T) {
	g := startGateway(t)
	sh := g.openShell(t, debugID[:8], g.actor)
	if sh.term.sandboxID != debugID {
		t.Errorf("Open got sandbox %q, want the full ID %q", sh.term.sandboxID, debugID)
	}
	if want := (sandbox.TermSize{Rows: 40, Cols: 120}); sh.term.size != want {
		t.Errorf("Open got size %+v, want the pty size %+v", sh.term.size, want)
	}

	if _, err := io.WriteString(sh.stdin, "ls\n"); err != nil {
		t.Fatal(err)
	}
	typed := make([]byte, 3)
	if _, err := io.ReadFull(sh.term.peer, typed); err != nil || string(typed) != "ls\n" {
		t.Fatalf("terminal got %q (%v), want %q", typed, err, "ls\n")
	}
	if _, err := io.WriteString(sh.term.peer, "go.mod\r\n"); err != nil {
		t.Fatal(err)
	}
	out := readUntil(t, sh.stdout, "go.mod")
	if i := strings.Index(out, "octo/api"); i < 0 || i > strings.Index(out, "go.mod") {
		t.Errorf("output %q does not start with a banner that names the repository", out)
	}

	e := recv(t, g.audits)
	if e.action != "sandbox.ssh" || e.target != debugID || e.by != (audit.Actor{IP: "127.0.0.1"}) || e.detail["github_login"] != "octocat" ||
		e.detail["key"] != ssh.FingerprintSHA256(g.actor.PublicKey()) || e.detail["job_name"] != "build" || e.detail["repository"] != "octo/api" {
		t.Errorf("audit entry %+v, want sandbox.ssh on %s for the job build of octo/api by octocat from 127.0.0.1", e, debugID)
	}
}

func TestWindowChangeResizesTerminal(t *testing.T) {
	g := startGateway(t)
	sh := g.openShell(t, debugID, g.actor)
	if err := sh.session.WindowChange(50, 132); err != nil {
		t.Fatal(err)
	}
	if got, want := recv(t, sh.term.resized), (sandbox.TermSize{Rows: 50, Cols: 132}); got != want {
		t.Errorf("Resize got %+v, want %+v", got, want)
	}
}

func TestShellEndEndsSession(t *testing.T) {
	g := startGateway(t)
	sh := g.openShell(t, debugID, g.actor)
	sh.term.peer.Close() // the user typed exit
	done := make(chan error, 1)
	go func() { done <- sh.session.Wait() }()
	if err := recv(t, done); err != nil {
		t.Errorf("Wait = %v, want exit status 0", err)
	}
}

func TestClientDisconnectClosesTerminal(t *testing.T) {
	g := startGateway(t)
	sh := g.openShell(t, debugID, g.actor)
	sh.session.Close()
	// A closed terminal gives EOF on the other end of the pipe. A deadline
	// error means that the gateway left the shell open in the sandbox.
	if _, err := sh.term.peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("terminal read = %v, want EOF after the client left", err)
	}
}

func TestAdminKeyOpensShellWithoutActor(t *testing.T) {
	g := startGateway(t)
	sh := g.openShell(t, noActorID, g.admin)
	if sh.term.sandboxID != noActorID {
		t.Errorf("Open got sandbox %q, want %q", sh.term.sandboxID, noActorID)
	}
	if detail := recv(t, g.audits).detail; detail["admin_key"] != true || detail["github_login"] != nil {
		t.Errorf("audit detail %v, want an admin key and no GitHub login", detail)
	}
}

func TestExecIsRefused(t *testing.T) {
	g := startGateway(t)
	c, _, err := g.dial(debugID, g.actor)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	session.Stderr = &stderr

	err = session.Run("cat /etc/shadow")
	var exitErr *ssh.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitStatus() != 1 {
		t.Errorf("Run = %v, want exit status 1", err)
	}
	if stderr.Len() == 0 {
		t.Error("stderr is empty, want a message")
	}
	if len(g.opens) != 0 {
		t.Error("exec opened a terminal")
	}
}

func TestRefusedConnections(t *testing.T) {
	g := startGateway(t)
	tests := []struct {
		name   string
		user   string
		signer ssh.Signer
		// Only a client that proved it holds an allowed key learns why the
		// sandbox refuses. Public keys are public.
		wantBanner bool
	}{
		{"unknown key", debugID, g.stranger, false},
		{"unknown key on a refused sandbox", plainID, g.stranger, false},
		{"not a debug sandbox", plainID, g.actor, true},
		{"hold ended", expiredID, g.actor, true},
		{"unknown sandbox with admin key", "01zzzzzz", g.admin, true},
		{"unknown sandbox with actor key", "01zzzzzz", g.actor, false},
		{"user name that is not an ID", "root", g.admin, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, banners, err := g.dial(tt.user, tt.signer)
			if err == nil {
				c.Close()
				t.Fatal("connection accepted")
			}
			if got := len(banners) > 0; got != tt.wantBanner {
				t.Errorf("banners %q, want a banner: %v", banners, tt.wantBanner)
			}
		})
	}
	if len(g.opens) != 0 {
		t.Error("a refused connection opened a terminal")
	}
}

func TestRefusal(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	later, earlier := now.Add(10*time.Minute), now.Add(-time.Second)
	tests := []struct {
		name   string
		target Target
		accept bool
	}{
		{"busy debug sandbox", Target{Debug: true, State: sandbox.StateBusy}, true},
		{"held after the job", Target{Debug: true, State: sandbox.StateStopping, HoldUntil: &later}, true},
		{"hold ended", Target{Debug: true, State: sandbox.StateStopping, HoldUntil: &earlier}, false},
		{"debug sandbox before its job", Target{Debug: true, State: sandbox.StateIdle}, false},
		{"busy sandbox without debug", Target{State: sandbox.StateBusy}, false},
		{"held sandbox without debug", Target{State: sandbox.StateBusy, HoldUntil: &later}, false},
		{"gone during its hold", Target{Debug: true, State: sandbox.StateGone, HoldUntil: &later}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if msg := refusal(tt.target, now); (msg == "") != tt.accept {
				t.Errorf("refusal = %q, want accept: %v", msg, tt.accept)
			}
		})
	}
}
