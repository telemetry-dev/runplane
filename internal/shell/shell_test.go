package shell

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sshinstall"
	"github.com/runplane/runplane/internal/store"
)

const (
	debugID   = "01jbd5x7k2m4n6p8q9r0s1t2v3"
	plainID   = "01jbd6aaaaaaaaaaaaaaaaaaaa"
	expiredID = "01jbd7bbbbbbbbbbbbbbbbbbbb"
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

type gateway struct {
	addr    string
	hostKey ssh.PublicKey
	opens   fakeOpener
	audits  chan store.AuditEntry
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

// startGateway serves targets by name. keys are the GitHub keys by login.
func startGateway(t *testing.T, targets map[string]Target, keys map[string][]ssh.PublicKey, admin []ssh.PublicKey) *gateway {
	t.Helper()
	g := &gateway{opens: make(fakeOpener, 4), audits: make(chan store.AuditEntry, 4)}
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
		Keys:      func(_ context.Context, login string) ([]ssh.PublicKey, error) { return keys[login], nil },
		AdminKeys: func(context.Context) []ssh.PublicKey { return admin },
		Audit: func(_ context.Context, e store.AuditEntry) error {
			g.audits <- e
			return nil
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
	hostKey, _, err := sshinstall.LoadOrCreateKey(hostKeyPath)
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

func debugTarget() Target {
	return Target{SandboxID: debugID, State: sandbox.StateBusy, Debug: true, Actor: "octocat", Repository: "octo/api", Job: "build"}
}

func TestShellWithActorKey(t *testing.T) {
	actor := newSigner(t)
	target := debugTarget()
	g := startGateway(t, map[string]Target{debugID: target, debugID[:8]: target},
		map[string][]ssh.PublicKey{"octocat": {actor.PublicKey()}}, nil)

	sh := g.openShell(t, debugID[:8], actor)
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
	var detail map[string]any
	if err := json.Unmarshal(e.Detail, &detail); err != nil {
		t.Fatal(err)
	}
	if e.Action != "sandbox.ssh" || e.Target != debugID || e.IP != "127.0.0.1" || detail["github_login"] != "octocat" ||
		detail["key"] != ssh.FingerprintSHA256(actor.PublicKey()) || detail["job_name"] != "build" || detail["repository"] != "octo/api" {
		t.Errorf("audit entry %+v with detail %v, want sandbox.ssh on %s for the job build of octo/api by octocat from 127.0.0.1", e, detail, debugID)
	}
}

func TestWindowChangeResizesTerminal(t *testing.T) {
	actor := newSigner(t)
	g := startGateway(t, map[string]Target{debugID: debugTarget()},
		map[string][]ssh.PublicKey{"octocat": {actor.PublicKey()}}, nil)
	sh := g.openShell(t, debugID, actor)

	if err := sh.session.WindowChange(50, 132); err != nil {
		t.Fatal(err)
	}
	if got, want := recv(t, sh.term.resized), (sandbox.TermSize{Rows: 50, Cols: 132}); got != want {
		t.Errorf("Resize got %+v, want %+v", got, want)
	}
}

func TestShellEndEndsSession(t *testing.T) {
	actor := newSigner(t)
	g := startGateway(t, map[string]Target{debugID: debugTarget()},
		map[string][]ssh.PublicKey{"octocat": {actor.PublicKey()}}, nil)
	sh := g.openShell(t, debugID, actor)

	sh.term.peer.Close() // the user typed exit
	done := make(chan error, 1)
	go func() { done <- sh.session.Wait() }()
	if err := recv(t, done); err != nil {
		t.Errorf("Wait = %v, want exit status 0", err)
	}
}

func TestClientDisconnectClosesTerminal(t *testing.T) {
	actor := newSigner(t)
	g := startGateway(t, map[string]Target{debugID: debugTarget()},
		map[string][]ssh.PublicKey{"octocat": {actor.PublicKey()}}, nil)
	sh := g.openShell(t, debugID, actor)

	sh.session.Close()
	// A closed terminal gives EOF on the other end of the pipe. A deadline
	// error means that the gateway left the shell open in the sandbox.
	if _, err := sh.term.peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("terminal read = %v, want EOF after the client left", err)
	}
}

func TestAdminKeyOpensShellWithoutActor(t *testing.T) {
	admin := newSigner(t)
	target := debugTarget()
	target.Actor = "" // the job has no enrichment yet
	g := startGateway(t, map[string]Target{debugID: target}, nil, []ssh.PublicKey{admin.PublicKey()})

	sh := g.openShell(t, debugID, admin)
	if sh.term.sandboxID != debugID {
		t.Errorf("Open got sandbox %q, want %q", sh.term.sandboxID, debugID)
	}
	e := recv(t, g.audits)
	var detail map[string]any
	if err := json.Unmarshal(e.Detail, &detail); err != nil {
		t.Fatal(err)
	}
	if detail["admin_key"] != true || detail["github_login"] != nil {
		t.Errorf("audit detail %v, want an admin key and no GitHub login", detail)
	}
}

func TestExecIsRefused(t *testing.T) {
	actor := newSigner(t)
	g := startGateway(t, map[string]Target{debugID: debugTarget()},
		map[string][]ssh.PublicKey{"octocat": {actor.PublicKey()}}, nil)
	c, _, err := g.dial(debugID, actor)
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
	actor, admin, stranger := newSigner(t), newSigner(t), newSigner(t)
	plain := debugTarget()
	plain.SandboxID, plain.Debug = plainID, false
	expired := debugTarget()
	ended := time.Now().Add(-time.Minute)
	expired.SandboxID, expired.State, expired.HoldUntil = expiredID, sandbox.StateStopping, &ended
	g := startGateway(t,
		map[string]Target{debugID: debugTarget(), plainID: plain, expiredID: expired},
		map[string][]ssh.PublicKey{"octocat": {actor.PublicKey()}},
		[]ssh.PublicKey{admin.PublicKey()})

	tests := []struct {
		name   string
		user   string
		signer ssh.Signer
		// Only a client that proved it holds an allowed key learns why the
		// sandbox refuses. Public keys are public.
		wantBanner bool
	}{
		{"unknown key", debugID, stranger, false},
		{"unknown key on a refused sandbox", plainID, stranger, false},
		{"not a debug sandbox", plainID, actor, true},
		{"hold ended", expiredID, actor, true},
		{"unknown sandbox with admin key", "01zzzzzz", admin, true},
		{"unknown sandbox with actor key", "01zzzzzz", actor, false},
		{"user name that is not an ID", "root", admin, false},
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
