package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/joblog"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// fakeShell stands in for the sandbox driver: the test plays the terminal.
// With err, no shell opens.
type fakeShell struct {
	opened chan sandbox.TermSize
	term   *fakeTerm
	err    error
}

func (s *fakeShell) Open(_ context.Context, _ string, size sandbox.TermSize) (sandbox.Terminal, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.opened <- size
	return s.term, nil
}

// dialShell opens a shell as the dashboard does: path over WebSocket, with
// the cookies of b and the headers h.
func (b *browser) dialShell(ctx context.Context, path string, h http.Header) (*websocket.Conn, *http.Response, error) {
	u, _ := url.Parse(b.e.srv.URL)
	h = h.Clone()
	if h == nil {
		h = http.Header{}
	}
	for _, c := range b.client.Jar.Cookies(u) {
		h.Add("Cookie", c.String())
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(b.e.srv.URL, "http")+path, &websocket.DialOptions{HTTPHeader: h})
}

type fakeTerm struct {
	output  *io.PipeReader // what the shell prints
	input   chan string
	resized chan sandbox.TermSize
}

func (f *fakeTerm) Read(p []byte) (int, error) { return f.output.Read(p) }
func (f *fakeTerm) Write(p []byte) (int, error) {
	f.input <- string(p)
	return len(p), nil
}
func (f *fakeTerm) Close() error { return f.output.Close() }
func (f *fakeTerm) Resize(s sandbox.TermSize) error {
	f.resized <- s
	return nil
}

func TestShellBridgesTerminal(t *testing.T) {
	r, w := io.Pipe()
	shell := &fakeShell{opened: make(chan sandbox.TermSize, 1), term: &fakeTerm{
		output: r, input: make(chan string, 1), resized: make(chan sandbox.TermSize, 1),
	}}
	e := newEnv(t, func(d *Deps) { d.Shell = shell })
	e.addSandbox("sb1", sandbox.StateBusy)
	e.addSandbox("plain", sandbox.StateBusy)
	must(t, e.st.SetSandboxDebug(context.Background(), "sb1"))
	b := e.login("bob@example.com", auth.RoleMember)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A page on another site cannot open the shell with the cookie.
	_, res, err := b.dialShell(ctx, "/api/sandboxes/sb1/shell", http.Header{"Origin": {"https://evil.example"}})
	if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site dial: err %v, response %v; want 403", err, res)
	}
	b.do("GET", "/api/sandboxes/plain/shell", nil, nil).want(t, http.StatusForbidden)

	conn, _, err := b.dialShell(ctx, "/api/sandboxes/sb1/shell?rows=30&cols=100", nil)
	must(t, err)
	defer conn.CloseNow()
	if got := recv(t, shell.opened); got != (sandbox.TermSize{Rows: 30, Cols: 100}) {
		t.Fatalf("shell opened with size %+v, want 30x100", got)
	}

	go w.Write([]byte("$ "))
	typ, data, err := conn.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(data) != "$ " {
		t.Fatalf("read prompt: %v %q %v", typ, data, err)
	}
	must(t, conn.Write(ctx, websocket.MessageBinary, []byte("ls\n")))
	if got := recv(t, shell.term.input); got != "ls\n" {
		t.Fatalf("terminal input %q, want ls", got)
	}
	must(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","rows":40,"cols":120}`)))
	if got := recv(t, shell.term.resized); got != (sandbox.TermSize{Rows: 40, Cols: 120}) {
		t.Fatalf("resize %+v, want 40x120", got)
	}

	// The shell ends: the browser gets a normal close.
	w.Close()
	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("after the shell ends: %v, want a normal close", err)
	}

	entries, _, err := e.st.ListAudit(context.Background(), "", 10)
	must(t, err)
	member, _ := e.st.GetUserByEmail(context.Background(), "bob@example.com")
	if len(entries) != 1 || entries[0].Action != "sandbox.shell" || entries[0].UserID != member.ID {
		t.Fatalf("audit = %+v, want one sandbox.shell entry by the member", entries)
	}
}

