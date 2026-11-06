package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/backup"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/fleet"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/pools"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/settings"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/tlsutil"
)

type env struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
	d   Deps
}

// newEnv starts the API on a new database. with sets services in Deps. The
// other services are fakes that do nothing, or that panic in a method that
// the test does not expect.
func newEnv(t *testing.T, with ...func(*Deps)) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "runplane.db"))
	must(t, err)
	t.Cleanup(func() { st.Close() })
	box, err := secret.NewBox(bytes.Repeat([]byte{7}, secret.KeySize))
	must(t, err)
	cfg := config.Default()
	cfg.DataDir, cfg.ConfigDir, cfg.Mode = dir, dir, sandbox.ModeContainer
	bus := &events.Bus{}
	d := Deps{
		Store: st, Auth: auth.NewService(st, []byte("device key"), nil), Channels: notify.NewChannels(st, box, nil),
		Bus: bus, Box: box, Config: cfg,
		Scheduler: okScheduler{}, GitHub: struct{ GitHub }{}, Servers: &fakeServers{}, Images: struct{ Images }{},
		Caches: fakeCaches{}, Mirrors: fakeMirrors{}, Backups: backup.New(backup.Config{Store: st, Box: box, Bus: bus, Dir: cfg.BackupsDir()}),
		Updater: fakeUpdater{}, Logs: fakeLogs{}, Shell: struct{ Shell }{}, HostChecker: fakeHostChecker{},
		InstallScript: struct{ InstallScript }{}, HTTPS: fakeHTTPS{},
	}
	for _, f := range with {
		f(&d)
	}
	d.Settings = settings.New(settings.Config{
		Store: st, Bus: d.Bus, Mode: d.Config.Mode, DevInsecure: d.Config.DevInsecure, KVM: func() bool { return false },
	})
	d.Pools = pools.New(pools.Config{Store: st, Bus: d.Bus, Arch: "x64"})
	h := New(d)
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		h.Close()
		srv.Close()
	})
	return &env{t: t, srv: srv, st: st, d: d}
}

type fakeHostChecker struct{}

func (fakeHostChecker) Check(context.Context) (fleet.HostCheck, error) {
	return fleet.HostCheck{Mode: sandbox.ModeContainer}, nil
}

type fakeHTTPS struct{}

func (fakeHTTPS) Status(context.Context) tlsutil.CertStatus { return tlsutil.CertStatus{State: "none"} }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// browser is an HTTP client with a cookie jar, like the dashboard.
type browser struct {
	e      *env
	client *http.Client
	header http.Header // sent with each request; has the CSRF header
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	b := &browser{e: e, header: http.Header{}, client: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	b.header.Set("X-Runplane-CSRF", "1")
	return b
}

type response struct {
	*http.Response
	body []byte
}

// do sends a request and decodes the JSON body of a success into out.
func (b *browser) do(method, path string, body any, out any) response {
	t := b.e.t
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		must(t, err)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, b.e.srv.URL+path, rd)
	must(t, err)
	maps.Copy(req.Header, b.header)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := b.client.Do(req)
	must(t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	must(t, err)
	if out != nil && res.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
		}
	}
	return response{res, raw}
}

func (r response) want(t *testing.T, status int) {
	t.Helper()
	if r.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d: %s", r.Request.Method, r.Request.URL.Path, r.StatusCode, status, r.body)
	}
}

// wantField checks that the response is a problem with status and one field
// error, at loc.
func (r response) wantField(t *testing.T, status int, loc string) {
	t.Helper()
	r.want(t, status)
	var p struct {
		Errors []struct{ Location string } `json:"errors"`
	}
	must(t, json.Unmarshal(r.body, &p))
	if len(p.Errors) != 1 || p.Errors[0].Location != loc {
		t.Fatalf("%s %s: errors %+v, want one at %s", r.Request.Method, r.Request.URL.Path, p.Errors, loc)
	}
}

type sseEvent struct{ name, data string }

// events opens the SSE stream of b and sends its events to the channel.
func (b *browser) events(t *testing.T) <-chan sseEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", b.e.srv.URL+"/api/events", nil)
	res, err := b.client.Do(req)
	must(t, err)
	if res.StatusCode != 200 {
		t.Fatalf("events: status %d", res.StatusCode)
	}
	ch := make(chan sseEvent, 64)
	go func() {
		defer res.Body.Close()
		r := bufio.NewReader(res.Body)
		var name string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\n")
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				ch <- sseEvent{name, v}
			}
		}
	}()
	return ch
}

