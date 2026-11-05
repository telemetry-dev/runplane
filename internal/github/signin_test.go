package github

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/runplane/runplane/internal/api"
)

const testOAuthClientID = "Ov23litestclient"

// signInEnv fakes the device flow of GitHub. The token endpoint answers
// with the next item of tokenAnswers each time; the last item repeats.
type signInEnv struct {
	*env
	interval     int
	tokenAnswers []map[string]any
	tokenCalls   atomic.Int32
}

func newSignInEnv(t *testing.T, interval int, tokenAnswers ...map[string]any) *signInEnv {
	e := &signInEnv{env: newEnv(t), interval: interval, tokenAnswers: tokenAnswers}
	e.svc.clientID = testOAuthClientID
	e.mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("device code request Accept = %q, want application/json", r.Header.Get("Accept"))
		}
		form := formOf(t, r)
		if form.Get("client_id") != testOAuthClientID || form.Get("scope") != "read:org" {
			t.Errorf("device code request = %v, want the client ID and the scope read:org", form)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code": "3584d83530557fdd1f46af8289938c8ef79f9dc5", "user_code": "WDJB-MJHT",
			"verification_uri": "https://github.com/login/device", "expires_in": 900, "interval": e.interval,
		})
	})
	e.mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		form := formOf(t, r)
		if form.Get("client_id") != testOAuthClientID || form.Get("device_code") != "3584d83530557fdd1f46af8289938c8ef79f9dc5" ||
			form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Errorf("token request = %v", form)
		}
		n := int(e.tokenCalls.Add(1))
		writeJSON(w, http.StatusOK, e.tokenAnswers[min(n, len(e.tokenAnswers))-1])
	})
	return e
}

func formOf(t *testing.T, r *http.Request) url.Values {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	return r.PostForm
}

var (
	pending = map[string]any{"error": "authorization_pending",
		"error_description": "The authorization request is still pending.", "error_uri": "https://docs.github.com"}
	granted = map[string]any{"access_token": "gho_16C7e42F292c6912E7710c838347Ae178B4a", "token_type": "bearer", "scope": "read:org"}
)

// userAPI answers /user and the organization memberships of the user ada for
// the token of granted only.
func (e *signInEnv) userAPI() {
	authorized := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+granted["access_token"].(string) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
			return false
		}
		return true
	}
	e.mux.HandleFunc("GET /api/v3/user", func(w http.ResponseWriter, r *http.Request) {
		if authorized(w, r) {
			writeJSON(w, http.StatusOK, map[string]any{"login": "ada", "id": 1, "type": "User", "name": "Ada Okafor",
				"avatar_url": "https://avatars.githubusercontent.com/u/1?v=4", "html_url": "https://github.com/ada"})
		}
	})
	membership := func(role, login string, id int) map[string]any {
		return map[string]any{
			"url": "https://api.github.com/orgs/" + login + "/memberships/ada", "state": "active", "role": role,
			"organization_url": "https://api.github.com/orgs/" + login,
			"organization":     map[string]any{"login": login, "id": id, "avatar_url": "https://avatars.githubusercontent.com/u/" + login, "description": nil},
			"user":             map[string]any{"login": "ada", "id": 1, "type": "User"},
			"permissions":      map[string]any{"can_create_repository": true},
		}
	}
	e.mux.HandleFunc("GET /api/v3/user/memberships/orgs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "active" {
			e.t.Errorf("memberships query = %q, want state=active", r.URL.RawQuery)
		}
		if authorized(w, r) {
			writeJSON(w, http.StatusOK, []any{
				membership("member", "zeta-labs", 30), membership("admin", "Kestrel", 20), membership("admin", "acme", 10),
			})
		}
	})
}

func TestSignInListsTheAccountsOfTheUser(t *testing.T) {
	e := newSignInEnv(t, 0, pending, granted)
	e.userAPI()
	ctx := context.Background()

	in, err := e.svc.StartSignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if in.UserCode != "WDJB-MJHT" || in.VerificationURI != "https://github.com/login/device" || in.ID == "" {
		t.Fatalf("sign-in = %+v", in)
	}

	if got, err := e.svc.SignInResult(ctx, in.ID); err != nil || got.Status != "pending" {
		t.Fatalf("before the user enters the code: %+v, %v; want pending", got, err)
	}
	got, err := e.svc.SignInResult(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The organizations that the user owns come first, then the personal
	// account, then the other organizations.
	want := []api.GitHubAccount{
		{Login: "acme", Type: "organization", Owner: true},
		{Login: "Kestrel", Type: "organization", Owner: true},
		{Login: "ada", Type: "user", Owner: true},
		{Login: "zeta-labs", Type: "organization", Owner: false},
	}
	if got.Status != "done" || got.Login != "ada" || !reflect.DeepEqual(got.Accounts, want) {
		t.Fatalf("after the user enters the code: %+v\nwant accounts %+v", got, want)
	}

	// A later read gives the same accounts and asks GitHub for no new token.
	again, err := e.svc.SignInResult(ctx, in.ID)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Errorf("second read = %+v, %v; want %+v", again, err, got)
	}
	if n := e.tokenCalls.Load(); n != 2 {
		t.Errorf("token requests = %d, want 2", n)
	}
}

func TestSignInWaitsForTheIntervalOfGitHub(t *testing.T) {
	e := newSignInEnv(t, 60, granted)
	ctx := context.Background()
	in, err := e.svc.StartSignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.SignInResult(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" || got.Interval != 60 {
		t.Errorf("result = %+v, want pending with interval 60", got)
	}
	if n := e.tokenCalls.Load(); n != 0 {
		t.Errorf("token requests before the interval = %d, want 0: GitHub answers slow_down", n)
	}
}

func TestSignInEndStates(t *testing.T) {
	tests := []struct {
		name         string
		answer       map[string]any
		wantStatus   string
		wantInterval int
		wantErr      string
	}{
		{"code expired", map[string]any{"error": "expired_token", "error_description": "The device_code has expired."}, "expired", 0, ""},
		{"user cancelled", map[string]any{"error": "access_denied", "error_description": "The authorization request was denied."}, "denied", 0, ""},
		{"too fast", map[string]any{"error": "slow_down", "error_description": "Too many requests.", "interval": 10}, "pending", 10, ""},
		{"device flow off", map[string]any{"error": "device_flow_disabled",
			"error_description": "Device Flow must be explicitly enabled for this App"}, "", 0, "Device Flow must be explicitly enabled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newSignInEnv(t, 0, tt.answer)
			ctx := context.Background()
			in, err := e.svc.StartSignIn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got, err := e.svc.SignInResult(ctx, in.ID)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one with %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tt.wantStatus || (tt.wantInterval != 0 && got.Interval != tt.wantInterval) {
				t.Errorf("result = %+v, want status %s and interval %d", got, tt.wantStatus, tt.wantInterval)
			}
		})
	}
}

func TestSignInOfAnUnknownID(t *testing.T) {
	e := newSignInEnv(t, 0, granted)
	got, err := e.svc.SignInResult(context.Background(), "after-a-restart")
	if err != nil || got.Status != "expired" {
		t.Errorf("result = %+v, %v; want expired, so that the browser starts again", got, err)
	}
}

func TestSignInWithoutClientID(t *testing.T) {
	e := newEnv(t)
	e.mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		t.Error("runplane asked GitHub for a device code without a client ID")
	})
	if e.svc.CanSignIn() {
		t.Error("CanSignIn = true without a client ID")
	}
	if _, err := e.svc.StartSignIn(context.Background()); err == nil {
		t.Error("StartSignIn without a client ID: no error")
	}
}
