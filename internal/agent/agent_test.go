package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/cache"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/runner"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/tlsutil"
)

func TestRegisterThenReconnect(t *testing.T) {
	p := startPrimary(t, nil)
	node := newFakeNode()
	a := p.startAgent(p.agentJSON(goodToken), node, nil)

	s1 := recv(t, p.sessions)
	if s1.ServerID() != testServer {
		t.Fatalf("server ID %q, want %q", s1.ServerID(), testServer)
	}
	first := recv(t, p.hellos)
	if first.Token != goodToken || first.ServerID != "" || first.Secret != "" {
		t.Errorf("first hello credentials: token %q server %q secret %q, want the token only", first.Token, first.ServerID, first.Secret)
	}
	if first.Mode != sandbox.ModeContainer || first.Host.Hostname != "agent-1" || first.Addr != "127.0.0.1" {
		t.Errorf("first hello: mode %q host %q addr %q", first.Mode, first.Host.Hostname, first.Addr)
	}

	// The agent writes agent.json before it serves the first request.
	if err := s1.Driver().Start(t.Context(), sandbox.Spec{ID: "sb1", PoolID: "pool1"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]string
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"primary": p.addr, "fingerprint": p.fp, "server_id": testServer, "secret": testSecret}
	if !maps.Equal(file, want) {
		t.Errorf("agent.json after registration:\n%s\nwant the fields %v and no token", raw, want)
	}
	if st, _ := os.Stat(a.path); st.Mode().Perm() != 0o600 {
		t.Errorf("agent.json mode %v, want 0600", st.Mode().Perm())
	}
	// install.sh keeps a registered agent.json when it finds these lines.
	if !regexp.MustCompile(`"server_id": *"[^"]`).Match(raw) || !strings.Contains(string(raw), `"primary": "`+p.addr+`"`) {
		t.Errorf("install.sh would not find the registration in:\n%s", raw)
	}

	s1.Close()
	p.logs.waitFor(t, "connection to the primary ended")
	// An event while the agent has no connection arrives after it connects again.
	node.events <- sandbox.Event{SandboxID: "sb1", Type: sandbox.EventState, State: sandbox.StateIdle}

	s2 := recv(t, p.sessions)
	second := recv(t, p.hellos)
	if second.Token != "" || second.ServerID != testServer || second.Secret != testSecret {
		t.Errorf("second hello credentials: token %q server %q secret %q, want the server ID and secret", second.Token, second.ServerID, second.Secret)
	}
	if ev := recv(t, s2.Driver().Events()); ev.SandboxID != "sb1" || ev.State != sandbox.StateIdle {
		t.Errorf("event after the reconnect: %+v", ev)
	}
	if list, err := s2.Driver().List(t.Context()); err != nil || len(list) != 1 || list[0].ID != "sb1" {
		t.Errorf("sandboxes after the reconnect: %+v, %v; want sb1 still there", list, err)
	}
}

func TestRefusedAgentKeepsItsConfig(t *testing.T) {
	_, otherFP, err := tlsutil.AgentCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		fingerprint string // empty: the primary's
		token       string
		log         string
		hello       bool // the primary sees a hello
	}{
		{name: "wrong fingerprint", fingerprint: otherFP, token: goodToken, log: "does not match pinned"},
		{name: "used token", token: "rp_srv_used", log: "primary refused the agent: the install token is not valid", hello: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := startPrimary(t, nil)
			f := p.agentJSON(tt.token)
			if tt.fingerprint != "" {
				f = strings.Replace(f, p.fp, tt.fingerprint, 1)
			}
			a := p.startAgent(f, newFakeNode(), nil)
			p.logs.waitFor(t, tt.log)

			if got := len(p.hellos) > 0; got != tt.hello {
				t.Errorf("primary saw a hello: %v, want %v", got, tt.hello)
			}
			if len(p.sessions) != 0 {
				t.Error("the primary made a session")
			}
			if raw, _ := os.ReadFile(a.path); string(raw) != f {
				t.Errorf("agent.json changed:\n%s", raw)
			}
		})
	}
}

