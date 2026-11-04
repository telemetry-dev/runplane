package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/store"
)

func TestManifest(t *testing.T) {
	tests := []struct {
		accountType, org string
		wantAction       string
		wantPermissions  map[string]string
	}{
		{"organization", "acme", "/organizations/acme/settings/apps/new",
			map[string]string{"organization_self_hosted_runners": "write", "actions": "write", "metadata": "read"}},
		{"user", "", "/settings/apps/new",
			map[string]string{"administration": "write", "actions": "write", "metadata": "read"}},
	}
	for _, tt := range tests {
		t.Run(tt.accountType, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			action, manifest, err := e.svc.Manifest(ctx, "https://ci.example.com", tt.accountType, tt.org)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(action)
			if err != nil {
				t.Fatal(err)
			}
			if u.Path != tt.wantAction {
				t.Errorf("action path = %q, want %q", u.Path, tt.wantAction)
			}
			var m struct {
				Name          string            `json:"name"`
				URL           string            `json:"url"`
				RedirectURL   string            `json:"redirect_url"`
				SetupURL      string            `json:"setup_url"`
				SetupOnUpdate bool              `json:"setup_on_update"`
				Public        *bool             `json:"public"`
				Permissions   map[string]string `json:"default_permissions"`
				Hook          any               `json:"hook_attributes"`
				Events        any               `json:"default_events"`
			}
			if err := json.Unmarshal([]byte(manifest), &m); err != nil {
				t.Fatal(err)
			}
			if m.RedirectURL != "https://ci.example.com/api/github/callback" || m.SetupURL != "https://ci.example.com/api/github/setup" {
				t.Errorf("redirect_url = %q, setup_url = %q", m.RedirectURL, m.SetupURL)
			}
			if m.URL != "https://ci.example.com" || !m.SetupOnUpdate || m.Public == nil || *m.Public {
				t.Errorf("url = %q, setup_on_update = %v, public = %v", m.URL, m.SetupOnUpdate, m.Public)
			}
			if m.Hook != nil || m.Events != nil {
				t.Errorf("manifest has a webhook: hook_attributes = %v, default_events = %v", m.Hook, m.Events)
			}
			if !maps.Equal(m.Permissions, tt.wantPermissions) {
				t.Errorf("permissions = %v, want %v", m.Permissions, tt.wantPermissions)
			}
			if !strings.HasPrefix(m.Name, "runplane-") || len(m.Name) != len("runplane-")+6 {
				t.Errorf("name = %q, want runplane-<6 random>", m.Name)
			}

			// The store keeps only the hash of the state.
			state := u.Query().Get("state")
			sum := sha256.Sum256([]byte(state))
			var stored, account string
			var expires int64
			err = e.st.DB().QueryRow("SELECT state_hash, account_type, expires_at FROM manifest_states").Scan(&stored, &account, &expires)
			if err != nil {
				t.Fatal(err)
			}
			if stored != hex.EncodeToString(sum[:]) || account != tt.accountType {
				t.Errorf("stored state %q (%s), want the SHA-256 of the state", stored, account)
			}
			if d := time.Until(time.UnixMilli(expires)); d < 59*time.Minute || d > time.Hour {
				t.Errorf("state expires in %s, want 1 hour", d)
			}
		})
	}
}

func TestManifestNameUsesInstanceName(t *testing.T) {
	tests := []struct{ instance, wantPrefix string }{
		{"Acme CI", "Acme CI-"},
		// GitHub refuses app names longer than 34 characters.
		{"The continuous integration of Acme Corporation", "The continuous integration-"},
	}
	for _, tt := range tests {
		e := newEnv(t)
		ctx := context.Background()
		e.st.UpdateSettings(ctx, func(s *store.Settings) error { s.InstanceName = tt.instance; return nil })
		_, manifest, err := e.svc.Manifest(ctx, "http://localhost:8484", "user", "")
		if err != nil {
			t.Fatal(err)
		}
		var m struct{ Name string }
		json.Unmarshal([]byte(manifest), &m)
		if !strings.HasPrefix(m.Name, tt.wantPrefix) || len(m.Name) > 34 {
			t.Errorf("name = %q (%d characters), want %q and a suffix in 34 characters", m.Name, len(m.Name), tt.wantPrefix)
		}
	}
}

