package github

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/ghrest"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/store"
)

// Manifest returns the GitHub URL for the manifest form and the manifest
// JSON. accountType is "organization" or "user".
func (s *Service) Manifest(ctx context.Context, base, accountType, organization string) (string, string, error) {
	if accountType != "organization" && accountType != "user" {
		return "", "", fmt.Errorf("unknown account type %q", accountType)
	}
	settings, err := s.st.GetSettings(ctx)
	if err != nil {
		return "", "", err
	}
	state := rand.Text()
	now := time.Now()
	err = s.st.CreateManifestState(ctx, store.ManifestState{
		StateHash: hashState(state), AccountType: accountType, Organization: organization,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		return "", "", err
	}

	manifest, err := json.Marshal(map[string]any{
		"name":                appName(settings.InstanceName),
		"url":                 base,
		"description":         "Runs GitHub Actions jobs in runplane sandboxes at " + base + ".",
		"redirect_url":        base + "/api/github/callback",
		"setup_url":           base + "/api/github/setup",
		"setup_on_update":     true,
		"public":              false,
		"default_permissions": permissions(accountType),
	})
	if err != nil {
		return "", "", err
	}
	action := s.web + "/settings/apps/new"
	if accountType == "organization" {
		action = s.web + "/organizations/" + url.PathEscape(organization) + "/settings/apps/new"
	}
	return action + "?state=" + url.QueryEscape(state), string(manifest), nil
}

// permissions are the GitHub App permissions of an account type. An
// organization uses organization runners; a personal account can only
// use repository runners, which need administration.
func permissions(accountType string) map[string]string {
	p := map[string]string{"actions": "write", "metadata": "read"}
	if accountType == "organization" {
		p["organization_self_hosted_runners"] = "write"
	} else {
		p["administration"] = "write"
	}
	return p
}

// appName makes a name from the instance name. GitHub App names are unique
// on GitHub and have at most 34 characters, so it adds a random suffix.
func appName(instance string) string {
	name := cmp.Or(strings.TrimSpace(instance), "runplane")
	if r := []rune(name); len(r) > 27 {
		name = strings.TrimSpace(string(r[:27]))
	}
	return name + "-" + strings.ToLower(rand.Text()[:6])
}

// Callback converts the manifest code into the app and stores it. It
// returns the installation page of the new app.
func (s *Service) Callback(ctx context.Context, _, code, state string) (string, error) {
	if code == "" || state == "" {
		return "", errors.New("GitHub did not send the code and the state. Start again")
	}
	requested, err := s.st.ConsumeManifestState(ctx, hashState(state), time.Now())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", errors.New("the GitHub App request is unknown or older than one hour. Start again")
		}
		return "", err
	}
	var conv struct {
		ID            int64   `json:"id"`
		Slug          string  `json:"slug"`
		Name          string  `json:"name"`
		ClientID      string  `json:"client_id"`
		ClientSecret  string  `json:"client_secret"`
		WebhookSecret string  `json:"webhook_secret"`
		PEM           string  `json:"pem"`
		HTMLURL       string  `json:"html_url"`
		Owner         account `json:"owner"`
	}
	err = s.api.JSON(ctx, ghrest.Call{Method: http.MethodPost, Path: "/app-manifests/" + url.PathEscape(code) + "/conversions"}, &conv)
	if err != nil {
		return "", fmt.Errorf("make the GitHub App: %w", err)
	}
	// For an organization that does not exist, or where the user cannot make
	// apps, GitHub makes the app on the personal account.
	if requested.AccountType == "organization" && !strings.EqualFold(conv.Owner.Login, requested.Organization) {
		made := store.GitHubApp{Slug: conv.Slug, OwnerLogin: conv.Owner.Login, OwnerType: conv.Owner.Type, HTMLURL: conv.HTMLURL}
		return "", fmt.Errorf("GitHub made the app on %s, not on the organization %s: the organization does not exist, "+
			"or you cannot make apps in it. Delete the app at %s, then start again", conv.Owner.Login, requested.Organization, appDeleteURL(made))
	}
	if _, err := parseKey([]byte(conv.PEM)); err != nil {
		return "", err
	}

	s.ops.Lock()
	defer s.ops.Unlock()
	if err := s.removeApp(ctx); err != nil && !errors.Is(err, errNotConnected) {
		s.log.Warn("remove the earlier GitHub App", "err", err)
	}
	app := store.GitHubApp{
		ID: ids.New(), AppID: conv.ID, Slug: conv.Slug, Name: conv.Name, ClientID: conv.ClientID,
		OwnerLogin: conv.Owner.Login, OwnerType: conv.Owner.Type, HTMLURL: conv.HTMLURL,
		PrivateKeyEnc:   s.box.Seal([]byte(conv.PEM)),
		ClientSecretEnc: s.box.Seal([]byte(conv.ClientSecret)),
		CreatedAt:       time.Now(),
	}
	if conv.WebhookSecret != "" {
		app.WebhookSecretEnc = s.box.Seal([]byte(conv.WebhookSecret))
	}
	if err := s.st.SaveGitHubApp(ctx, app); err != nil {
		return "", err
	}
	s.dropCreds()
	s.changed()
	s.log.Info("made GitHub App", "slug", app.Slug, "owner", app.OwnerLogin)
	return s.web + "/apps/" + url.PathEscape(app.Slug) + "/installations/new", nil
}

