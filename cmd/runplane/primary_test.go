package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/store"
)

func TestLogSetupCodeKeepsAValidCode(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	a := auth.NewService(st, nil, nil)
	var out bytes.Buffer

	// install.sh prints a code, then runplane restarts.
	printed, err := a.NewSetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := logSetupCode(ctx, st, a, slog.New(slog.NewTextHandler(&out, nil))); err != nil {
		t.Fatal(err)
	}
	if loggedCode(out.String()) != "" {
		t.Errorf("serve made a new code while one was valid:\n%s", out.String())
	}
	if _, _, err := a.Setup(ctx, printed, "Ada", "ada@example.com", "a long password", "127.0.0.1", "test"); err != nil {
		t.Fatalf("the printed code stopped working after a restart: %v", err)
	}
}

func TestLogSetupCodeReplacesAnExpiredCode(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	a := auth.NewService(st, nil, nil)
	old := time.Now().Add(-48 * time.Hour)
	if err := st.CreateSetupCode(ctx, store.SetupCode{CodeHash: "expired", CreatedAt: old, ExpiresAt: old.Add(auth.SetupCodeTTL)}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := logSetupCode(ctx, st, a, slog.New(slog.NewTextHandler(&out, nil))); err != nil {
		t.Fatal(err)
	}
	code := loggedCode(out.String())
	if code == "" {
		t.Fatalf("no new code in the log:\n%s", out.String())
	}
	if _, _, err := a.Setup(ctx, code, "Ada", "ada@example.com", "a long password", "127.0.0.1", "test"); err != nil {
		t.Fatalf("the logged code does not work: %v", err)
	}
}

func loggedCode(log string) string {
	if m := regexp.MustCompile(`code=(\S+)`).FindStringSubmatch(log); m != nil {
		return m[1]
	}
	return ""
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