// publish sends the events again each 20 ms until the test ends, because a
// stream subscribes to the bus only after it sent its headers.
func (e *env) publish(evs ...events.Event) {
	stop := make(chan struct{})
	e.t.Cleanup(func() { close(stop) })
	go func() {
		for {
			for _, ev := range evs {
				e.d.Bus.Publish(ev)
			}
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
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

func (e *env) addUser(email, role string) {
	e.t.Helper()
	must(e.t, e.st.CreateUser(context.Background(), store.User{
		ID: ids.New(), Email: email, Name: email, PasswordHash: auth.HashPassword("correct horse battery"),
		Role: role, CreatedAt: time.Now(),
	}))
}

// login returns a browser with a session of a new user with role.
func (e *env) login(email, role string) *browser {
	e.t.Helper()
	e.addUser(email, role)
	b := e.browser()
	b.do("POST", "/api/session", map[string]string{"email": email, "password": "correct horse battery"}, nil).want(e.t, 200)
	return b
}

func (e *env) addJob(requestID int64, repo string, queued time.Time) store.Job {
	e.t.Helper()
	j, _, err := e.st.UpsertJob(context.Background(), store.JobMessage{
		RunnerRequestID: requestID, State: store.JobQueued, Repository: repo, JobName: fmt.Sprint("job ", requestID),
		WorkflowRef: repo + "/.github/workflows/ci.yml@refs/heads/main", QueuedAt: queued,
	}, time.Now())
	must(e.t, err)
	return j
}

func (e *env) addServer(id, arch, status string, vcpu, memoryMB int) {
	e.t.Helper()
	now := time.Now()
	must(e.t, e.st.CreateServer(context.Background(), store.Server{
		ID: id, Name: id, Kind: store.ServerAgent, Status: status, CreatedAt: now, UpdatedAt: now,
		ServerFacts: store.ServerFacts{Arch: arch, VCPU: vcpu, MemoryMB: memoryMB, Mode: sandbox.ModeContainer},
	}))
}

func (e *env) addSandbox(id string, state sandbox.State) {
	e.t.Helper()
	must(e.t, e.st.InsertSandbox(context.Background(), store.Sandbox{
		ID: id, PoolID: "p", ServerID: "s", Mode: sandbox.ModeContainer, State: state, VCPU: 2, MemoryMB: 4096,
		CreatedAt: time.Now(),
	}))
}

func TestSetupMakesAdminWithSession(t *testing.T) {
	e := newEnv(t)
	code, err := e.d.Auth.NewSetupCode(context.Background())
	must(t, err)
	b := e.browser()

	var status Status
	b.do("GET", "/api/status", nil, &status).want(t, 200)
	if !status.NeedsSetup || status.Mode != "container" {
		t.Fatalf("status before setup = %+v, want needs_setup and mode container", status)
	}

	setup := map[string]string{"code": "AAAA-BBBB-CCCC", "name": "Ada", "email": "ada@example.com", "password": "long enough password"}
	b.do("POST", "/api/setup", setup, nil).wantField(t, 422, "body.code")
	setup["code"] = code
	var u User
	res := b.do("POST", "/api/setup", setup, &u)
	res.want(t, 200)
	if u.Role != "admin" || u.Email != "ada@example.com" {
		t.Fatalf("setup user = %+v, want admin ada@example.com", u)
	}
	cookie := res.Header.Get("Set-Cookie")
	for _, part := range []string{"runplane_session=", "Path=/", "Max-Age=2592000", "HttpOnly", "SameSite=Lax"} {
		if !strings.Contains(cookie, part) {
			t.Errorf("Set-Cookie %q has no %q", cookie, part)
		}
	}
	if strings.Contains(cookie, "Secure") {
		t.Errorf("Set-Cookie %q is Secure on plain HTTP; the browser would not send it back", cookie)
	}

	var me User
	b.do("GET", "/api/session", nil, &me).want(t, 200)
	if me.ID != u.ID {
		t.Fatalf("session user %q, want %q", me.ID, u.ID)
	}
	b.do("GET", "/api/status", nil, &status).want(t, 200)
	if status.NeedsSetup {
		t.Fatal("status still needs setup after the first admin")
	}
	b.do("GET", "/api/setup/host?code="+code, nil, nil).want(t, 409)

	// The audit log records the setup with the new admin.
	var page AuditPage
	b.do("GET", "/api/audit", nil, &page).want(t, 200)
	if len(page.Items) != 1 || page.Items[0].Action != "setup.done" || page.Items[0].UserID != u.ID {
		t.Fatalf("audit log = %+v, want one setup.done entry by the admin", page.Items)
	}
}

func TestSessionCookieSecureBehindHTTPSProxy(t *testing.T) {
	e := newEnv(t)
	e.addUser("ada@example.com", auth.RoleAdmin)
	b := e.browser()
	b.header.Set("X-Forwarded-Proto", "https")
	res := b.do("POST", "/api/session", map[string]string{"email": "ada@example.com", "password": "correct horse battery"}, nil)
	res.want(t, 200)
	if !strings.Contains(res.Header.Get("Set-Cookie"), "Secure") {
		t.Fatalf("Set-Cookie %q is not Secure behind an HTTPS proxy", res.Header.Get("Set-Cookie"))
	}
}

func TestSetupHostChecksTheCode(t *testing.T) {
	e := newEnv(t)
	code, err := e.d.Auth.NewSetupCode(context.Background())
	must(t, err)
	b := e.browser()
	b.do("GET", "/api/setup/host?code=AAAA-BBBB-CCCC", nil, nil).want(t, 422)
	// The code passes without dashes and in lowercase, as it does for setup.
	b.do("GET", "/api/setup/host?code="+strings.ToLower(strings.ReplaceAll(code, "-", "")), nil, nil).want(t, 200)
}

func TestMutationNeedsCSRFHeader(t *testing.T) {
	e := newEnv(t)
	e.addUser("ada@example.com", auth.RoleAdmin)
	b := e.browser()
	creds := map[string]string{"email": "ada@example.com", "password": "correct horse battery"}

	b.header.Del("X-Runplane-CSRF")
	res := b.do("POST", "/api/session", creds, nil)
	res.want(t, 403)
	if res.Header.Get("Set-Cookie") != "" {
		t.Fatal("a login without the CSRF header made a session")
	}
	b.header.Set("X-Runplane-CSRF", "1")
	b.do("POST", "/api/session", creds, nil).want(t, 200)
	b.header.Del("X-Runplane-CSRF")
	b.do("DELETE", "/api/session", nil, nil).want(t, 403)
	b.do("GET", "/api/session", nil, nil).want(t, 200) // still logged in
	b.do("GET", "/api/pools", nil, nil).want(t, 200)
}

func TestRoles(t *testing.T) {
	e := newEnv(t)
	member := e.login("bob@example.com", auth.RoleMember)
	anon := e.browser()

	for _, tc := range []struct {
		method, path string
		body         any
		member, anon int
	}{
		{"GET", "/api/pools", nil, 200, 401},
		{"GET", "/api/jobs", nil, 200, 401},
		{"GET", "/api/settings", nil, 200, 401},
		{"PATCH", "/api/settings", map[string]any{"instance_name": "x"}, 403, 401},
		{"POST", "/api/pools", map[string]any{"label": "x", "vcpu": 2, "memory_mb": 4096, "disk_gb": 20, "max_runners": 1}, 403, 401},
		{"GET", "/api/users", nil, 403, 401},
		{"GET", "/api/audit", nil, 403, 401},
		{"POST", "/api/invites", map[string]any{"email": "eve@example.com", "role": "admin"}, 403, 401},
		// A member may stop sandboxes and re-run jobs. The request passes the
		// role check and gets 404 for the unknown ID.
		{"POST", "/api/sandboxes/x/stop", nil, 404, 401},
		{"POST", "/api/jobs/x/rerun", map[string]any{}, 404, 401},
	} {
		if got := member.do(tc.method, tc.path, tc.body, nil).StatusCode; got != tc.member {
			t.Errorf("member %s %s: status %d, want %d", tc.method, tc.path, got, tc.member)
		}
		if got := anon.do(tc.method, tc.path, tc.body, nil).StatusCode; got != tc.anon {
			t.Errorf("no session %s %s: status %d, want %d", tc.method, tc.path, got, tc.anon)
		}
	}
}

func TestLoginRateLimitUsesClientIP(t *testing.T) {
	e := newEnv(t)
	e.addUser("ada@example.com", auth.RoleAdmin)
	creds := map[string]string{"email": "ada@example.com", "password": "correct horse battery"}

	// A reverse proxy on the same host sends the client address.
	attacker := e.browser()
	attacker.header.Set("X-Forwarded-For", "203.0.113.9")
	for i := range 10 {
		// Other emails, so only the limit of the IP address can block.
		attacker.do("POST", "/api/session", map[string]string{"email": fmt.Sprintf("guess%d@example.com", i), "password": "wrong"}, nil).want(t, 401)
	}
	attacker.do("POST", "/api/session", creds, nil).want(t, 429)

	other := e.browser()
	other.header.Set("X-Forwarded-For", "198.51.100.7")
	other.do("POST", "/api/session", creds, nil).want(t, 200)
}

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	e := newEnv(t)
	laptop := e.login("ada@example.com", auth.RoleMember)
	phone := e.browser()
	phone.do("POST", "/api/session", map[string]string{"email": "ada@example.com", "password": "correct horse battery"}, nil).want(t, 200)

	laptop.do("PATCH", "/api/me", map[string]string{
		"name": "Ada L", "current_password": "wrong password", "new_password": "a new long password",
	}, nil).wantField(t, 422, "body.current_password")
	var me User
	laptop.do("GET", "/api/session", nil, &me).want(t, 200)
	if me.Name != "ada@example.com" {
		t.Fatalf("a rejected change renamed the user to %q", me.Name)
	}

	laptop.do("PATCH", "/api/me", map[string]string{
		"name": "Ada L", "current_password": "correct horse battery", "new_password": "a new long password",
	}, &me).want(t, 200)
	if me.Name != "Ada L" {
		t.Fatalf("name %q, want Ada L", me.Name)
	}
	laptop.do("GET", "/api/session", nil, nil).want(t, 200)
	phone.do("GET", "/api/session", nil, nil).want(t, 401)
	e.browser().do("POST", "/api/session", map[string]string{"email": "ada@example.com", "password": "a new long password"}, nil).want(t, 200)
}

// Anyone who knows the email of a user can lock it with wrong passwords. A
// browser that logged in before must still log in and change the password.
func TestLockedEmailKeepsKnownBrowsers(t *testing.T) {
	e := newEnv(t)
	laptop := e.login("ada@example.com", auth.RoleAdmin)
	attacker := e.browser()
	for i := range 10 {
		attacker.header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i+1))
		attacker.do("POST", "/api/session", map[string]string{"email": "ada@example.com", "password": "wrong"}, nil).want(t, 401)
	}
	e.browser().do("POST", "/api/session", map[string]string{"email": "ada@example.com", "password": "correct horse battery"}, nil).want(t, 429)

	// The change of the password keeps the browser logged in.
	laptop.do("PATCH", "/api/me", map[string]string{
		"current_password": "correct horse battery", "new_password": "a new long password",
	}, nil).want(t, 200)
	laptop.do("GET", "/api/session", nil, nil).want(t, 200)

	// After a logout, the device cookie lets the browser log in again.
	laptop.do("DELETE", "/api/session", nil, nil)
	laptop.do("GET", "/api/session", nil, nil).want(t, 401)
	laptop.do("POST", "/api/session", map[string]string{"email": "ada@example.com", "password": "a new long password"}, nil).want(t, 200)
	e.browser().do("POST", "/api/session", map[string]string{"email": "ada@example.com", "password": "a new long password"}, nil).want(t, 429)
}

