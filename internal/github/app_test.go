package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/runplane/runplane/internal/ghapp"
	"github.com/runplane/runplane/internal/store"
)

func TestManifest(t *testing.T) {
	tests := []struct {
		owner           ghapp.Account
		wantAction      string
		wantPermissions map[string]any
	}{
		{ghapp.Account{Type: ghapp.Organization, Login: "acme"}, "/organizations/acme/settings/apps/new",
			map[string]any{"organization_self_hosted_runners": "write", "actions": "write", "metadata": "read",
				"contents": "write", "pull_requests": "write", "workflows": "write"}},
		{ghapp.Account{Type: ghapp.User}, "/settings/apps/new",
			map[string]any{"administration": "write", "actions": "write", "metadata": "read",
				"contents": "write", "pull_requests": "write", "workflows": "write"}},
	}
	for _, tt := range tests {
		t.Run(tt.owner.Type, func(t *testing.T) {
			e := newEnv(t)
			action, manifest, err := e.svc.Manifest(context.Background(), "https://ci.example.com", tt.owner)
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(action)
			if u.Path != tt.wantAction {
				t.Errorf("action path = %q, want %q", u.Path, tt.wantAction)
			}
			// No webhook: the manifest has no hook_attributes and no default_events.
			var m map[string]any
			json.Unmarshal([]byte(manifest), &m)
			delete(m, "name")
			delete(m, "description")
			want := map[string]any{"url": "https://ci.example.com", "redirect_url": "https://ci.example.com/api/github/callback",
				"setup_url": "https://ci.example.com/api/github/setup", "setup_on_update": true, "public": false,
				"default_permissions": tt.wantPermissions}
			if !reflect.DeepEqual(m, want) {
				t.Errorf("manifest = %v, want %v", m, want)
			}
		})
	}
}

// GitHub App names are unique on GitHub, and GitHub refuses names longer
// than 34 characters.
func TestManifestNamesTheApp(t *testing.T) {
	tests := []struct{ instance, want string }{
		{"", `^runplane-[a-z2-7]{6}$`}, // the default instance name
		{" Acme CI ", `^Acme CI-[a-z2-7]{6}$`},
		{"The continuous integration of Acme Corporation", `^The continuous integration-[a-z2-7]{6}$`},
	}
	for _, tt := range tests {
		e := newEnv(t)
		ctx := context.Background()
		if tt.instance != "" {
			if _, err := e.st.UpdateSettings(ctx, func(s *store.Settings) error { s.InstanceName = tt.instance; return nil }); err != nil {
				t.Fatal(err)
			}
		}
		_, manifest, err := e.svc.Manifest(ctx, "https://ci.example.com", ghapp.Account{Type: ghapp.User})
		if err != nil {
			t.Fatal(err)
		}
		var m struct{ Name string }
		if err := json.Unmarshal([]byte(manifest), &m); err != nil || !regexp.MustCompile(tt.want).MatchString(m.Name) {
			t.Errorf("instance %q: app name %q, %v; want %s", tt.instance, m.Name, err, tt.want)
		}
	}
}

