package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

func TestNewInviteLinkReplacesTheOldOne(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleAdmin)
	b.do("PATCH", "/api/me", map[string]string{"name": "Ada Okafor"}, nil).want(t, 200)

	var first, second CreatedInvite
	b.do("POST", "/api/invites", map[string]string{"email": "bob@example.com", "role": "member"}, &first).want(t, 200)
	b.do("POST", "/api/invites/"+first.Invite.ID+"/link", nil, &second).want(t, 200)
	if second.URL == first.URL || second.Invite.Email != "bob@example.com" || second.Invite.Role != "member" {
		t.Fatalf("new link %+v, want another URL for the same email and role", second)
	}

	visitor := e.browser()
	token := func(u string) string { return u[strings.LastIndex(u, "/")+1:] }
	visitor.do("GET", "/api/invites/"+token(first.URL), nil, nil).want(t, 404)
	var info InviteInfo
	visitor.do("GET", "/api/invites/"+token(second.URL), nil, &info).want(t, 200)
	if info.InvitedBy != "Ada Okafor" || info.Email != "bob@example.com" {
		t.Fatalf("invite info %+v, want bob invited by Ada Okafor", info)
	}

	var members Members
	b.do("GET", "/api/users", nil, &members).want(t, 200)
	if len(members.Invites) != 1 || members.Invites[0].ID != second.Invite.ID {
		t.Fatalf("open invites %+v, want only the new one", members.Invites)
	}
	b.do("POST", "/api/invites/"+first.Invite.ID+"/link", nil, nil).want(t, 404)
}

// fakeNotifier records the tests and fails for URLs that contain "broken".
type fakeNotifier struct{ sent []string }

func (f *fakeNotifier) Test(_ context.Context, _, kind, url string) error {
	f.sent = append(f.sent, kind+" "+url)
	if strings.Contains(url, "broken") {
		return errors.New("receiver answered 404 Not Found")
	}
	return nil
}

func TestNotificationTests(t *testing.T) {
	n := &fakeNotifier{}
	e := newEnv(t, func(d *Deps) { d.Notifier = n })
	b := e.login("ada@example.com", auth.RoleAdmin)

	b.do("POST", "/api/notifications/test", map[string]string{"kind": "slack", "url": "http://hooks.slack.com/x"}, nil).want(t, 422)
	b.do("POST", "/api/notifications/test", map[string]string{"kind": "webhook", "url": "http://10.0.0.2/hook"}, nil).want(t, 204)
	if len(n.sent) != 1 || n.sent[0] != "webhook http://10.0.0.2/hook" {
		t.Fatalf("sent %q, want one test to the unsaved webhook", n.sent)
	}

	var c NotificationChannel
	b.do("POST", "/api/notifications", map[string]any{
		"name": "ops", "kind": "webhook", "url": "https://example.com/broken", "events": []string{"job.failed"},
	}, &c).want(t, 200)
	if c.LastSentAt != nil {
		t.Fatalf("new channel has last_sent_at %v", c.LastSentAt)
	}
	b.do("POST", "/api/notifications/"+c.ID+"/test", nil, nil).want(t, 500)
	var list []NotificationChannel
	b.do("GET", "/api/notifications", nil, &list).want(t, 200)
	if list[0].LastSentAt == nil || !strings.Contains(list[0].LastError, "404") {
		t.Fatalf("after a failed test: last_sent_at %v, last_error %q", list[0].LastSentAt, list[0].LastError)
	}

	b.do("PATCH", "/api/notifications/"+c.ID, map[string]any{"url": "https://example.com/fixed"}, nil).want(t, 200)
	b.do("POST", "/api/notifications/"+c.ID+"/test", nil, nil).want(t, 204)
	b.do("GET", "/api/notifications", nil, &list).want(t, 200)
	if list[0].LastError != "" {
		t.Fatalf("after a good test: last_error %q, want none", list[0].LastError)
	}
}

// fakeUpdater blocks Apply until release closes.
type fakeUpdater struct {
	applying chan struct{}
	release  chan struct{}
}

func (f *fakeUpdater) Check(context.Context) (UpdateStatus, error) {
	return UpdateStatus{Current: "v1.0.0", Latest: "v1.1.0", Available: true}, nil
}

func (f *fakeUpdater) Apply(context.Context) error {
	close(f.applying)
	<-f.release
	return nil
}