func TestSettingsValidation(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleAdmin)

	for _, tc := range []struct {
		patch map[string]any
		loc   string
	}{
		{map[string]any{"dns_servers": []string{"1.1.1.1", "2606:4700::1111"}}, "body.dns_servers[1]"},
		{map[string]any{"dns_servers": []string{}}, "body.dns_servers"},
		{map[string]any{"public_url": "https://ci.example.com/runplane"}, "body.public_url"},
		{map[string]any{"public_url": "ci.example.com"}, "body.public_url"},
		{map[string]any{"domain": "192.0.2.1"}, "body.domain"},
		{map[string]any{"egress_allowlist": []string{"github.com", "not a host"}}, "body.egress_allowlist[1]"},
		{map[string]any{"cpu_overcommit": 20}, "body.cpu_overcommit"},
		{map[string]any{"default_time_limit_minutes": 0}, "body.default_time_limit_minutes"},
		{map[string]any{"allow_public_repos": true}, "body.allow_public_repos"},
		{map[string]any{"mode": "dev"}, "body.mode"},
		{map[string]any{"backup_s3": map[string]any{"bucket": "backups", "access_key": "AK"}}, "body.backup_s3.secret_key"},
		{map[string]any{"instance_nam": "typo"}, "body.instance_nam"},
	} {
		b.do("PATCH", "/api/settings", tc.patch, nil).wantField(t, 422, tc.loc)
	}
	st, err := e.st.GetSettings(context.Background())
	must(t, err)
	if st.CPUOvercommit != 1 || len(st.DNSServers) != 2 || st.AllowPublicRepos || st.BackupS3.Bucket != "" {
		t.Fatalf("rejected patches changed the settings: %+v", st)
	}

	var out Settings
	b.do("PATCH", "/api/settings", map[string]any{
		"public_url": "https://ci.example.com/", "dns_servers": []string{" 9.9.9.9 "},
		"backup_s3": map[string]any{"bucket": "backups", "access_key": "AK", "secret_key": "s3cret"},
	}, &out).want(t, 200)
	if out.PublicURL != "https://ci.example.com" || out.DNSServers[0] != "9.9.9.9" || !out.BackupS3.SecretKeySet {
		t.Fatalf("settings after patch = %+v", out)
	}
	if res := b.do("GET", "/api/settings", nil, nil); bytes.Contains(res.body, []byte("s3cret")) {
		t.Fatal("GET /api/settings shows the S3 secret key")
	}
	st, _ = e.st.GetSettings(context.Background())
	if plain, err := e.d.Box.Open(st.BackupS3.SecretKeyEnc); err != nil || string(plain) != "s3cret" {
		t.Fatalf("stored S3 secret opens to %q, %v; want s3cret", plain, err)
	}
}

