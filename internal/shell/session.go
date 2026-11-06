package shell

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/sandbox"
)

// client is an authenticated connection.
type client struct {
	sandboxID string
	login     string // empty for an admin key
	key       string // SHA-256 fingerprint
	ip        string
}

func (s *Server) serveConn(nc net.Conn) {
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(handshakeTimeout))
	conn, chans, reqs, err := ssh.NewServerConn(nc, s.ssh)
	if err != nil {
		s.cfg.Log.Debug("ssh handshake", "remote", nc.RemoteAddr().String(), "err", err)
		return
	}
	nc.SetDeadline(time.Time{})
	defer conn.Close()
	go ssh.DiscardRequests(reqs)

	ext := conn.Permissions.Extensions
	c := client{sandboxID: ext[extSandbox], login: ext[extLogin], key: ext[extKey], ip: nc.RemoteAddr().String()}
	if host, _, err := net.SplitHostPort(c.ip); err == nil {
		c.ip = host
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	var wg sync.WaitGroup
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.Prohibited, "runplane gives only a shell")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.session(ctx, c, ch, chReqs)
		}()
	}
	wg.Wait()
}

// session serves one session channel. It returns when the channel closes,
// and then closes the terminal.
func (s *Server) session(ctx context.Context, c client, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	size := sandbox.TermSize{Rows: 24, Cols: 80}
	var term sandbox.Terminal
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var p struct {
				Term          string
				Cols, Rows    uint32
				Width, Height uint32
				Modes         string
			}
			if ssh.Unmarshal(req.Payload, &p) == nil {
				size = termSize(p.Cols, p.Rows, size)
			}
			req.Reply(true, nil)
		case "window-change":
			var w struct {
				Cols, Rows    uint32
				Width, Height uint32
			}
			if ssh.Unmarshal(req.Payload, &w) == nil {
				size = termSize(w.Cols, w.Rows, size)
				if term != nil {
					if err := term.Resize(size); err != nil {
						s.cfg.Log.Debug("resize debug shell", "sandbox", c.sandboxID, "err", err)
					}
				}
			}
			req.Reply(true, nil)
		case "shell":
			if term != nil {
				req.Reply(false, nil)
				continue
			}
			t, msg := s.target(ctx, c.sandboxID)
			if msg != "" {
				refuse(ch, req, msg)
				continue
			}
			opened, err := s.cfg.Opener.Open(ctx, t.SandboxID, size)
			if err != nil {
				s.cfg.Log.Warn("open debug shell", "sandbox", t.SandboxID, "err", err)
				refuse(ch, req, "runplane could not open the shell: "+err.Error())
				continue
			}
			term = opened
			req.Reply(true, nil)
			s.audit(ctx, c, t)
			s.cfg.Log.Info("debug shell opened over ssh", "sandbox", t.SandboxID, "login", c.login, "key", c.key, "ip", c.ip)
			io.WriteString(ch, banner(t, time.Now()))
			go pipe(ch, term)
		case "exec", "subsystem":
			refuse(ch, req, "The debug shell runs no commands. Connect without a command for a login shell.")
		default: // env, signal, and agent and X11 forwarding
			req.Reply(false, nil)
		}
	}
	if term != nil {
		term.Close()
	}
}

// pipe copies bytes between the channel and the terminal. When the shell
// ends, it ends the session with exit status 0, because the terminal does
// not give the status of the shell.
func pipe(ch ssh.Channel, term sandbox.Terminal) {
	go io.Copy(term, ch)
	io.Copy(ch, term)
	exit(ch, 0)
	ch.Close()
}

// refuse accepts the request, then prints msg on stderr and ends the
// session with exit status 1. Thus the client shows msg and no generic
// "request failed" error.
func refuse(ch ssh.Channel, req *ssh.Request, msg string) {
	req.Reply(true, nil)
	fmt.Fprintf(ch.Stderr(), "runplane: %s\r\n", msg)
	exit(ch, 1)
	ch.Close()
}

func exit(ch ssh.Channel, status uint32) {
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
}

// termSize returns the size from the client, or old if the client sends
// a size that a terminal cannot have.
func termSize(cols, rows uint32, old sandbox.TermSize) sandbox.TermSize {
	if cols == 0 || rows == 0 || cols > math.MaxUint16 || rows > math.MaxUint16 {
		return old
	}
	return sandbox.TermSize{Rows: uint16(rows), Cols: uint16(cols)}
}

// banner is the text before the shell output. The terminal of the client is
// in raw mode, so the lines end with CR LF.
func banner(t Target, now time.Time) string {
	var b strings.Builder
	b.WriteString("runplane debug shell\r\n")
	if t.Repository != "" {
		fmt.Fprintf(&b, "  Repository  %s\r\n", t.Repository)
	}
	if t.Job != "" {
		fmt.Fprintf(&b, "  Job         %s\r\n", t.Job)
	}
	if t.HoldUntil != nil {
		left := int(math.Ceil(t.HoldUntil.Sub(now).Minutes()))
		fmt.Fprintf(&b, "  Hold        until %s UTC (%d min left)\r\n", t.HoldUntil.UTC().Format("15:04"), left)
	} else {
		b.WriteString("  Hold        starts when the job ends\r\n")
	}
	b.WriteString("\r\n")
	return b.String()
}

func (s *Server) audit(ctx context.Context, c client, t Target) {
	detail := map[string]any{"key": c.key}
	if c.login != "" {
		detail["github_login"] = c.login
	} else {
		detail["admin_key"] = true
	}
	if t.Job != "" {
		detail["job_name"], detail["repository"] = t.Job, t.Repository
	}
	s.cfg.Audit(ctx, audit.Actor{IP: c.ip}, audit.SandboxSSH, c.sandboxID, detail)
}
