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
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

func TestLogSetupCodeKeepsAValidCode(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	a := auth.NewService(st, nil)
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
	a := auth.NewService(st, nil)
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

// The cache and the mirror find a sandbox of the primary by its address, so
// an agent must not give its address to a sandbox of the primary.
func TestRecordIPTakesOnlySandboxesOfTheServer(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	for _, b := range []store.Sandbox{
		{ID: "primary-box", ServerID: "local", IP: "10.213.0.5"},
		{ID: "agent-box", ServerID: "agent-1"},
	} {
		b.PoolID, b.Mode, b.State, b.VCPU, b.MemoryMB, b.CreatedAt = "pool", sandbox.ModeContainer, sandbox.StateIdle, 2, 4096, time.Now()
		if err := st.InsertSandbox(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	p := &primary{st: st, log: slog.New(slog.DiscardHandler)}
	// The agent lists the sandbox of the primary with the address of its own.
	agentList := func(context.Context) ([]sandbox.Sandbox, error) {
		return []sandbox.Sandbox{{ID: "primary-box", IP: "10.214.0.7"}, {ID: "agent-box", IP: "10.214.0.8"}}, nil
	}

	p.recordIP(ctx, "agent-1", "primary-box", agentList)
	p.recordIP(ctx, "agent-1", "agent-box", agentList)

	for id, want := range map[string]string{"primary-box": "10.213.0.5", "agent-box": "10.214.0.8"} {
		if b, err := st.GetSandbox(ctx, id); err != nil || b.IP != want {
			t.Errorf("sandbox %s: address %q, %v; want %s", id, b.IP, err, want)
		}
	}
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
