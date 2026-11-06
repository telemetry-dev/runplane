package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/update"
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

// A test message goes to the receiver of the channel, and the delivery of
// a saved channel shows in the list.
func TestNotificationTests(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.Contains(r.URL.Path, "broken") {
			http.Error(w, "no such hook", http.StatusNotFound)
		}
	}))
	defer receiver.Close()
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleAdmin)

	b.do("POST", "/api/notifications/test", map[string]string{"kind": "slack", "url": "http://hooks.slack.com/x"}, nil).want(t, 422)
	b.do("POST", "/api/notifications/test", map[string]string{"kind": "webhook", "url": receiver.URL + "/hook"}, nil).want(t, 204)
	mu.Lock()
	sent := slices.Clone(paths)
	mu.Unlock()
	if !slices.Equal(sent, []string{"/hook"}) {
		t.Fatalf("receiver got %q, want one test to the unsaved webhook", sent)
	}

	var c NotificationChannel
	b.do("POST", "/api/notifications", map[string]any{
		"name": "ops", "kind": "webhook", "url": receiver.URL + "/broken", "events": []string{"job.failed"},
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

	b.do("PATCH", "/api/notifications/"+c.ID, map[string]any{"url": receiver.URL + "/fixed"}, nil).want(t, 200)
	b.do("POST", "/api/notifications/"+c.ID+"/test", nil, nil).want(t, 204)
	b.do("GET", "/api/notifications", nil, &list).want(t, 200)
	if list[0].LastError != "" {
		t.Fatalf("after a good test: last_error %q, want none", list[0].LastError)
	}
	b.do("POST", "/api/notifications/nope/test", nil, nil).want(t, 404)
}

// fakeUpdater is an updater in a state, whose Apply returns err.
type fakeUpdater struct {
	state update.State
	err   error
}

func (f fakeUpdater) Check(context.Context) (update.Status, error) {
	return update.Status{Current: "v1.0.0", Latest: "v1.1.0", Available: true}, nil
}

func (f fakeUpdater) State() update.State { return f.state }

func (f fakeUpdater) Apply(context.Context) (update.Status, error) {
	return update.Status{Current: "v1.0.0", Latest: "v1.1.0", Available: true}, f.err
}

// The dashboard shows the state of the updater, also of an auto update. A
// second update while one runs is a conflict.
func TestUpdateState(t *testing.T) {
	for _, tt := range []struct {
		state  update.State
		err    error
		status int
	}{
		{update.Idle, nil, 204},
		{update.Applying, update.ErrBusy, 409},
		{update.Restarting, update.ErrBusy, 409},
		{update.Idle, update.ErrNoUpdate, 409},
	} {
		e := newEnv(t, func(d *Deps) { d.Updater = fakeUpdater{state: tt.state, err: tt.err} })
		b := e.login("ada@example.com", auth.RoleAdmin)
		var st UpdateStatus
		b.do("GET", "/api/update", nil, &st).want(t, 200)
		if st.State != string(tt.state) || st.Latest != "v1.1.0" {
			t.Errorf("state %q, latest %q; want %s and v1.1.0", st.State, st.Latest, tt.state)
		}
		if res := b.do("POST", "/api/update", nil, nil); res.StatusCode != tt.status {
			t.Errorf("update with %v: status %d, want %d", tt.err, res.StatusCode, tt.status)
		}
	}
}