// The API answers the shell rule of package shell, which has its own test,
// with a status: 403 for a sandbox without debug, and 409 for a debug
// sandbox without a job that runs or holds. A plain GET of a shell that the
// rule lets open gets 426, because it is no WebSocket.
func TestShellNeedsALiveDebugJob(t *testing.T) {
	e := newEnv(t)
	b := e.login("bob@example.com", auth.RoleMember)
	later, earlier := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name string
		b    store.Sandbox
		want int
	}{
		{"busy debug sandbox", store.Sandbox{Debug: true, State: sandbox.StateBusy}, http.StatusUpgradeRequired},
		{"held after the job", store.Sandbox{Debug: true, State: sandbox.StateStopping, HoldUntil: &later}, http.StatusUpgradeRequired},
		{"busy without debug", store.Sandbox{State: sandbox.StateBusy}, http.StatusForbidden},
		{"hold ended", store.Sandbox{Debug: true, State: sandbox.StateStopping, HoldUntil: &earlier}, http.StatusConflict},
		{"debug sandbox before its job", store.Sandbox{Debug: true, State: sandbox.StateIdle}, http.StatusConflict},
		{"gone during its hold", store.Sandbox{Debug: true, State: sandbox.StateGone, HoldUntil: &later}, http.StatusConflict},
	} {
		sb := tc.b
		sb.ID, sb.PoolID, sb.ServerID, sb.Mode = ids.New(), "p", "s", sandbox.ModeContainer
		sb.VCPU, sb.MemoryMB, sb.CreatedAt = 2, 4096, time.Now()
		must(t, e.st.InsertSandbox(context.Background(), sb))
		if got := b.do("GET", "/api/sandboxes/"+sb.ID+"/shell", nil, nil).StatusCode; got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The browser shows why a shell did not open: the reason of the close. A
// close reason has at most 123 bytes of valid UTF-8.
func TestShellErrorKeepsWholeCharacters(t *testing.T) {
	// Byte 120 falls inside the 60th "é", so a cut there leaves half of it.
	e := newEnv(t, func(d *Deps) { d.Shell = &fakeShell{err: errors.New("a" + strings.Repeat("é", 100))} })
	e.addSandbox("sb1", sandbox.StateBusy)
	must(t, e.st.SetSandboxDebug(context.Background(), "sb1"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := e.login("bob@example.com", auth.RoleMember).dialShell(ctx, "/api/sandboxes/sb1/shell", nil)
	must(t, err)
	defer conn.CloseNow()
	_, _, err = conn.Read(ctx)
	var ce websocket.CloseError
	if want := "a" + strings.Repeat("é", 59) + "..."; !errors.As(err, &ce) || ce.Code != websocket.StatusInternalError || ce.Reason != want {
		t.Errorf("close %v, want status 1011 with the reason %q", err, want)
	}
}

type fakeLogs struct {
	log io.ReadCloser
	err error
}

func (f fakeLogs) Sandbox(context.Context, string, bool) (io.ReadCloser, error)  { return f.log, f.err }
func (f fakeLogs) Job(context.Context, string, int, bool) (io.ReadCloser, error) { return f.log, f.err }

// A log that is not there is a 404, and a server that is not connected a
// 503, so that the page tells why it shows no log.
func TestMissingLogs(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{joblog.ErrMissing, 404},
		{joblog.ErrExpired, 404},
		{joblog.ErrNoGitHubLog, 404},
		{joblog.ErrNotYet, 404},
		{joblog.ErrSandboxStopped, 404},
		{joblog.ErrNotConnected, 503},
	} {
		e := newEnv(t, func(d *Deps) { d.Logs = fakeLogs{err: tc.err} })
		e.addSandbox("sb1", sandbox.StateBusy)
		j := e.addJob(1, "acme/api", time.Now())
		b := e.login("bob@example.com", auth.RoleMember)
		for _, path := range []string{"/api/sandboxes/sb1/log", "/api/jobs/" + j.ID + "/log"} {
			if res := b.do("GET", path, nil, nil); res.StatusCode != tc.status {
				t.Errorf("%s with %v: status %d, want %d", path, tc.err, res.StatusCode, tc.status)
			}
		}
	}
}

func TestFollowedLogStreamsEachLine(t *testing.T) {
	r, w := io.Pipe()
	e := newEnv(t, func(d *Deps) { d.Logs = fakeLogs{log: r} })
	e.addSandbox("sb1", sandbox.StateBusy)
	b := e.login("bob@example.com", auth.RoleMember)

	req, _ := http.NewRequest("GET", e.srv.URL+"/api/sandboxes/sb1/log?follow=1", nil)
	go w.Write([]byte("line 1\n"))
	res, err := b.client.Do(req)
	must(t, err)
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	// The first line must arrive while the log is still open.
	stop := time.AfterFunc(5*time.Second, func() { res.Body.Close() })
	defer stop.Stop()
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || line != "line 1\n" {
		t.Fatalf("first line %q, %v", line, err)
	}
	w.Close()
	if _, err := io.ReadAll(res.Body); err != nil {
		t.Fatalf("stream did not end after the log closed: %v", err)
	}
}
