package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// fakeShell stands in for the sandbox driver: the test plays the terminal.
type fakeShell struct {
	opened chan sandbox.TermSize
	term   *fakeTerm
}

func (s *fakeShell) Open(_ context.Context, _ string, size sandbox.TermSize) (sandbox.Terminal, error) {
	s.opened <- size
	return s.term, nil
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
	u, _ := url.Parse(e.srv.URL)
	cookie := b.client.Jar.Cookies(u)[0].String()
	shellURL := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/sandboxes/sb1/shell"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A page on another site cannot open the shell with the cookie.
	_, res, err := websocket.Dial(ctx, shellURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {cookie}, "Origin": {"https://evil.example"}},
	})
	if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site dial: err %v, response %v; want 403", err, res)
	}
	b.do("GET", "/api/sandboxes/plain/shell", nil, nil).want(t, http.StatusForbidden)

	conn, _, err := websocket.Dial(ctx, shellURL+"?rows=30&cols=100", &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {cookie}},
	})
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

func TestShellAllowedOnlyForLiveDebugJobs(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	later, earlier := now.Add(time.Minute), now.Add(-time.Minute)
	for _, tc := range []struct {
		name string
		b    store.Sandbox
		want int // 0: allowed
	}{
		{"busy debug sandbox", store.Sandbox{Debug: true, State: sandbox.StateBusy}, 0},
		{"held after the job", store.Sandbox{Debug: true, State: sandbox.StateStopping, HoldUntil: &later}, 0},
		{"busy without debug", store.Sandbox{State: sandbox.StateBusy}, http.StatusForbidden},
		{"hold ended", store.Sandbox{Debug: true, State: sandbox.StateStopping, HoldUntil: &earlier}, http.StatusConflict},
		{"debug sandbox before its job", store.Sandbox{Debug: true, State: sandbox.StateIdle}, http.StatusConflict},
		{"gone during its hold", store.Sandbox{Debug: true, State: sandbox.StateGone, HoldUntil: &later}, http.StatusConflict},
	} {
		err := shellAllowed(tc.b, now)
		got := 0
		if err != nil {
			got = err.(interface{ GetStatus() int }).GetStatus()
		}
		if got != tc.want {
			t.Errorf("%s: status %d (%v), want %d", tc.name, got, err, tc.want)
		}
	}
}

func TestCloseReasonKeepsWholeCharacters(t *testing.T) {
	// Byte 120 falls inside the 60th "é", so a cut there leaves half of it.
	got := closeReason("a" + strings.Repeat("é", 100))
	if want := "a" + strings.Repeat("é", 59) + "..."; got != want {
		t.Errorf("closeReason = %q, want %q", got, want)
	}
}

type fakeLogs struct {
	log  io.ReadCloser
	live bool
}

func (f fakeLogs) SandboxLog(context.Context, string, bool) (io.ReadCloser, error) { return f.log, nil }
func (f fakeLogs) JobLog(context.Context, string, int, bool) (io.ReadCloser, bool, error) {
	return f.log, f.live, nil
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