func TestUpdateState(t *testing.T) {
	u := &fakeUpdater{applying: make(chan struct{}), release: make(chan struct{})}
	e := newEnv(t, func(d *Deps) { d.Updater = u })
	b := e.login("ada@example.com", auth.RoleAdmin)
	other := e.login("eve@example.com", auth.RoleAdmin)

	var st UpdateStatus
	b.do("GET", "/api/update", nil, &st).want(t, 200)
	if st.State != "idle" {
		t.Fatalf("state before the update = %q, want idle", st.State)
	}
	done := make(chan response)
	go func() { done <- b.send("POST", "/api/update", nil, nil, true) }()
	recv(t, u.applying)
	other.do("GET", "/api/update", nil, &st).want(t, 200)
	if st.State != "applying" {
		t.Fatalf("state during the update = %q, want applying", st.State)
	}
	other.do("POST", "/api/update", nil, nil).want(t, 409)
	close(u.release)
	recv(t, done).want(t, 204)
	other.do("GET", "/api/update", nil, &st).want(t, 200)
	if st.State != "restarting" {
		t.Fatalf("state after the update = %q, want restarting", st.State)
	}
}

func TestRunnerVersionsCurrentIsTheNewestReady(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, b := range []store.RunnerBundle{
		{Version: "2.331.0", Arch: "x64", Status: store.BundleFailed},
		{Version: "2.330.0", Arch: "x64", Status: store.BundleReady},
		{Version: "2.99.0", Arch: "x64", Status: store.BundleReady},
		{Version: "2.331.0", Arch: "arm64", Status: store.BundlePending},
	} {
		if err := e.st.SetRunnerBundleBuild(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	var got RunnerVersions
	e.login("ada@example.com", auth.RoleMember).do("GET", "/api/runner-versions", nil, &got).want(t, 200)
	if got.Latest != "2.331.0" || got.Current != (RunnerCurrent{X64: "2.330.0"}) {
		t.Fatalf("latest %q, current %+v; want 2.331.0 and x64 2.330.0 only", got.Latest, got.Current)
	}
}

func TestOverviewHasNoRatesWithoutData(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleMember)
	ctx := context.Background()
	now := time.Now().UTC()
	e.addJob(1, "acme/api", now.Add(-time.Minute))

	res := b.do("GET", "/api/overview", nil, nil)
	res.want(t, 200)
	for _, want := range []string{`"median_queue_ms":null`, `"cache_hit_rate":null`} {
		if !strings.Contains(string(res.body), want) {
			t.Errorf("overview without assigned jobs and cache lookups has no %s: %s", want, res.body)
		}
	}

	if _, err := e.st.UpsertJob(ctx, store.JobMessage{RunnerRequestID: 1, State: store.JobAssigned, AssignedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddCacheStats(ctx, now, "acme/api", store.CacheCounts{Misses: 4}); err != nil {
		t.Fatal(err)
	}
	var o Overview
	b.do("GET", "/api/overview", nil, &o).want(t, 200)
	if o.Stats.MedianQueueMS == nil || o.Stats.CacheHitRate == nil || *o.Stats.CacheHitRate != 0 {
		t.Fatalf("overview stats %+v, want a median and a hit rate of 0", o.Stats)
	}
}

// An unreachable agent keeps its sandbox rows until it connects again. Its
// sandboxes must not count against the vCPUs of the servers that are left.
func TestOverviewVCPUsSkipUnreachableServers(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleMember)
	e.addServer("online", "x64", store.ServerOnline, 8, 16384)
	e.addServer("lost", "x64", store.ServerUnreachable, 16, 32768)
	for i, x := range []struct {
		server string
		vcpu   int
	}{{"online", 2}, {"lost", 4}, {"lost", 4}} {
		err := e.st.InsertSandbox(context.Background(), store.Sandbox{
			ID: fmt.Sprint("box", i), PoolID: "p", ServerID: x.server, Mode: sandbox.ModeContainer,
			State: sandbox.StateBusy, VCPU: x.vcpu, MemoryMB: 4096, CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var o Overview
	b.do("GET", "/api/overview", nil, &o).want(t, 200)
	if o.Stats.VCPUInUse != 2 || o.Stats.VCPUTotal != 8 {
		t.Fatalf("vCPUs %d / %d, want 2 / 8", o.Stats.VCPUInUse, o.Stats.VCPUTotal)
	}
}
