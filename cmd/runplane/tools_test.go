package main

import (
	"context"
	"testing"

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