func TestNewSessionClosesOldSessionOfServer(t *testing.T) {
	p := startPrimary(t, nil)
	registered := fmt.Sprintf(`{"primary":%q,"fingerprint":%q,"server_id":%q,"secret":%q}`, p.addr, p.fp, testServer, testSecret)
	p.startAgent(registered, newFakeNode(), nil)
	s1 := recv(t, p.sessions)
	p.startAgent(registered, newFakeNode(), nil)
	recv(t, p.sessions)
	select {
	case <-s1.Done():
	default:
		t.Error("the old session of the server is still open")
	}
}

// TestCalls makes the calls of a Session in order on one agent.
func TestCalls(t *testing.T) {
	a, node, s := connect(t)
	ctx := t.Context()
	// check compares the result of a call and the calls that the node got.
	check := func(name string, got any, err error, want any, calls ...[]any) {
		t.Helper()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %+v, %v; want %+v", name, got, err, want)
		}
		if got := node.takeCalls(); !reflect.DeepEqual(got, calls) {
			t.Errorf("%s: the node got %v, want %v", name, got, calls)
		}
	}
	checkErr := func(name string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want the error %q", name, err, want)
		}
	}

	spec := sandbox.Spec{
		ID: "sb1", PoolID: "pool1",
		Image:         sandbox.ImageRef{Ref: "builtin:ubuntu-24.04", Digest: "sha256:5f1c"},
		RunnerVersion: "2.330.0", Arch: "arm64", VCPU: 2, MemoryMB: 4096, DiskGB: 20,
		TimeLimit: 6 * time.Hour, JITConfig: "jit-secret",
		Env:   map[string]string{"ACTIONS_RESULTS_URL": "http://10.0.0.1:8486/"},
		Debug: true, Labels: map[string]string{sandbox.LabelSandbox: "sb1"},
	}
	check("mode", s.Mode(), nil, sandbox.ModeContainer)
	check("start", nil, s.Start(ctx, spec), nil, []any{"Start", spec})
	checkErr("start again", s.Start(ctx, spec), "sandbox sb1 exists")
	stored := spec
	stored.JITConfig = ""
	list, err := s.List(ctx)
	check("list", list, err, []sandbox.Sandbox{{ID: "sb1", PoolID: "pool1", State: sandbox.StateStarting, Spec: stored}})
	c, err := s.Capacity(ctx)
	check("capacity", c, err, fakeCapacity)
	until := time.Date(2026, 11, 4, 12, 30, 0, 0, time.UTC)
	check("hold", nil, s.Hold(ctx, "sb1", until), nil, []any{"Hold", "sb1", until})
	check("stop", nil, s.Stop(ctx, "sb1"), nil, []any{"Stop", "sb1"})
	list, err = s.List(ctx)
	check("list after stop", list, err, []sandbox.Sandbox{})

	info, err := s.Facts(ctx)
	check("facts", info, err, testInfo)
	checks, err := s.Doctor(ctx)
	check("doctor", checks, err, testChecks)
	var steps []image.Progress
	ref, err := s.EnsureImage(ctx, "ghcr.io/acme/runner:1", func(p image.Progress) { steps = append(steps, p) })
	check("image", ref, err, sandbox.ImageRef{Ref: "ghcr.io/acme/runner:1", Digest: "sha256:5f1c"})
	check("image progress", steps, nil, testProgress)
	_, err = s.EnsureImage(ctx, "missing:latest", nil)
	checkErr("missing image", err, "pull missing:latest: not found")
	b, err := s.EnsureRunner(ctx, "2.330.0", "arm64")
	check("runner", b, err, runner.Bundle{Version: "2.330.0", Arch: "arm64", SHA256: "9e2f", Patched: true, Dir: "/var/lib/runplane/runners/2.330.0-arm64"})
	m, err := s.MirrorStats(ctx)
	check("mirror stats", m, err, testMirror)
	keep := []string{"sha256:5f1c", "sha256:9e2f"}
	check("image GC", nil, s.GCImages(ctx, keep), nil, []any{"GCImages", keep})
	check("runner prune", nil, s.PruneRunners(ctx, []string{"2.329.0"}), nil, []any{"PruneRunners", []string{"2.329.0"}})
	settings := sandbox.Settings{
		ReserveVCPU: 1, ReserveMemoryMB: 2048, Overcommit: 1.5, MinFreeDiskPercent: 15,
		DNS: []string{"1.1.1.1"}, EgressAllowlist: []string{"github.com"}, BandwidthMbps: 100,
		RegistryMirror: true, ActionsCache: true,
	}
	check("settings", nil, s.Configure(ctx, settings), nil, []any{"Configure", settings})

	err = s.Update(ctx, "v0.5.0")
	check("update", nil, err, nil, []any{"Update", "v0.5.0"})
	if err := a.result(t); !errors.Is(err, ErrUpdated) {
		t.Errorf("Run after the update: %v, want ErrUpdated", err)
	}
	waitClosed(t, s.Done())
}

