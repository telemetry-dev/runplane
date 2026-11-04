package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

type env struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
	d   Deps
}

// newEnv starts the API on a new database. with sets services in Deps.
func newEnv(t *testing.T, with ...func(*Deps)) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secret.NewBox(bytes.Repeat([]byte{7}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{
		Store: st, Auth: auth.NewService(st, []byte("device key")), Bus: &events.Bus{}, Box: box,
		Config: config.Config{DataDir: dir, ConfigDir: dir, Mode: sandbox.ModeContainer},
	}
	for _, f := range with {
		f(&d)
	}
	h := New(d)
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		h.Close()
		srv.Close()
	})
	return &env{t: t, srv: srv, st: st, d: d}
}

// browser is an HTTP client with a cookie jar, like the dashboard.
type browser struct {
	e      *env
	client *http.Client
	header http.Header // sent with each request
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e: e, client: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, header: http.Header{}}
}

type response struct {
	*http.Response
	body []byte
}

// do sends a request with the CSRF header and decodes a JSON body into out.
func (b *browser) do(method, path string, body any, out any) response {
	b.e.t.Helper()
	return b.send(method, path, body, out, true)
}

func (b *browser) send(method, path string, body any, out any, csrf bool) response {
	b.e.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			b.e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, b.e.srv.URL+path, rd)
	if err != nil {
		b.e.t.Fatal(err)
	}
	for k, v := range b.header {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		req.Header.Set("X-Runplane-CSRF", "1")
	}
	res, err := b.client.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		b.e.t.Fatal(err)
	}
	if out != nil && res.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			b.e.t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
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

// problem is the RFC 9457 body of an error.
type problem struct {
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Errors []struct {
		Location string `json:"location"`
		Message  string `json:"message"`
	} `json:"errors"`
}

func (r response) problem(t *testing.T) problem {
	t.Helper()
	var p problem
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("decode problem %s: %v", r.body, err)
	}
	return p
}

func (p problem) locations() []string {
	var locs []string
	for _, e := range p.Errors {
		locs = append(locs, e.Location)
	}
	return locs
}

