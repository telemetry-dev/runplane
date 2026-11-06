package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// After the first start the settings win over RUNPLANE_MODE, so install.sh
// --mode must change the mode through set-mode.
func TestSetModeStoresTheMode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("RUNPLANE_CONFIG_DIR", dir)
	t.Setenv("RUNPLANE_DATA_DIR", dir)
	t.Setenv("RUNPLANE_DEV_INSECURE", "")
	st, err := store.Open(dir + "/runplane.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateSettings(ctx, func(s *store.Settings) error {
		s.Mode, s.AllowPublicRepos = sandbox.ModeVM, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	for _, bad := range []string{"dev", "kvm"} {
		if err := setMode(ctx, []string{bad}); err == nil {
			t.Errorf("set-mode %s has no error", bad)
		}
	}
	if s := settingsIn(t, dir); s.Mode != sandbox.ModeVM {
		t.Fatalf("a refused set-mode changed the mode to %s", s.Mode)
	}

	if err := setMode(ctx, []string{"container"}); err != nil {
		t.Fatal(err)
	}
	// The dashboard refuses to save settings with public repositories on in
	// container mode, so set-mode turns them off.
	if s := settingsIn(t, dir); s.Mode != sandbox.ModeContainer || s.AllowPublicRepos {
		t.Fatalf("mode %s, public repositories %v; want container without public repositories", s.Mode, s.AllowPublicRepos)
	}
}

// install.sh --domain runs set-domain before the first start of runplane,
// thus before the database exists.
func TestSetDomainStoresTheDomain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("RUNPLANE_CONFIG_DIR", dir)
	t.Setenv("RUNPLANE_DATA_DIR", dir)

	if err := setDomain(ctx, []string{" CI.Example.com "}); err != nil {
		t.Fatal(err)
	}
	if s := settingsIn(t, dir); s.Domain != "ci.example.com" {
		t.Fatalf("domain %q, want ci.example.com", s.Domain)
	}

	for _, bad := range []string{"192.0.2.1", "localhost", "https://ci.example.com"} {
		if err := setDomain(ctx, []string{bad}); err == nil {
			t.Errorf("set-domain %s has no error", bad)
		}
	}
	if s := settingsIn(t, dir); s.Domain != "ci.example.com" {
		t.Fatalf("a refused set-domain changed the domain to %q", s.Domain)
	}

	if err := setDomain(ctx, []string{""}); err != nil {
		t.Fatal(err)
	}
	if s := settingsIn(t, dir); s.Domain != "" {
		t.Fatalf("domain %q after set-domain '', want none", s.Domain)
	}
}

// install.sh makes the setup code before the first start of runplane, thus
// before the database exists. serve then keeps that code.
func TestSetupCodeBeforeTheFirstStart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("RUNPLANE_CONFIG_DIR", dir)
	t.Setenv("RUNPLANE_DATA_DIR", dir)

	if err := setupCode(ctx, nil); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir + "/runplane.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if code, err := auth.NewService(st, nil, nil).EnsureSetupCode(ctx); err != nil || code != "" {
		t.Fatalf("serve makes the setup code %q (%v), want none: the code of setup-code is valid", code, err)
	}
}

// An agent must not install the latest release, which can be newer than
// the release of its primary.
func TestUpdateRefusesAnAgent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNPLANE_CONFIG_DIR", dir)
	t.Setenv("RUNPLANE_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), []byte(`{"primary": "203.0.113.5:8485"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"-check"}} {
		if err := updateCmd(context.Background(), args); !errors.Is(err, errAgentUpdate) {
			t.Errorf("update %v on an agent: %v, want the refusal", args, err)
		}
	}
}

// An agent reads no database, and the primary makes its database at the
// first start. A command that made a database would report a change that
// runplane does not use. Only set-domain and setup-code make one on a
// primary, because install.sh runs them before the first start.
func TestCommandsMakeNoDatabase(t *testing.T) {
	for _, tt := range []struct {
		agent bool
		args  []string
	}{
		{true, []string{"setup-code"}},
		{true, []string{"reset-password", "admin@example.com"}},
		{true, []string{"set-mode", "vm"}},
		{true, []string{"set-domain", "ci.example.com"}},
		{false, []string{"reset-password", "admin@example.com"}},
		{false, []string{"set-mode", "vm"}},
	} {
		dir := t.TempDir()
		t.Setenv("RUNPLANE_CONFIG_DIR", dir)
		t.Setenv("RUNPLANE_DATA_DIR", dir)
		if tt.agent {
			if err := os.WriteFile(filepath.Join(dir, "agent.json"), []byte(`{"primary": "203.0.113.5:8485"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		err := run(context.Background(), tt.args)
		if err == nil || tt.agent && !errors.Is(err, errAgent) {
			t.Errorf("runplane %v (agent %v): %v, want the refusal", tt.args, tt.agent, err)
		}
		if exists(filepath.Join(dir, "runplane.db")) {
			t.Errorf("runplane %v (agent %v) made a database", tt.args, tt.agent)
		}
	}
}

func settingsIn(t *testing.T, dir string) store.Settings {
	t.Helper()
	st, err := store.Open(dir + "/runplane.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