func TestEventsAndStats(t *testing.T) {
	_, node, s := connect(t)
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	events := []sandbox.Event{
		{SandboxID: "sb1", Type: sandbox.EventState, Time: at, State: sandbox.StateStarting},
		{SandboxID: "sb1", Type: sandbox.EventRunner, Time: at, Runner: &sandbox.RunnerEvent{Kind: sandbox.RunnerJobStarted, JobName: "build"}},
		{SandboxID: "sb1", Type: sandbox.EventMetrics, Time: at, Metrics: &sandbox.Metrics{CPUPercent: 42.5, MemoryMB: 900}},
		{SandboxID: "sb1", Type: sandbox.EventState, Time: at, State: sandbox.StateFailed, Reason: "runner exited with code 1"},
	}
	for _, ev := range events {
		node.events <- ev
	}
	for i, want := range events {
		if got := recv(t, s.Driver().Events()); !reflect.DeepEqual(got, want) {
			t.Errorf("event %d: got %+v, want %+v", i, got, want)
		}
	}

	stats := host.Stats{At: at, CPUPercent: 12.5, MemoryPercent: 40, DiskPercent: 70, Resources: host.Resources{VCPU: 16, MemoryMB: 65536, DiskBytes: 500e9, FreeDiskBytes: 150e9}}
	node.stats <- stats
	if got := recv(t, s.Stats()); got != stats {
		t.Errorf("stats %+v, want %+v", got, stats)
	}
}

func TestLogsFollow(t *testing.T) {
	_, node, s := connect(t)
	node.put(sandbox.Sandbox{ID: "sb1", State: sandbox.StateBusy})
	if _, err := s.Driver().Logs(t.Context(), "nope", true); err == nil || !strings.Contains(err.Error(), "no sandbox nope") {
		t.Errorf("logs of an unknown sandbox: %v, want the driver error", err)
	}
	rc, err := s.Driver().Logs(t.Context(), "sb1", true)
	if err != nil {
		t.Fatal(err)
	}
	stream := recv(t, node.logs)
	if calls := node.takeCalls(); !reflect.DeepEqual(calls, [][]any{{"Logs", "sb1", true}}) {
		t.Errorf("the node got %v, want the logs of sb1 with follow", calls)
	}
	// Each line must arrive while the log is still open.
	for _, line := range []string{"1762246800000 runner Listening for Jobs\n", "1762246801000 runner Running job: build\n"} {
		go stream.w.Write([]byte(line))
		if got := readFull(t, rc, len(line)); got != line {
			t.Errorf("read %q, want %q", got, line)
		}
	}
	// Closing the reader on the primary stops the log reader on the agent.
	rc.Close()
	waitClosed(t, stream.done)
}

func TestShell(t *testing.T) {
	_, node, s := connect(t)
	term, err := s.Driver().Shell(t.Context(), "sb1", sandbox.TermSize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	ft := recv(t, node.shells)
	if got := recv(t, ft.sizes); got != (sandbox.TermSize{Rows: 24, Cols: 80}) {
		t.Errorf("open size %+v, want 24x80", got)
	}

	if _, err := term.Write([]byte("echo hi\n")); err != nil {
		t.Fatal(err)
	}
	if got := readFull(t, term, 8); got != "echo hi\n" {
		t.Errorf("echo %q", got)
	}
	// A write larger than one frame arrives whole and in order.
	big := make([]byte, 100<<10)
	for i := range big {
		big[i] = byte(i % 251)
	}
	go term.Write(big)
	if got := readFull(t, term, len(big)); got != string(big) {
		t.Error("a 100 KiB write came back changed")
	}

	if err := term.Resize(sandbox.TermSize{Rows: 50, Cols: 120}); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, ft.sizes); got != (sandbox.TermSize{Rows: 50, Cols: 120}) {
		t.Errorf("resize %+v, want 50x120", got)
	}
	term.Close()
	waitClosed(t, ft.closed)

	// When the shell ends on the agent, the terminal on the primary ends.
	term2, err := s.Driver().Shell(t.Context(), "sb1", sandbox.TermSize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	recv(t, node.shells).Close()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(term2)
		done <- err
	}()
	if err := recv(t, done); err != nil {
		t.Errorf("read after the shell ended: %v, want EOF", err)
	}
}