// addUser makes a user directly in the store.
func (e *env) addUser(email, role string) {
	e.t.Helper()
	err := e.st.CreateUser(context.Background(), store.User{
		ID: ids.New(), Email: email, Name: email, PasswordHash: auth.HashPassword("correct horse battery"),
		Role: role, CreatedAt: time.Now(),
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// login returns a browser with a session of a new user with role.
func (e *env) login(email, role string) *browser {
	e.t.Helper()
	e.addUser(email, role)
	b := e.browser()
	b.do("POST", "/api/session", map[string]string{"email": email, "password": "correct horse battery"}, nil).want(e.t, 200)
	return b
}

func TestSetupMakesAdminWithSession(t *testing.T) {
	e := newEnv(t)
	code, err := e.d.Auth.NewSetupCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()

	var status Status
	b.do("GET", "/api/status", nil, &status).want(t, 200)
	if !status.NeedsSetup || status.Mode != "container" {
		t.Fatalf("status before setup = %+v, want needs_setup and mode container", status)
	}

	bad := b.do("POST", "/api/setup", map[string]string{
		"code": "AAAA-BBBB-CCCC", "name": "Ada", "email": "ada@example.com", "password": "long enough password",
	}, nil)
	bad.want(t, 422)
	if locs := bad.problem(t).locations(); len(locs) != 1 || locs[0] != "body.code" {
		t.Fatalf("wrong code: error locations %v, want [body.code]", locs)
	}

	var u User
	res := b.do("POST", "/api/setup", map[string]string{
		"code": code, "name": "Ada", "email": "ada@example.com", "password": "long enough password",
	}, &u)
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
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.do("GET", "/api/setup/host?code=AAAA-BBBB-CCCC", nil, nil).want(t, 422)
	// The code passes without dashes and in lowercase, as it does for setup.
	// No host checker runs in the test, so a valid code gets 503.
	b.do("GET", "/api/setup/host?code="+strings.ToLower(strings.ReplaceAll(code, "-", "")), nil, nil).want(t, 503)
}

func TestMutationNeedsCSRFHeader(t *testing.T) {
	e := newEnv(t)
	e.addUser("ada@example.com", auth.RoleAdmin)
	b := e.browser()
	creds := map[string]string{"email": "ada@example.com", "password": "correct horse battery"}

	res := b.send("POST", "/api/session", creds, nil, false)
	res.want(t, 403)
	if res.Header.Get("Set-Cookie") != "" {
		t.Fatal("a login without the CSRF header made a session")
	}
	b.do("POST", "/api/session", creds, nil).want(t, 200)
	b.send("DELETE", "/api/session", nil, nil, false).want(t, 403)
	b.do("GET", "/api/session", nil, nil).want(t, 200) // still logged in
	b.send("GET", "/api/pools", nil, nil, false).want(t, 200)
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
		// A member may stop sandboxes and re-run jobs. Without a scheduler
		// the request passes the role check and gets 503.
		{"POST", "/api/sandboxes/x/stop", nil, 503, 401},
		{"POST", "/api/jobs/x/rerun", map[string]any{}, 503, 401},
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

func TestJobsCursorPages(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleMember)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var want []string
	for i := range 5 {
		j, err := e.st.UpsertJob(ctx, store.JobMessage{
			RunnerRequestID: int64(i + 1), State: store.JobQueued, Repository: "acme/api", JobName: fmt.Sprint("job ", i),
			WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", QueuedAt: base.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		want = append([]string{j.ID}, want...) // newest first
	}

	var got []string
	cursor := ""
	for range 4 {
		var page JobPage
		b.do("GET", "/api/jobs?limit=2&cursor="+cursor, nil, &page).want(t, 200)
		for _, j := range page.Items {
			got = append(got, j.ID)
			if j.WorkflowFile != ".github/workflows/ci.yml" || j.State != "queued" || j.Result != nil {
				t.Fatalf("job %+v: wrong workflow file, state, or result", j)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pages gave %v, want %v", got, want)
	}
	b.do("GET", "/api/jobs?cursor=bad!", nil, nil).want(t, 422)
}

func TestSettingsValidation(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleAdmin)

	for _, tc := range []struct {
		name  string
		patch map[string]any
		loc   string
	}{
		{"dns not ipv4", map[string]any{"dns_servers": []string{"1.1.1.1", "2606:4700::1111"}}, "body.dns_servers[1]"},
		{"no dns", map[string]any{"dns_servers": []string{}}, "body.dns_servers"},
		{"url with path", map[string]any{"public_url": "https://ci.example.com/runplane"}, "body.public_url"},
		{"url without scheme", map[string]any{"public_url": "ci.example.com"}, "body.public_url"},
		{"domain is ip", map[string]any{"domain": "192.0.2.1"}, "body.domain"},
		{"bad egress", map[string]any{"egress_allowlist": []string{"github.com", "not a host"}}, "body.egress_allowlist[1]"},
		{"overcommit too high", map[string]any{"cpu_overcommit": 20}, "body.cpu_overcommit"},
		{"zero time limit", map[string]any{"default_time_limit_minutes": 0}, "body.default_time_limit_minutes"},
		{"public repos in container mode", map[string]any{"allow_public_repos": true}, "body.allow_public_repos"},
		{"dev mode without insecure flag", map[string]any{"mode": "dev"}, "body.mode"},
		{"bucket without secret", map[string]any{"backup_s3": map[string]any{"bucket": "backups", "access_key": "AK"}}, "body.backup_s3.secret_key"},
		{"unknown key", map[string]any{"instance_nam": "typo"}, "body.instance_nam"},
	} {
		res := b.do("PATCH", "/api/settings", tc.patch, nil)
		if res.StatusCode != 422 {
			t.Errorf("%s: status %d, want 422: %s", tc.name, res.StatusCode, res.body)
			continue
		}
		if locs := res.problem(t).locations(); !contains(locs, tc.loc) {
			t.Errorf("%s: error locations %v, want %s", tc.name, locs, tc.loc)
		}
	}
	st, err := e.st.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
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

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func TestPools(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleAdmin)
	valid := func(changes map[string]any) map[string]any {
		p := map[string]any{"label": "runplane", "vcpu": 2, "memory_mb": 8192, "disk_gb": 64, "min_idle": 1, "max_runners": 5}
		for k, v := range changes {
			p[k] = v
		}
		return p
	}
	for _, tc := range []struct {
		name string
		pool map[string]any
		loc  string
	}{
		{"uppercase label", valid(map[string]any{"label": "Runplane"}), "body.label"},
		{"label with dot", valid(map[string]any{"label": "run.plane"}), "body.label"},
		{"no vcpu", valid(map[string]any{"vcpu": 0}), "body.vcpu"},
		{"65 vcpu", valid(map[string]any{"vcpu": 65}), "body.vcpu"},
		{"too little memory", valid(map[string]any{"memory_mb": 511}), "body.memory_mb"},
		{"too much memory", valid(map[string]any{"memory_mb": 262145}), "body.memory_mb"},
		{"small disk", valid(map[string]any{"disk_gb": 7}), "body.disk_gb"},
		{"big disk", valid(map[string]any{"disk_gb": 1025}), "body.disk_gb"},
		{"more idle than max", valid(map[string]any{"min_idle": 6}), "body.min_idle"},
		{"unknown image", valid(map[string]any{"image_id": "nope"}), "body.image_id"},
		{"unknown server", valid(map[string]any{"server_ids": []string{"nope"}}), "body.server_ids[0]"},
	} {
		res := b.do("POST", "/api/pools", tc.pool, nil)
		if res.StatusCode != 422 {
			t.Errorf("%s: status %d, want 422: %s", tc.name, res.StatusCode, res.body)
			continue
		}
		if locs := res.problem(t).locations(); !contains(locs, tc.loc) {
			t.Errorf("%s: error locations %v, want %s", tc.name, locs, tc.loc)
		}
	}

	var p Pool
	b.do("POST", "/api/pools", valid(nil), &p).want(t, 200)
	if p.Name != "runplane" || !p.Enabled || p.Arch == "" {
		t.Fatalf("new pool = %+v, want name from label, enabled, and an arch", p)
	}
	dup := b.do("POST", "/api/pools", valid(map[string]any{"name": "other"}), nil)
	dup.want(t, 409)
	if locs := dup.problem(t).locations(); !contains(locs, "body.label") {
		t.Fatalf("duplicate label: locations %v", locs)
	}

	var changed Pool
	b.do("PATCH", "/api/pools/"+p.ID, map[string]any{"max_runners": 3}, &changed).want(t, 200)
	if changed.MaxRunners != 3 || changed.MinIdle != 1 || changed.VCPU != 2 {
		t.Fatalf("patched pool = %+v, want max_runners 3 and the other fields kept", changed)
	}
	b.do("PATCH", "/api/pools/"+p.ID, map[string]any{"max_runners": 0}, nil).want(t, 422)
	b.do("PATCH", "/api/pools/"+p.ID, map[string]any{"min_idle": 4}, nil).want(t, 422)

	// A live sandbox shows in the counts of its pool.
	err := e.st.InsertSandbox(context.Background(), store.Sandbox{
		ID: ids.New(), PoolID: p.ID, ServerID: "s1", Mode: sandbox.ModeContainer, State: sandbox.StateIdle,
		VCPU: 2, MemoryMB: 8192, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if strings.Join(actions, ",") != "pool.delete ada@example.com,pool.update ada@example.com,pool.create ada@example.com" {
		t.Fatalf("audit log = %v", actions)
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
	if err != nil {
		t.Fatal(err)
	}
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
	b := e.login("ada@example.com", auth.RoleMember)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	pool := store.Pool{ID: ids.New(), Name: "Default", Label: "runplane", Arch: "x64", VCPU: 2, MemoryMB: 4096, DiskGB: 20, MaxRunners: 1, Enabled: true, CreatedAt: now, UpdatedAt: now}
	if err := e.st.CreatePool(ctx, pool); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("GET", e.srv.URL+"/api/events", nil)
	res, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events: status %d, type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}

	// The handler subscribes after the headers, so publish until the event comes.
	sb := store.Sandbox{ID: "sb1", PoolID: pool.ID, ServerID: "srv", State: sandbox.StateBusy, VCPU: 2, MemoryMB: 4096, CreatedAt: now}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			e.d.Bus.Publish(events.Event{Type: events.SettingsChanged, Data: store.Settings{}})
			e.d.Bus.Publish(events.Event{Type: events.SandboxState, Data: &sb})
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()

	r := bufio.NewReader(res.Body)
	deadline := time.AfterFunc(5*time.Second, func() { res.Body.Close() })
	defer deadline.Stop()
	var event string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			event = name
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if event != "sandbox.state" {
			t.Fatalf("got event %q with %s; settings.changed must not reach the browser", event, data)
		}
		var got Sandbox
		if err := json.Unmarshal([]byte(data), &got); err != nil {
			t.Fatal(err)
		}
		if got.ID != "sb1" || got.PoolLabel != "runplane" || got.State != "busy" {
			t.Fatalf("sandbox.state data = %+v, want sb1 busy in pool runplane", got)
		}
		return
	}
}

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	e := newEnv(t)
	laptop := e.login("ada@example.com", auth.RoleMember)
	phone := e.browser()
	phone.do("POST", "/api/session", map[string]string{"email": "ada@example.com", "password": "correct horse battery"}, nil).want(t, 200)

	wrong := laptop.do("PATCH", "/api/me", map[string]string{
		"name": "Ada L", "current_password": "wrong password", "new_password": "a new long password",
	}, nil)
	wrong.want(t, 422)
	if locs := wrong.problem(t).locations(); !contains(locs, "body.current_password") {
		t.Fatalf("wrong current password: locations %v", locs)
	}
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