func TestCallbackStoresSealedApp(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	conversions := 0
	e.mux.HandleFunc("POST /api/v3/app-manifests/{code}/conversions", func(w http.ResponseWriter, r *http.Request) {
		conversions++
		if r.PathValue("code") != "code123" {
			t.Errorf("code = %q", r.PathValue("code"))
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": 4242, "slug": "runplane-ab12cd", "node_id": "A_1", "name": "runplane-ab12cd",
			"owner":    map[string]any{"login": "acme", "id": 9, "type": "Organization"},
			"html_url": "https://github.com/apps/runplane-ab12cd", "client_id": "Iv23.abc",
			"client_secret": "secret-1", "webhook_secret": nil, "pem": string(testPEM()),
			"created_at": "2026-11-04T10:00:00Z", "permissions": map[string]string{"actions": "write"}, "events": []string{},
		})
	})
	action, _, err := e.svc.Manifest(ctx, "https://ci.example.com", "organization", "acme")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(action)
	state := u.Query().Get("state")

	to, err := e.svc.Callback(ctx, "https://ci.example.com", "code123", state)
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
	if strings.Contains(string(app.PrivateKeyEnc), "PRIVATE KEY") || strings.Contains(string(app.ClientSecretEnc), "secret-1") {
		t.Error("the store has the key or the client secret in clear text")
	}
	if key, err := e.box.Open(app.PrivateKeyEnc); err != nil || string(key) != string(testPEM()) {
		t.Errorf("sealed key does not open to the PEM: %v", err)
	}
	if secret, err := e.box.Open(app.ClientSecretEnc); err != nil || string(secret) != "secret-1" {
		t.Errorf("sealed client secret = %q, %v", secret, err)
	}

	// A state works one time.
	if _, err := e.svc.Callback(ctx, "https://ci.example.com", "code123", state); err == nil {
		t.Error("second callback with the same state succeeded")
	}
	if _, err := e.svc.Callback(ctx, "https://ci.example.com", "code123", "bad-state"); err == nil {
		t.Error("callback with an unknown state succeeded")
	}
	if conversions != 1 {
		t.Errorf("conversions = %d, want 1: a bad state must not reach GitHub", conversions)
	}
}

func TestSetupRecordsInstallation(t *testing.T) {
	e := newEnv(t)
	e.connect()
	ctx := context.Background()
	other := false
	e.mux.HandleFunc("GET /api/v3/app/installations/{id}", func(w http.ResponseWriter, r *http.Request) {
		e.checkJWT(r)
		app := testAppID
		if other {
			app = 99
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": 77, "app_id": app, "repository_selection": "selected", "suspended_at": nil,
			"account": map[string]any{"login": "octo", "id": 5, "type": "User", "avatar_url": "https://a/5"},
		})
	})
	e.mux.HandleFunc("GET /api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"total_count": 1, "repositories": []map[string]any{
			{"id": 300, "full_name": "octo/app", "private": true, "archived": false, "default_branch": "main"},
		}})
	})

	to, err := e.svc.Setup(ctx, "https://ci.example.com", 77, "install")
	if err != nil {
		t.Fatal(err)
	}
	if to != "/setup/first-job" {
		t.Errorf("first setup goes to %q, want /setup/first-job", to)
	}
	inst, err := e.st.GetInstallation(ctx, 77)
	if err != nil {
		t.Fatal(err)
	}
	if inst.AccountLogin != "octo" || inst.AccountType != "User" || inst.RepositorySelection != "selected" {
		t.Errorf("installation = %+v", inst)
	}
	repos, _ := e.st.ListRepositories(ctx, 77)
	if len(repos) != 1 || repos[0].FullName != "octo/app" || !repos[0].Private {
		t.Errorf("repositories = %+v", repos)
	}

	to, err = e.svc.Setup(ctx, "https://ci.example.com", 77, "update")
	if err != nil || to != "/settings/github" {
		t.Errorf("later setup goes to %q (%v), want /settings/github", to, err)
	}

	other = true
	if _, err := e.svc.Setup(ctx, "https://ci.example.com", 77, "install"); err == nil {
		t.Error("setup accepted an installation of a different app")
	}
}
