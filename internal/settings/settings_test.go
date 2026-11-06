package settings

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/policy"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

func newService(t *testing.T, c Config) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c.Store, c.Bus = st, &events.Bus{}
	return New(c), st
}

// The dashboard refuses allow_public_repos in each mode but VM, so a mode
// change by install.sh must not keep it. install.sh runs set-mode at each
// update with --mode, and in VM mode the setting stays: a server in a
// different mode then waits.
func TestSetModeTurnsPublicReposOff(t *testing.T) {
	ctx := context.Background()
	s, st := newService(t, Config{DevInsecure: true})
	if err := st.CreateServer(ctx, store.Server{
		ID: "01jbz0000000000000000srv02", Name: "fsn1-b", Kind: store.ServerAgent, Status: store.ServerOnline,
		ServerFacts: store.ServerFacts{Arch: "x64", Mode: sandbox.ModeContainer},
	}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		mode       sandbox.Mode
		wantPublic bool
	}{
		{sandbox.ModeVM, true},
		{sandbox.ModeDev, false},
		{sandbox.ModeContainer, false},
	} {
		if _, err := st.UpdateSettings(ctx, func(st *store.Settings) error {
			st.Mode, st.AllowPublicRepos = sandbox.ModeVM, true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		got, err := s.SetMode(ctx, audit.Actor{}, tt.mode)
		if err != nil {
			t.Fatalf("set-mode %s: %v", tt.mode, err)
		}
		if got.Mode != tt.mode || got.AllowPublicRepos != tt.wantPublic {
			t.Errorf("set-mode %s: mode %s, allow_public_repos %v; want %s, %v", tt.mode, got.Mode, got.AllowPublicRepos, tt.mode, tt.wantPublic)
		}
	}
}

// After the first start, the mode setting wins over RUNPLANE_MODE, so that
// the dashboard and set-mode can change it.
func TestInitModeKeepsTheModeOfTheFirstStart(t *testing.T) {
	ctx := context.Background()
	first, st := newService(t, Config{Mode: sandbox.ModeContainer})
	if m, err := first.InitMode(ctx); err != nil || m != sandbox.ModeContainer {
		t.Fatalf("first start: %s, %v; want container", m, err)
	}
	later := New(Config{Store: st, Bus: &events.Bus{}, Mode: sandbox.ModeVM})
	if m, err := later.InitMode(ctx); err != nil || m != sandbox.ModeContainer {
		t.Fatalf("start with RUNPLANE_MODE=vm: %s, %v; want the container mode of the settings", m, err)
	}
	if _, err := first.SetMode(ctx, audit.Actor{}, sandbox.ModeDev); err == nil {
		t.Fatal("set-mode dev without RUNPLANE_DEV_INSECURE has no error")
	}

	dev, _ := newService(t, Config{Mode: sandbox.ModeDev})
	if _, err := dev.InitMode(ctx); !errors.Is(err, policy.ErrDevInsecure) {
		t.Errorf("dev mode without RUNPLANE_DEV_INSECURE: %v, want ErrDevInsecure", err)
	}
	none, _ := newService(t, Config{})
	if _, err := none.InitMode(ctx); !errors.Is(err, policy.ErrNoMode) {
		t.Errorf("no mode: %v, want ErrNoMode", err)
	}
}

// The other services follow the settings through one event for each change.
func TestUpdateSendsOneEventForEachChange(t *testing.T) {
	ctx := t.Context()
	s, st := newService(t, Config{})
	changes := events.Subscribe[events.SettingsChanged](ctx, s.bus)
	rename := func(name string) func(*store.Settings) error {
		return func(st *store.Settings) error {
			st.InstanceName = name
			return nil
		}
	}
	if _, err := s.Update(ctx, audit.Actor{UserID: "u1"}, rename(" ci ")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, audit.Actor{UserID: "u1"}, rename("ci")); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-changes:
		if c.Old.InstanceName != "runplane" || c.New.InstanceName != "ci" {
			t.Errorf("change %q to %q, want runplane to ci", c.Old.InstanceName, c.New.InstanceName)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event for the change")
	}
	select {
	case c := <-changes:
		t.Errorf("an event for a change with no effect: %+v", c)
	case <-time.After(50 * time.Millisecond):
	}
	entries, _, err := st.ListAudit(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Action != "settings.update" || entries[0].UserID != "u1" {
		t.Errorf("audit entries %+v, want one settings.update by u1", entries)
	}
}