// Disconnect stops the listeners, deletes the scale sets and their runners
// on GitHub, and removes the app from runplane. GitHub has no API to delete
// an app, so the app stays on GitHub; the log gives the page where the
// owner deletes it.
func (s *Service) Disconnect(ctx context.Context) error {
	s.ops.Lock()
	defer s.ops.Unlock()
	c, err := s.creds(ctx)
	if errors.Is(err, errNotConnected) {
		return nil
	}
	if err != nil {
		return err
	}
	err = s.removeApp(ctx)
	s.changed()
	s.log.Info("disconnected GitHub; delete the app on GitHub", "url", appDeleteURL(c.app))
	return err
}

// appDeleteURL is the GitHub page with the "Delete GitHub App" button.
// It is on the host of the app page, so GitHub Enterprise Server works.
func appDeleteURL(app store.GitHubApp) string {
	web := "https://github.com"
	if u, err := url.Parse(app.HTMLURL); err == nil && u.Host != "" {
		web = u.Scheme + "://" + u.Host
	}
	slug := url.PathEscape(app.Slug)
	if app.OwnerType == "Organization" {
		return web + "/organizations/" + url.PathEscape(app.OwnerLogin) + "/settings/apps/" + slug + "/advanced"
	}
	return web + "/settings/apps/" + slug + "/advanced"
}

// removeApp deletes all scale sets on GitHub and all app rows. A scale set
// that GitHub does not delete stays on GitHub; the error names it. s.ops
// must be held.
func (s *Service) removeApp(ctx context.Context) error {
	c, err := s.creds(ctx)
	if err != nil {
		return err
	}
	sets, err := s.st.ListScaleSets(ctx)
	if err != nil {
		return err
	}
	var failed []string
	for _, set := range sets {
		s.stopListener(set.ID)
		if err := s.deleteRemote(ctx, set.ID); err != nil {
			s.log.Warn("delete scale set on GitHub", "scale_set", set.Name, "err", err)
			failed = append(failed, set.Name)
		}
	}
	if err := s.st.DeleteGitHubApp(ctx, c.app.ID); err != nil {
		return err
	}
	for _, set := range sets {
		if err := s.st.DeleteScaleSet(ctx, set.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	s.dropCreds()
	s.checkMu.Lock()
	clear(s.checks)
	s.checkMu.Unlock()
	if len(failed) > 0 {
		return fmt.Errorf("runplane forgot the GitHub App, but GitHub did not delete the scale sets %s. "+
			"Remove them in the Actions runner settings on GitHub", strings.Join(failed, ", "))
	}
	return nil
}

func hashState(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}