func TestPools(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleAdmin)
	valid := func(changes map[string]any) map[string]any {
		p := map[string]any{"label": "runplane", "vcpu": 2, "memory_mb": 8192, "disk_gb": 64, "min_idle": 1, "max_runners": 5}
		maps.Copy(p, changes)
		return p
	}
	for _, tc := range []struct {
		pool map[string]any
		loc  string
	}{
		{valid(map[string]any{"label": "Runplane"}), "body.label"},
		{valid(map[string]any{"label": "run.plane"}), "body.label"},
		{valid(map[string]any{"vcpu": 0}), "body.vcpu"},
		{valid(map[string]any{"vcpu": 65}), "body.vcpu"},
		{valid(map[string]any{"memory_mb": 511}), "body.memory_mb"},
		{valid(map[string]any{"memory_mb": 262145}), "body.memory_mb"},
		{valid(map[string]any{"disk_gb": 7}), "body.disk_gb"},
		{valid(map[string]any{"disk_gb": 1025}), "body.disk_gb"},
		{valid(map[string]any{"min_idle": 6}), "body.min_idle"},
		{valid(map[string]any{"image_id": "nope"}), "body.image_id"},
		{valid(map[string]any{"server_ids": []string{"nope"}}), "body.server_ids[0]"},
	} {
		b.do("POST", "/api/pools", tc.pool, nil).wantField(t, 422, tc.loc)
	}

	var p Pool
	b.do("POST", "/api/pools", valid(nil), &p).want(t, 200)
	if p.Name != "runplane" || !p.Enabled || p.Arch == "" {
		t.Fatalf("new pool = %+v, want name from label, enabled, and an arch", p)
	}
	b.do("POST", "/api/pools", valid(map[string]any{"name": "other"}), nil).wantField(t, 409, "body.label")

	var changed Pool
	b.do("PATCH", "/api/pools/"+p.ID, map[string]any{"max_runners": 3}, &changed).want(t, 200)
	if changed.MaxRunners != 3 || changed.MinIdle != 1 || changed.VCPU != 2 {
		t.Fatalf("patched pool = %+v, want max_runners 3 and the other fields kept", changed)
	}
	b.do("PATCH", "/api/pools/"+p.ID, map[string]any{"max_runners": 0}, nil).want(t, 422)
	b.do("PATCH", "/api/pools/"+p.ID, map[string]any{"min_idle": 4}, nil).want(t, 422)

	// A live sandbox shows in the counts of its pool.
	must(t, e.st.InsertSandbox(context.Background(), store.Sandbox{
		ID: ids.New(), PoolID: p.ID, ServerID: "s1", Mode: sandbox.ModeContainer, State: sandbox.StateIdle,
		VCPU: 2, MemoryMB: 8192, CreatedAt: time.Now(),
	}))
	var list []Pool
	b.do("GET", "/api/pools", nil, &list).want(t, 200)
	if len(list) != 1 || list[0].Idle != 1 || list[0].Busy != 0 {
		t.Fatalf("pools = %+v, want one pool with 1 idle sandbox", list)
	}

	b.do("DELETE", "/api/pools/"+p.ID, nil, nil).want(t, 204)
	b.do("GET", "/api/pools/"+p.ID, nil, nil).want(t, 404)

	var page AuditPage
	b.do("GET", "/api/audit", nil, &page).want(t, 200)
	var actions []string
	for _, a := range page.Items {
		actions = append(actions, a.Action+" "+a.UserEmail)
	}
	if want := []string{"pool.delete ada@example.com", "pool.update ada@example.com", "pool.create ada@example.com"}; !slices.Equal(actions, want) {
		t.Fatalf("audit log = %v, want %v", actions, want)
	}
}

