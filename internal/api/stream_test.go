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

func (e *env) addSandbox(id string, state sandbox.State) {
	e.t.Helper()
	err := e.st.InsertSandbox(context.Background(), store.Sandbox{
		ID: id, PoolID: "p", ServerID: "s", Mode: sandbox.ModeContainer, State: state, VCPU: 2, MemoryMB: 4096,
		CreatedAt: time.Now(),
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// recv fails the test if ch gets no value in 5 seconds.
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("no value in 5 seconds")
		panic("unreachable")
	}
}

func wsURL(e *env, path string) string {
	return "ws" + strings.TrimPrefix(e.srv.URL, "http") + path
}

func TestShellBridgesTerminal(t *testing.T) {
	r, w := io.Pipe()
	shell := &fakeShell{opened: make(chan sandbox.TermSize, 1), term: &fakeTerm{
		output: r, input: make(chan string, 1), resized: make(chan sandbox.TermSize, 1),
	}}
	e := newEnv(t, func(d *Deps) { d.Shell = shell })
	e.addSandbox("sb1", sandbox.StateBusy)
	e.addSandbox("gone1", sandbox.StateGone)
	e.addSandbox("plain", sandbox.StateBusy)
	for _, id := range []string{"sb1", "gone1"} {
		if err := e.st.SetSandboxDebug(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	b := e.login("bob@example.com", auth.RoleMember)
	u, _ := url.Parse(e.srv.URL)
	cookie := b.client.Jar.Cookies(u)[0].String()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A page on another site cannot open the shell with the cookie.
	_, res, err := websocket.Dial(ctx, wsURL(e, "/api/sandboxes/sb1/shell"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {cookie}, "Origin": {"https://evil.example"}},
	})
	if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site dial: err %v, response %v; want 403", err, res)
	}
	if b.do("GET", "/api/sandboxes/gone1/shell", nil, nil).StatusCode != http.StatusConflict {
		t.Fatal("a shell to a gone sandbox did not get 409")
	}
	if b.do("GET", "/api/sandboxes/plain/shell", nil, nil).StatusCode != http.StatusForbidden {
		t.Fatal("a shell to a sandbox without debug did not get 403")
	}

	conn, _, err := websocket.Dial(ctx, wsURL(e, "/api/sandboxes/sb1/shell?rows=30&cols=100"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {cookie}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if got := recv(t, shell.opened); got != (sandbox.TermSize{Rows: 30, Cols: 100}) {
		t.Fatalf("shell opened with size %+v, want 30x100", got)
	}

	go w.Write([]byte("$ "))
	typ, data, err := conn.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(data) != "$ " {
		t.Fatalf("read prompt: %v %q %v", typ, data, err)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, shell.term.input); got != "ls\n" {
		t.Fatalf("terminal input %q, want ls", got)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","rows":40,"cols":120}`)); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, shell.term.resized); got != (sandbox.TermSize{Rows: 40, Cols: 120}) {
		t.Fatalf("resize %+v, want 40x120", got)
	}

	// The shell ends: the browser gets a normal close.
	w.Close()
	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("after the shell ends: %v, want a normal close", err)
	}

	entries, _, err := e.st.ListAudit(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	member, _ := e.st.GetUserByEmail(context.Background(), "bob@example.com")
	if len(entries) != 1 || entries[0].Action != "sandbox.shell" || entries[0].UserID != member.ID {
		t.Fatalf("audit = %+v, want one sandbox.shell entry by the member", entries)
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
	if err != nil {
		t.Fatal(err)
	}
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

// The live log of a running job has no step marks. A request for one step
// gets the whole log and a header that says so.
func TestLiveJobLogSaysThatTheStepFilterIsOff(t *testing.T) {
	for _, tc := range []struct {
		name   string
		live   bool
		query  string
		header string
	}{
		{"live log, one step", true, "?step=2", "ignored"},
		{"live log, all steps", true, "", ""},
		{"stored log, one step", false, "?step=2", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, func(d *Deps) {
				d.Logs = fakeLogs{log: io.NopCloser(strings.NewReader("line 1\n")), live: tc.live}
			})
			j := e.addJob(1, "acme/api", time.Now())
			b := e.login("bob@example.com", auth.RoleMember)
			res := b.do("GET", "/api/jobs/"+j.ID+"/log"+tc.query, nil, nil)
			res.want(t, 200)
			if got := res.Header.Get("X-Runplane-Step-Filter"); got != tc.header {
				t.Errorf("X-Runplane-Step-Filter = %q, want %q", got, tc.header)
			}
			if string(res.body) != "line 1\n" {
				t.Errorf("body %q, want the log", res.body)
			}
		})
	}
}