func TestCallbackStoresSealedApp(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	conversions := 0
	e.mux.HandleFunc("POST /api/v3/app-manifests/code123/conversions", func(w http.ResponseWriter, r *http.Request) {
		conversions++
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": 4242, "slug": "runplane-ab12cd", "name": "runplane-ab12cd", "client_id": "Iv23.abc",
			"owner":    map[string]any{"login": "acme", "id": 9, "type": "Organization"},
			"html_url": "https://github.com/apps/runplane-ab12cd", "client_secret": "secret-1", "webhook_secret": nil,
			"pem": string(testPEM()), "created_at": "2026-11-04T10:00:00Z",
		})
	})
	action, _, err := e.svc.Manifest(ctx, "https://ci.example.com", ghapp.Account{Type: ghapp.Organization, Login: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(action)
	state := u.Query().Get("state")

	to, err := e.svc.Callback(ctx, "code123", state)
	if err != nil {
		t.Fatal(err)
	}
	if to != e.srv.URL+"/apps/runplane-ab12cd/installations/new" {
		t.Errorf("redirect = %q", to)
	}
	app, err := e.st.GetGitHubApp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if app.AppID != 4242 || app.ClientID != "Iv23.abc" || app.OwnerLogin != "acme" || app.OwnerType != "Organization" {
		t.Errorf("app = %+v", app)
	}
	if strings.Contains(string(app.PrivateKeyEnc), "PRIVATE KEY") {
		t.Error("the store has the key in clear text")
	}
	if key, err := e.box.Open(app.PrivateKeyEnc); err != nil || string(key) != string(testPEM()) {
		t.Errorf("sealed key does not open to the PEM: %v", err)
	}

	// A state works one time, and a bad state does not reach GitHub.
	for _, s := range []string{state, "bad-state"} {
		if _, err := e.svc.Callback(ctx, "code123", s); err == nil {
			t.Errorf("callback with the state %q succeeded", s)
		}
	}
	if conversions != 1 {
		t.Errorf("conversions = %d, want 1", conversions)
	}
}

// GitHub makes the app on the personal account when the organization does
// not exist or the user cannot make apps there.
func TestCallbackRejectsAppOfAnotherAccount(t *testing.T) {
	e := newEnv(t)
	e.connect()
	ctx := context.Background()
	e.mux.HandleFunc("POST /api/v3/app-manifests/code123/conversions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": 4242, "slug": "runplane-ab12cd", "name": "runplane-ab12cd", "client_id": "Iv23.abc",
			"owner":    map[string]any{"login": "ada", "id": 1, "type": "User"},
			"html_url": "https://github.com/apps/runplane-ab12cd", "client_secret": "secret-1", "webhook_secret": nil,
			"pem": string(testPEM()), "created_at": "2026-11-04T10:00:00Z",
		})
	})
	action, _, err := e.svc.Manifest(ctx, "https://ci.example.com", ghapp.Account{Type: ghapp.Organization, Login: "Telemetry"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(action)

	_, err = e.svc.Callback(ctx, "code123", u.Query().Get("state"))
	if err == nil || !strings.Contains(err.Error(), "ada") || !strings.Contains(err.Error(), "Telemetry") ||
		!strings.Contains(err.Error(), e.srv.URL+"/settings/apps/runplane-ab12cd/advanced") {
		t.Fatalf("err = %v, want the two accounts and the delete page of the app", err)
	}
	app, err := e.st.GetGitHubApp(ctx)
	if err != nil || app.AppID != testAppID {
		t.Errorf("app = %+v, %v; want the earlier app %d, unchanged", app, err, testAppID)
	}
}

func TestDisconnect(t *testing.T) {
	e, f, _ := listeningEnv(t)
	ctx := context.Background()
	if err := e.svc.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.GetGitHubApp(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the app is still there: %v", err)
	}
	insts, _ := e.st.ListInstallations(ctx)
	sets, _ := e.st.ListScaleSets(ctx)
	if len(insts) != 0 || len(sets) != 0 {
		t.Errorf("installations = %+v, scale sets = %+v", insts, sets)
	}
	if st := f.state(); len(st.sets) != 0 || st.sessions != 0 {
		t.Errorf("on GitHub: scale sets = %+v, open sessions = %d", st.sets, st.sessions)
	}
	if err := e.svc.Disconnect(ctx); err != nil {
		t.Errorf("second disconnect: %v", err)
	}
}

func TestDisconnectNamesScaleSetsLeftOnGitHub(t *testing.T) {
	e, f, _ := listeningEnv(t)
	f.mu.Lock()
	f.agents[50] = "broken"
	f.mu.Unlock()
	err := e.svc.Disconnect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "scale sets runplane.") {
		t.Fatalf("err = %v, want the scale set name", err)
	}
	if _, err := e.st.GetGitHubApp(context.Background()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the app is still there after a GitHub failure: %v", err)
	}
}
