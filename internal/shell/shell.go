// Package shell serves the debug shell of sandboxes over SSH.
//
// The SSH user name is the ID of a sandbox, or its first 8 or more
// characters. A key can connect if it is one of the GitHub keys of the job
// actor or an admin key. Only a debug sandbox accepts a connection, and only
// while it runs its job or waits in its debug hold.
package shell

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sshinstall"
	"github.com/runplane/runplane/internal/store"
)

// Opener opens a login shell as the runner user in a sandbox.
type Opener interface {
	Open(ctx context.Context, sandboxID string, size sandbox.TermSize) (sandbox.Terminal, error)
}

// Target is a sandbox as the gateway sees it.
type Target struct {
	SandboxID  string
	State      sandbox.State
	Debug      bool
	HoldUntil  *time.Time // set when the job completes
	Actor      string     // GitHub login of the job actor, empty if not known
	Repository string     // owner/name
	Job        string     // job name
}

// Errors of Config.Lookup.
var (
	ErrNotFound  = errors.New("no live sandbox has this ID")
	ErrAmbiguous = errors.New("several live sandboxes have IDs that start with this name")
)

type Config struct {
	Addr        string // default ":2222"
	HostKeyPath string // ed25519 host key; New makes it if the file does not exist
	Opener      Opener
	// Lookup returns the live sandbox whose ID is name or starts with name,
	// or ErrNotFound or ErrAmbiguous.
	Lookup func(ctx context.Context, name string) (Target, error)
	// Keys returns the public SSH keys of a GitHub user. The default reads
	// them from GitHub (NewGitHubKeys).
	Keys func(ctx context.Context, login string) ([]ssh.PublicKey, error)
	// AdminKeys returns the keys that can connect to all debug sandboxes.
	// Optional.
	AdminKeys func(ctx context.Context) []ssh.PublicKey
	// Audit records each shell that opens. Optional. (*store.Store).InsertAudit fits.
	Audit func(ctx context.Context, e store.AuditEntry) error
	Log   *slog.Logger
}

// Server is the SSH gateway.
type Server struct {
	cfg     Config
	ssh     *ssh.ServerConfig
	hostKey ssh.PublicKey
	ln      net.Listener

	ctx    context.Context // ends at Close
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
}

const (
	// handshakeTimeout limits the key exchange and the authentication, so
	// that clients which never finish do not keep their connections.
	handshakeTimeout = 30 * time.Second
	// authTimeout limits the lookups for one offered key.
	authTimeout = 15 * time.Second
)

// New loads or makes the host key and listens on cfg.Addr. Call Serve to
// accept connections.
func New(cfg Config) (*Server, error) {
	if cfg.Opener == nil || cfg.Lookup == nil || cfg.HostKeyPath == "" {
		return nil, errors.New("new ssh gateway: Opener, Lookup, and HostKeyPath are required")
	}
	if cfg.Addr == "" {
		cfg.Addr = ":2222"
	}
	if cfg.Keys == nil {
		cfg.Keys = NewGitHubKeys(nil).Keys
	}
	if cfg.AdminKeys == nil {
		cfg.AdminKeys = func(context.Context) []ssh.PublicKey { return nil }
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	hostKey, _, err := sshinstall.LoadOrCreateKey(cfg.HostKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load ssh host key: %w", err)
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen for ssh: %w", err)
	}
	s := &Server{cfg: cfg, hostKey: hostKey.PublicKey(), ln: ln, conns: map[net.Conn]struct{}{}}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.ssh = &ssh.ServerConfig{
		PublicKeyCallback:         s.authorize,
		VerifiedPublicKeyCallback: s.verified,
		ServerVersion:             "SSH-2.0-runplane",
	}
	s.ssh.AddHostKey(hostKey)
	return s, nil
}

// Addr is the address that the gateway listens on.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// HostKey is the public host key, for the known_hosts line that the
// dashboard can show.
func (s *Server) HostKey() ssh.PublicKey { return s.hostKey }

// Serve accepts connections until ctx ends or Close is called. Then it
// closes the gateway and returns nil.
func (s *Server) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	s.cfg.Log.Info("ssh gateway listens", "addr", s.ln.Addr().String())
	var delay time.Duration
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			// Accept fails for a while when the process has too many open files.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			s.cfg.Log.Warn("accept ssh connection", "err", err, "retry_in", delay)
			select {
			case <-time.After(delay):
			case <-s.ctx.Done():
				return nil
			}
			continue
		}
		delay = 0
		if !s.track(nc) {
			nc.Close()
			return nil
		}
		go func() {
			defer s.untrack(nc)
			s.serveConn(nc)
		}()
	}
}

