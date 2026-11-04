package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

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