func TestNotificationURLIsSealedAndMasked(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleAdmin)
	secretURL := "https://hooks.slack.com/services/T000/B000/XXXXsecretXXXXa1b2"

	b.do("POST", "/api/notifications", map[string]any{
		"name": "ops", "kind": "slack", "url": "http://hooks.slack.com/x", "events": []string{"job.failed"},
	}, nil).want(t, 422)

	var c NotificationChannel
	res := b.do("POST", "/api/notifications", map[string]any{
		"name": "ops", "kind": "slack", "url": secretURL, "events": []string{"job.failed", "job.failed"},
	}, &c)
	res.want(t, 200)
	if bytes.Contains(res.body, []byte("secret")) || c.URL != "https://hooks.slack.com/…a1b2" {
		t.Fatalf("channel URL in response = %q", c.URL)
	}
	if len(c.Events) != 1 {
		t.Fatalf("events = %v, want one job.failed", c.Events)
	}
	stored, err := e.st.GetNotificationChannel(context.Background(), c.ID)
	must(t, err)
	if bytes.Contains(stored.URLEnc, []byte("hooks.slack.com")) {
		t.Fatal("the database has the URL in plain text")
	}
	if plain, err := e.d.Box.Open(stored.URLEnc); err != nil || string(plain) != secretURL {
		t.Fatalf("sealed URL opens to %q, %v", plain, err)
	}

	// A change of the kind checks the kept URL against the new kind.
	b.do("PATCH", "/api/notifications/"+c.ID, map[string]any{"kind": "webhook", "url": "http://10.0.0.2/hook"}, nil).want(t, 200)
	b.do("PATCH", "/api/notifications/"+c.ID, map[string]any{"kind": "discord"}, nil).want(t, 422)
}

func TestEventsStreamConvertsStoreModels(t *testing.T) {
	e := newEnv(t)
	ch := e.login("ada@example.com", auth.RoleMember).events(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	pool := store.Pool{ID: ids.New(), Name: "Default", Label: "runplane", Arch: "x64", VCPU: 2, MemoryMB: 4096, DiskGB: 20, MaxRunners: 1, Enabled: true, CreatedAt: now, UpdatedAt: now}
	must(t, e.st.CreatePool(context.Background(), pool))
	sb := store.Sandbox{ID: "sb1", PoolID: pool.ID, ServerID: "srv", State: sandbox.StateBusy, VCPU: 2, MemoryMB: 4096, CreatedAt: now}
	e.publish(events.SettingsChanged{}, events.SandboxState(sb))

	ev := recv(t, ch)
	if ev.name != "sandbox.state" {
		t.Fatalf("got event %q with %s; settings.changed must not reach the browser", ev.name, ev.data)
	}
	var got Sandbox
	must(t, json.Unmarshal([]byte(ev.data), &got))
	if got.ID != "sb1" || got.PoolLabel != "runplane" || got.State != "busy" {
		t.Fatalf("sandbox.state data = %+v, want sb1 busy in pool runplane", got)
	}
}