// Close stops the gateway, ends all connections, and waits until each
// session has closed its terminal.
func (s *Server) Close() error {
	s.mu.Lock()
	s.cancel()
	err := s.ln.Close()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (s *Server) track(nc net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return false
	}
	s.conns[nc] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *Server) untrack(nc net.Conn) {
	s.mu.Lock()
	delete(s.conns, nc)
	s.mu.Unlock()
	s.wg.Done()
}

// Permission extensions that authorize gives to the connection.
const (
	extSandbox = "runplane-sandbox"
	extLogin   = "runplane-login" // empty for an admin key
	extKey     = "runplane-key"   // SHA-256 fingerprint
	extRefusal = "runplane-refusal"
)

var errDenied = errors.New("key not allowed for this sandbox")

// authorize decides if key may connect to the sandbox of the user name. It
// accepts the key even when the sandbox refuses the connection: verified
// then tells the reason, but only to a client that has proved it holds the
// key. Public keys are public, so a key alone must not reveal anything.
func (s *Server) authorize(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	name := strings.ToLower(meta.User())
	if !validName(name) {
		return nil, errDenied
	}
	ctx, cancel := context.WithTimeout(s.ctx, authTimeout)
	defer cancel()
	t, refusal := s.target(ctx, name)
	login, err := s.owner(ctx, t.Actor, key)
	if err != nil {
		return nil, err
	}
	return &ssh.Permissions{Extensions: map[string]string{
		extSandbox: t.SandboxID,
		extLogin:   login,
		extKey:     ssh.FingerprintSHA256(key),
		extRefusal: refusal,
	}}, nil
}

func (s *Server) verified(_ ssh.ConnMetadata, _ ssh.PublicKey, perms *ssh.Permissions, _ string) (*ssh.Permissions, error) {
	if msg := perms.Extensions[extRefusal]; msg != "" {
		return nil, &ssh.BannerError{Err: errDenied, Message: "runplane: " + msg + "\n"}
	}
	return perms, nil
}

// owner returns the GitHub login of actor if key is one of the actor's
// keys, or "" for an admin key. Other keys get errDenied.
func (s *Server) owner(ctx context.Context, actor string, key ssh.PublicKey) (string, error) {
	if hasKey(s.cfg.AdminKeys(ctx), key) {
		return "", nil
	}
	if actor == "" {
		return "", errDenied
	}
	keys, err := s.cfg.Keys(ctx, actor)
	if err != nil {
		s.cfg.Log.Warn("read github keys", "login", actor, "err", err)
	}
	if hasKey(keys, key) {
		return actor, nil
	}
	return "", errDenied
}

// target looks up the sandbox. If the sandbox refuses a connection, it also
// returns the reason for the user.
func (s *Server) target(ctx context.Context, name string) (Target, string) {
	t, err := s.cfg.Lookup(ctx, name)
	switch {
	case errors.Is(err, ErrNotFound):
		return Target{}, fmt.Sprintf("No live sandbox has the ID %s.", name)
	case errors.Is(err, ErrAmbiguous):
		return Target{}, fmt.Sprintf("Several sandbox IDs start with %s. Use the full sandbox ID.", name)
	case err != nil:
		s.cfg.Log.Error("look up sandbox", "name", name, "err", err)
		return Target{}, "runplane could not read the sandbox. The runplane log has the error."
	}
	return t, refusal(t, time.Now())
}

// refusal returns why the sandbox refuses a connection now, or "".
func refusal(t Target, now time.Time) string {
	held := t.HoldUntil != nil && now.Before(*t.HoldUntil)
	switch {
	case !t.Debug:
		return fmt.Sprintf("Sandbox %s is not a debug sandbox. Use \"Re-run with SSH\" on the job page.", t.SandboxID)
	case t.State.Done():
		return fmt.Sprintf("Sandbox %s has stopped.", t.SandboxID)
	case t.State == sandbox.StateBusy || held:
		return ""
	case t.HoldUntil != nil:
		return fmt.Sprintf("The debug hold of sandbox %s ended at %s UTC.", t.SandboxID, t.HoldUntil.UTC().Format("15:04"))
	default:
		return fmt.Sprintf("Sandbox %s has no job yet. Connect when its job runs.", t.SandboxID)
	}
}

// validName reports if name can be a sandbox ID or the start of one. The
// check keeps the user names of internet scanners away from the database.
func validName(name string) bool {
	if len(name) < 8 || len(name) > 26 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}