func TestDisconnectEndsSession(t *testing.T) {
	a, _, s := connect(t)
	a.cancel()
	if err := a.result(t); err != nil {
		t.Errorf("Run: %v, want nil after ctx ends", err)
	}
	waitClosed(t, s.Done())
	waitClosed(t, s.Driver().Events())
	if err := s.Driver().Start(t.Context(), sandbox.Spec{ID: "sb1"}); !errors.Is(err, ErrClosed) {
		t.Errorf("start after the disconnect: %v, want ErrClosed", err)
	}
}

func TestCacheProxy(t *testing.T) {
	type cacheCall struct {
		uri, body string
		header    http.Header
	}
	calls := make(chan cacheCall, 4)
	p := startPrimary(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls <- cacheCall{r.URL.RequestURI(), string(body), r.Header}
		io.WriteString(w, `{"ok":true}`)
	}))
	node := newFakeNode()
	node.put(sandbox.Sandbox{ID: "sb-here", State: sandbox.StateBusy, IP: "127.0.0.1"})
	proxy := NewCacheProxy(node, p.logs.logger("cache"))
	p.startAgent(p.agentJSON(goodToken), node, proxy)
	// The agent attaches the proxy before it answers the first call.
	if _, err := recv(t, p.sessions).Driver().List(t.Context()); err != nil {
		t.Fatal(err)
	}
	svc := httptest.NewServer(proxy)
	defer svc.Close()
	post := func() (int, string) {
		req, _ := http.NewRequest(http.MethodPost, svc.URL+"/twirp/CacheService/CreateCacheEntry?v=2", strings.NewReader(`{"key":"npm-linux"}`))
		// A sandbox must not choose its identity.
		req.Header.Set(cache.SandboxHeader, "sb-other")
		req.Header.Set(cache.ServerHeader, "other-server")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if status, body := post(); status != http.StatusOK || body != `{"ok":true}` {
		t.Errorf("response %d %s", status, body)
	}
	call := recv(t, calls)
	if call.uri != "/twirp/CacheService/CreateCacheEntry?v=2" || call.body != `{"key":"npm-linux"}` {
		t.Errorf("primary got %q with body %q", call.uri, call.body)
	}
	wantHeaders := map[string]string{
		cache.SandboxHeader: "sb-here",
		cache.ServerHeader:  testServer,
		"X-Forwarded-Host":  svc.Listener.Addr().String(),
		"X-Forwarded-Proto": "http",
	}
	for k, v := range wantHeaders {
		if got := call.header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}

	node.put(sandbox.Sandbox{ID: "sb-here", State: sandbox.StateBusy, IP: "10.1.2.3"})
	if status, _ := post(); status != http.StatusForbidden || len(calls) != 0 {
		t.Errorf("request from an address with no sandbox: status %d, %d calls on the primary; want 403 and none", status, len(calls))
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	fp := "sha256:" + strings.Repeat("ab", 32)
	tests := map[string]string{
		"no token or secret":       `{"primary":"10.0.0.1:8485","fingerprint":"` + fp + `"}`,
		"server ID without secret": `{"primary":"10.0.0.1:8485","fingerprint":"` + fp + `","server_id":"01srv"}`,
		"short fingerprint":        `{"primary":"10.0.0.1:8485","fingerprint":"sha256:abcd","token":"rp_srv_x"}`,
		"no port":                  `{"primary":"10.0.0.1","fingerprint":"` + fp + `","token":"rp_srv_x"}`,
		"not JSON":                 `primary=10.0.0.1:8485`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := Run(ctx, Config{Path: path, Node: newFakeNode()}); err == nil {
				t.Error("Run accepted the config")
			}
		})
	}
}
