package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/ghrest"
	"github.com/runplane/runplane/internal/store"
)

func TestManifest(t *testing.T) {
	tests := []struct {
		owner           ghrest.Account
		wantAction      string
		wantPermissions map[string]any
	}{
		{ghrest.Account{Type: ghrest.Organization, Login: "acme"}, "/organizations/acme/settings/apps/new",
			map[string]any{"organization_self_hosted_runners": "write", "actions": "write", "metadata": "read",
				"contents": "write", "pull_requests": "write", "workflows": "write"}},
		{ghrest.Account{Type: ghrest.User}, "/settings/apps/new",
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

			// The store keeps only the hash of the state, for one hour.
			sum := sha256.Sum256([]byte(u.Query().Get("state")))
			stored, err := e.st.ConsumeManifestState(context.Background(), hex.EncodeToString(sum[:]), time.Now())
			if err != nil {
				t.Fatalf("no stored state with the SHA-256 of the state: %v", err)
			}
			if d := time.Until(stored.ExpiresAt); d < 59*time.Minute || d > time.Hour {
				t.Errorf("state expires in %s, want 1 hour", d)
			}
		})
	}
}

func TestAppName(t *testing.T) {
	tests := []struct{ instance, want string }{
		{"", `^runplane-[a-z2-7]{6}$`},
		{" Acme CI ", `^Acme CI-[a-z2-7]{6}$`},
		// GitHub refuses app names longer than 34 characters.
		{"The continuous integration of Acme Corporation", `^The continuous integration-[a-z2-7]{6}$`},
	}
	for _, tt := range tests {
		if got := appName(tt.instance); !regexp.MustCompile(tt.want).MatchString(got) {
			t.Errorf("appName(%q) = %q, want %s", tt.instance, got, tt.want)
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
	action, _, err := e.svc.Manifest(ctx, "https://ci.example.com", ghrest.Account{Type: ghrest.Organization, Login: "acme"})
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
	action, _, err := e.svc.Manifest(ctx, "https://ci.example.com", ghrest.Account{Type: ghrest.Organization, Login: "Telemetry"})
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

func TestAppDeleteURL(t *testing.T) {
	tests := []struct {
		ownerType, want string
	}{
		{"Organization", "/organizations/acme/settings/apps/runplane-ab12cd/advanced"},
		{"User", "/settings/apps/runplane-ab12cd/advanced"},
	}
	e := newEnv(t)
	for _, tt := range tests {
		got := e.svc.appDeleteURL(store.GitHubApp{Slug: "runplane-ab12cd", OwnerLogin: "acme", OwnerType: tt.ownerType})
		if got != e.srv.URL+tt.want {
			t.Errorf("appDeleteURL(%s) = %q, want %q", tt.ownerType, got, tt.want)
		}
	}
}
