package shell

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func insertSandbox(t *testing.T, st *store.Store, id string, state sandbox.State, debug bool, jobID string) {
	t.Helper()
	err := st.InsertSandbox(context.Background(), store.Sandbox{
		ID: id, PoolID: "pool", ServerID: "srv", Mode: sandbox.ModeContainer, State: state, Debug: debug,
		JobID: jobID, VCPU: 2, MemoryMB: 4096, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLookupStore(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	job, err := st.UpsertJob(ctx, store.JobMessage{
		RunnerRequestID: 7, State: store.JobRunning, Repository: "octo/api", JobName: "build",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobEnrichment(ctx, job.ID, store.JobEnrichment{Actor: "octocat"}); err != nil {
		t.Fatal(err)
	}
	// The sandboxes of each group started in the same second, so their IDs
	// share the first 8 characters.
	insertSandbox(t, st, "01jbd5x7aaaaaaaaaaaaaaaaaa", sandbox.StateIdle, false, "")
	insertSandbox(t, st, "01jbd5x7bbbbbbbbbbbbbbbbbb", sandbox.StateBusy, true, job.ID)
	insertSandbox(t, st, "01jbd5x7cccccccccccccccccc", sandbox.StateIdle, false, "")
	insertSandbox(t, st, "01jbd6aaaaaaaaaaaaaaaaaaaa", sandbox.StateBusy, true, "")
	insertSandbox(t, st, "01jbd6bbbbbbbbbbbbbbbbbbbb", sandbox.StateBusy, true, "")
	insertSandbox(t, st, "01jbd7aaaaaaaaaaaaaaaaaaaa", sandbox.StateGone, true, "")
	insertSandbox(t, st, "01jbd7bbbbbbbbbbbbbbbbbbbb", sandbox.StateIdle, false, "")

	lookup := LookupStore(st)
	tests := []struct {
		name    string
		want    Target
		wantErr error
	}{
		{"01jbd5x7", Target{SandboxID: "01jbd5x7bbbbbbbbbbbbbbbbbb", State: sandbox.StateBusy, Debug: true,
			Actor: "octocat", Repository: "octo/api", Job: "build"}, nil},
		{"01jbd5x7aaaaaaaaaaaaaaaaaa", Target{SandboxID: "01jbd5x7aaaaaaaaaaaaaaaaaa", State: sandbox.StateIdle}, nil},
		{"01jbd6aa", Target{SandboxID: "01jbd6aaaaaaaaaaaaaaaaaaaa", State: sandbox.StateBusy, Debug: true}, nil},
		{"01jbd6", Target{}, ErrAmbiguous},
		{"01jbd7", Target{SandboxID: "01jbd7bbbbbbbbbbbbbbbbbbbb", State: sandbox.StateIdle}, nil},
		{"01jbd7aaaaaaaaaaaaaaaaaaaa", Target{}, ErrNotFound},
		{"01jbd9zz", Target{}, ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := lookup(ctx, tt.name)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// shellDriver is a driver that only opens shells.
type shellDriver struct {
	sandbox.Driver
	term sandbox.Terminal
	got  []string
}

func (d *shellDriver) Shell(_ context.Context, id string, _ sandbox.TermSize) (sandbox.Terminal, error) {
	d.got = append(d.got, id)
	return d.term, nil
}

func TestDriversOpenOnServerOfSandbox(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	insertSandbox(t, st, debugID, sandbox.StateBusy, true, "") // on server "srv"
	agent := &shellDriver{term: &fakeTerm{}}
	d := Drivers{Store: st, Driver: func(serverID string) sandbox.Driver {
		if serverID == "srv" {
			return agent
		}
		return nil
	}}

	term, err := d.Open(ctx, debugID, sandbox.TermSize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	if term != agent.term || len(agent.got) != 1 || agent.got[0] != debugID {
		t.Errorf("shell opened on srv for %q, want %q", agent.got, debugID)
	}

	d.Driver = func(string) sandbox.Driver { return nil }
	if _, err := d.Open(ctx, debugID, sandbox.TermSize{Rows: 24, Cols: 80}); err == nil {
		t.Error("Open on a server that is not connected = nil error")
	}
}
