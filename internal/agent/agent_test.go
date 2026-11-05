package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/cache"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/tlsutil"
)

const (
	goodToken  = "rp_srv_good"
	testServer = "01jbserver"
	testSecret = "s3cret"
)

func TestRegisterThenReconnect(t *testing.T) {
	logs := newLogBuffer(t)
	p := startPrimary(t, nil, logs)
	node := newFakeNode()
	a := startAgent(t, p.agentJSON(goodToken), node, nil, logs)

	s1 := recv(t, p.sessions)
	if s1.ServerID() != testServer {
		t.Fatalf("server ID %q, want %q", s1.ServerID(), testServer)
	}
	first := p.auth.hello(0)
	if first.Token != goodToken || first.ServerID != "" || first.Secret != "" {
		t.Errorf("first hello credentials: token %q server %q secret %q, want the token only", first.Token, first.ServerID, first.Secret)
	}
	if first.Mode != sandbox.ModeContainer || first.Host.Hostname != "agent-1" || first.Addr != "127.0.0.1" {
		t.Errorf("first hello: mode %q host %q addr %q", first.Mode, first.Host.Hostname, first.Addr)
	}

	// The agent writes agent.json before it serves the first request.
	ctx := testContext(t)
	if err := s1.Driver().Start(ctx, sandbox.Spec{ID: "sb1", PoolID: "pool1"}); err != nil {
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
	if !regexp.MustCompile(`"server_id": *"[^"]`).Match(raw) || !bytes.Contains(raw, []byte(`"primary": "`+p.addr+`"`)) {
		t.Errorf("install.sh would not find the registration in:\n%s", raw)
	}

	s1.Close()
	logs.waitFor(t, "connection to the primary ended")
	// An event while the agent has no connection arrives after it connects again.
	node.driver.events <- sandbox.Event{SandboxID: "sb1", Type: sandbox.EventState, State: sandbox.StateIdle}

	s2 := recv(t, p.sessions)
	second := p.auth.hello(1)
	if second.Token != "" || second.ServerID != testServer || second.Secret != testSecret {
		t.Errorf("second hello credentials: token %q server %q secret %q, want the server ID and secret", second.Token, second.ServerID, second.Secret)
	}
	if ev := recv(t, s2.Driver().Events()); ev.SandboxID != "sb1" || ev.State != sandbox.StateIdle {
		t.Errorf("event after the reconnect: %+v", ev)
	}
	list, err := s2.Driver().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "sb1" || len(node.driver.stoppedIDs()) != 0 {
		t.Errorf("after the reconnect: sandboxes %+v, stopped %v; want sb1 still there", list, node.driver.stoppedIDs())
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
		auth        bool // the primary sees a hello
	}{
		{name: "wrong fingerprint", fingerprint: otherFP, token: goodToken, log: "does not match pinned", auth: false},
		{name: "used token", token: "rp_srv_used", log: "primary refused the agent: the install token is not valid", auth: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := newLogBuffer(t)
			p := startPrimary(t, nil, logs)
			f := p.agentJSON(tt.token)
			if tt.fingerprint != "" {
				f = strings.Replace(f, p.fp, tt.fingerprint, 1)
			}
			a := startAgent(t, f, newFakeNode(), nil, logs)
			logs.waitFor(t, tt.log)

			if got := p.auth.count() > 0; got != tt.auth {
				t.Errorf("primary saw a hello: %v, want %v", got, tt.auth)
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
	logs := newLogBuffer(t)
	p := startPrimary(t, nil, logs)
	startAgent(t, p.registeredJSON(), newFakeNode(), nil, logs)
	s1 := recv(t, p.sessions)
	startAgent(t, p.registeredJSON(), newFakeNode(), nil, logs)
	recv(t, p.sessions)
	select {
	case <-s1.Done():
	default:
		t.Error("the old session of the server is still open")
	}
}

func TestRemoteDriver(t *testing.T) {
	_, node, s := connect(t)
	d := s.Driver()
	ctx := testContext(t)

	if d.Mode() != sandbox.ModeContainer {
		t.Errorf("mode %q", d.Mode())
	}
	spec := sandbox.Spec{
		ID: "sb1", PoolID: "pool1",
		Image:         sandbox.ImageRef{Ref: "builtin:ubuntu-24.04", Digest: "sha256:5f1c"},
		RunnerVersion: "2.330.0", Arch: "arm64", VCPU: 2, MemoryMB: 4096, DiskGB: 20,
		TimeLimit: 6 * time.Hour, JITConfig: "jit-secret",
		Env:   map[string]string{"ACTIONS_RESULTS_URL": "http://10.0.0.1:8486/"},
		Debug: true, Labels: map[string]string{sandbox.LabelSandbox: "sb1"},
	}
	if err := d.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := node.driver.spec("sb1"); !reflect.DeepEqual(got, spec) {
		t.Errorf("the agent driver got\n%+v\nwant\n%+v", got, spec)
	}
	if err := d.Start(ctx, spec); err == nil || !strings.Contains(err.Error(), "sandbox sb1 exists") {
		t.Errorf("second start: %v, want the driver error", err)
	}

	list, err := d.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "sb1" || list[0].State != sandbox.StateStarting || list[0].Spec.VCPU != 2 {
		t.Errorf("list: %+v", list)
	}
	c, err := d.Capacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c != fakeCapacity {
		t.Errorf("capacity %+v, want %+v", c, fakeCapacity)
	}

	until := time.Date(2026, 11, 4, 12, 30, 0, 0, time.UTC)
	if err := d.Hold(ctx, "sb1", until); err != nil {
		t.Fatal(err)
	}
	if got := node.driver.hold("sb1"); !got.Equal(until) {
		t.Errorf("hold until %v, want %v", got, until)
	}

	if err := d.Stop(ctx, "sb1"); err != nil {
		t.Fatal(err)
	}
	if got := node.driver.stoppedIDs(); !reflect.DeepEqual(got, []string{"sb1"}) {
		t.Errorf("stopped %v", got)
	}
	if list, err := d.List(ctx); err != nil || len(list) != 0 {
		t.Errorf("list after stop: %v, %v", list, err)
	}
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
		node.driver.events <- ev
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
	node.driver.put(sandbox.Sandbox{ID: "sb1", State: sandbox.StateBusy})
	ctx := testContext(t)

	if _, err := s.Driver().Logs(ctx, "nope", true); err == nil || !strings.Contains(err.Error(), "no sandbox nope") {
		t.Errorf("logs of an unknown sandbox: %v, want the driver error", err)
	}

	rc, err := s.Driver().Logs(ctx, "sb1", true)
	if err != nil {
		t.Fatal(err)
	}
	stream := recv(t, node.driver.logs)
	// Each line must arrive while the log is still open.
	for _, line := range []string{"1762246800000 runner Listening for Jobs\n", "1762246801000 runner Running job: build\n"} {
		go stream.w.Write([]byte(line))
		if got := string(readFull(t, rc, len(line))); got != line {
			t.Errorf("read %q, want %q", got, line)
		}
	}
	// Closing the reader on the primary stops the log reader on the agent.
	rc.Close()
	waitClosed(t, stream.done)
}

func TestShell(t *testing.T) {
	_, node, s := connect(t)
	node.driver.put(sandbox.Sandbox{ID: "sb1", State: sandbox.StateIdle})
	ctx := testContext(t)

	term, err := s.Driver().Shell(ctx, "sb1", sandbox.TermSize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	ft := recv(t, node.driver.shells)
	if ft.size != (sandbox.TermSize{Rows: 24, Cols: 80}) {
		t.Errorf("open size %+v, want 24x80", ft.size)
	}

	if _, err := term.Write([]byte("echo hi\n")); err != nil {
		t.Fatal(err)
	}
	if got := string(readFull(t, term, 8)); got != "echo hi\n" {
		t.Errorf("echo %q", got)
	}
	// A write larger than one frame arrives whole and in order.
	big := make([]byte, 100<<10)
	for i := range big {
		big[i] = byte(i % 251)
	}
	go term.Write(big)
	if got := readFull(t, term, len(big)); !bytes.Equal(got, big) {
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
	term2, err := s.Driver().Shell(ctx, "sb1", sandbox.TermSize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	recv(t, node.driver.shells).Close()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(term2)
		done <- err
	}()
	if err := recv(t, done); err != nil {
		t.Errorf("read after the shell ended: %v, want EOF", err)
	}
}

func TestEnsureImage(t *testing.T) {
	_, _, s := connect(t)
	ctx := testContext(t)

	var got []image.Progress
	ref, err := s.EnsureImage(ctx, "ghcr.io/acme/runner:1", func(p image.Progress) { got = append(got, p) })
	if err != nil {
		t.Fatal(err)
	}
	if want := (sandbox.ImageRef{Ref: "ghcr.io/acme/runner:1", Digest: "sha256:5f1c"}); ref != want {
		t.Errorf("image %+v, want %+v", ref, want)
	}
	want := []image.Progress{
		{Status: image.StatusPending},
		{Status: image.StatusPulling, Done: 50, Total: 100},
		{Status: image.StatusPulling, Done: 100, Total: 100},
		{Status: image.StatusReady},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("progress %+v, want %+v", got, want)
	}

	if _, err := s.EnsureImage(ctx, "missing:latest", nil); err == nil || !strings.Contains(err.Error(), "pull missing:latest: not found") {
		t.Errorf("missing image: %v, want the pull error", err)
	}
}

func TestNodeCalls(t *testing.T) {
	a, node, s := connect(t)
	ctx := testContext(t)

	info, err := s.Facts(ctx)
	if err != nil || !reflect.DeepEqual(info, testInfo) {
		t.Errorf("facts %+v, %v", info, err)
	}
	checks, err := s.Doctor(ctx)
	if err != nil || !reflect.DeepEqual(checks, testChecks) {
		t.Errorf("doctor %+v, %v", checks, err)
	}
	b, err := s.EnsureRunner(ctx, "2.330.0", "arm64")
	if err != nil || b.Version != "2.330.0" || b.Arch != "arm64" || !b.Patched {
		t.Errorf("runner bundle %+v, %v", b, err)
	}
	if m, err := s.MirrorStats(ctx); err != nil || m != testMirror {
		t.Errorf("mirror stats %+v, %v; want %+v", m, err, testMirror)
	}
	if err := s.GCImages(ctx, []string{"sha256:5f1c", "sha256:9e2f"}); err != nil {
		t.Fatal(err)
	}
	node.mu.Lock()
	gcKeeps := node.gcKeeps
	node.mu.Unlock()
	if !reflect.DeepEqual(gcKeeps, [][]string{{"sha256:5f1c", "sha256:9e2f"}}) {
		t.Errorf("image GC calls %v", gcKeeps)
	}

	settings := sandbox.Settings{
		ReserveVCPU: 1, ReserveMemoryMB: 2048, Overcommit: 1.5, MinFreeDiskPercent: 15,
		DNS: []string{"1.1.1.1"}, EgressAllowlist: []string{"github.com"}, BandwidthMbps: 100,
		RegistryMirror: true, ActionsCache: true,
	}
	if err := s.Configure(ctx, settings); err != nil {
		t.Fatal(err)
	}
	node.mu.Lock()
	gotSettings := node.settings
	node.mu.Unlock()
	if !reflect.DeepEqual(gotSettings, []sandbox.Settings{settings}) {
		t.Errorf("settings %+v", gotSettings)
	}

	if err := s.Update(ctx, "v0.5.0"); err != nil {
		t.Fatal(err)
	}
	if err := a.result(t); !errors.Is(err, ErrUpdated) {
		t.Errorf("Run after the update: %v, want ErrUpdated", err)
	}
	node.mu.Lock()
	updates := node.updates
	node.mu.Unlock()
	if !reflect.DeepEqual(updates, []string{"v0.5.0"}) {
		t.Errorf("updates %v", updates)
	}
	waitClosed(t, s.Done())
}

func TestDisconnectEndsSession(t *testing.T) {
	a, _, s := connect(t)
	a.cancel()
	if err := a.result(t); err != nil {
		t.Errorf("Run: %v, want nil after ctx ends", err)
	}
	waitClosed(t, s.Done())
	waitClosed(t, s.Driver().Events())
	if err := s.Driver().Start(testContext(t), sandbox.Spec{ID: "sb1"}); !errors.Is(err, ErrClosed) {
		t.Errorf("start after the disconnect: %v, want ErrClosed", err)
	}
}

func TestCacheProxy(t *testing.T) {
	type cacheCall struct {
		path, query, body string
		header            http.Header
	}
	calls := make(chan cacheCall, 4)
	primaryCache := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls <- cacheCall{path: r.URL.Path, query: r.URL.RawQuery, body: string(body), header: r.Header}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"signed_upload_url":"x"}`)
	})
	logs := newLogBuffer(t)
	p := startPrimary(t, primaryCache, logs)
	node := newFakeNode()
	node.driver.put(sandbox.Sandbox{ID: "sb-here", State: sandbox.StateBusy, IP: "127.0.0.1"})
	proxy := NewCacheProxy(node.driver, nil)
	startAgent(t, p.agentJSON(goodToken), node, proxy, logs)
	// The agent attaches the proxy before it answers the first call.
	if _, err := recv(t, p.sessions).Driver().List(testContext(t)); err != nil {
		t.Fatal(err)
	}
	svc := httptest.NewServer(proxy)
	defer svc.Close()

	post := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, svc.URL+"/twirp/github.actions.results.api.v1.CacheService/CreateCacheEntry?v=2", strings.NewReader(`{"key":"npm-linux"}`))
		// A sandbox must not choose its identity.
		req.Header.Set(cache.SandboxHeader, "sb-other")
		req.Header.Set(ServerHeader, "other-server")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := post()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true,"signed_upload_url":"x"}` {
		t.Errorf("response %d %s", resp.StatusCode, body)
	}
	call := recv(t, calls)
	if call.path != "/twirp/github.actions.results.api.v1.CacheService/CreateCacheEntry" || call.query != "v=2" || call.body != `{"key":"npm-linux"}` {
		t.Errorf("primary got path %q query %q body %q", call.path, call.query, call.body)
	}
	wantHeaders := map[string]string{
		cache.SandboxHeader: "sb-here",
		ServerHeader:        testServer,
		"X-Forwarded-Host":  svc.Listener.Addr().String(),
		"X-Forwarded-Proto": "http",
	}
	for k, v := range wantHeaders {
		if got := call.header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}

	node.driver.put(sandbox.Sandbox{ID: "sb-here", State: sandbox.StateBusy, IP: "10.1.2.3"})
	resp = post()
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(calls) != 0 {
		t.Errorf("request from an address with no sandbox: status %d, %d calls on the primary; want 403 and none", resp.StatusCode, len(calls))
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := Run(ctx, Config{Path: path, Node: newFakeNode()}); err == nil {
				t.Error("Run accepted the config")
			}
		})
	}
}

// Test harness: a primary and agents over TLS on 127.0.0.1.

type testPrimary struct {
	addr     string
	fp       string
	sessions chan *Session
	auth     *fakeAuth
}

func startPrimary(t *testing.T, cacheHandler http.Handler, logs *logBuffer) *testPrimary {
	t.Helper()
	cert, fp, err := tlsutil.AgentCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &testPrimary{addr: ln.Addr().String(), fp: fp, sessions: make(chan *Session, 8), auth: &fakeAuth{}}
	l := &Listener{
		Cert:         cert,
		Authenticate: p.auth.authenticate,
		OnSession:    func(s *Session) { p.sessions <- s },
		CacheHandler: cacheHandler,
		Logger:       logs.logger("primary"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return p
}

// agentJSON is the agent.json that install.sh writes.
func (p *testPrimary) agentJSON(token string) string {
	return fmt.Sprintf("{\n  \"primary\": %q,\n  \"fingerprint\": %q,\n  \"token\": %q\n}\n", p.addr, p.fp, token)
}

func (p *testPrimary) registeredJSON() string {
	return fmt.Sprintf(`{"primary":%q,"fingerprint":%q,"server_id":%q,"secret":%q}`, p.addr, p.fp, testServer, testSecret)
}

// fakeAuth accepts goodToken one time and then the server ID and secret
// that it gave.
type fakeAuth struct {
	mu     sync.Mutex
	hellos []Hello
	used   bool
}

func (a *fakeAuth) authenticate(_ context.Context, h Hello) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hellos = append(a.hellos, h)
	switch {
	case h.Token == goodToken && !a.used:
		a.used = true
		return testServer, testSecret, nil
	case h.Token != "":
		return "", "", errors.New("the install token is not valid")
	case h.ServerID == testServer && h.Secret == testSecret:
		return testServer, "", nil
	}
	return "", "", errors.New("unknown server")
}

func (a *fakeAuth) hello(i int) Hello {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hellos[i]
}

func (a *fakeAuth) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.hellos)
}

type testAgent struct {
	path   string
	cancel context.CancelFunc
	done   chan error

	once sync.Once
	err  error
}

func startAgent(t *testing.T, agentJSON string, node *fakeNode, proxy *CacheProxy, logs *logBuffer) *testAgent {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(agentJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &testAgent{path: path, cancel: cancel, done: make(chan error, 1)}
	go func() {
		a.done <- Run(ctx, Config{Path: path, Node: node, CacheProxy: proxy, Mirror: func() mirror.Stats { return testMirror },
			Logger: logs.logger("agent")})
	}()
	t.Cleanup(func() {
		cancel()
		a.result(t)
	})
	return a
}

// result waits for Run to return.
func (a *testAgent) result(t *testing.T) error {
	t.Helper()
	a.once.Do(func() { a.err = recv(t, a.done) })
	return a.err
}

// connect starts a primary and a registered agent.
func connect(t *testing.T) (*testAgent, *fakeNode, *Session) {
	t.Helper()
	logs := newLogBuffer(t)
	p := startPrimary(t, nil, logs)
	node := newFakeNode()
	a := startAgent(t, p.agentJSON(goodToken), node, nil, logs)
	return a, node, recv(t, p.sessions)
}

// testContext ends calls that hang, so that a test fails instead.
func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	var zero T
	return zero
}

// waitClosed waits until ch closes. It drops the values that come before.
func waitClosed[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for the channel to close")
		}
	}
}

func readFull(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b := make([]byte, n)
		_, err := io.ReadFull(r, b)
		ch <- result{b, err}
	}()
	res := recv(t, ch)
	if res.err != nil {
		t.Fatalf("read: %v", res.err)
	}
	return res.b
}

// logBuffer keeps the logs of a test and prints them when the test fails.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newLogBuffer(t *testing.T) *logBuffer {
	l := &logBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("logs:\n%s", l.String())
		}
	})
	return l
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logBuffer) logger(side string) *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug})).With("side", side)
}

func (l *logBuffer) waitFor(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(l.String(), text) {
		if time.Now().After(deadline) {
			t.Fatalf("no log line with %q", text)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
