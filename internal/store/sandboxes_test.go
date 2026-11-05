package store

import (
	"errors"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

func TestAdvanceSandboxStateTimes(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	must(t, s.InsertSandbox(ctx, Sandbox{ID: "b1", PoolID: "p", ServerID: "v", Mode: sandbox.ModeContainer,
		State: sandbox.StateStarting, VCPU: 2, MemoryMB: 4096, CreatedAt: sec(0)}))
	var b Sandbox
	for _, step := range []struct {
		state  sandbox.State
		reason string
		sec    int
	}{
		{sandbox.StateIdle, "", 1},
		{sandbox.StateIdle, "", 5}, // a repeat keeps the first time
		{sandbox.StateBusy, "", 9},
		{sandbox.StateStopping, "job done", 20},
		{sandbox.StateGone, "removed", 21},
		{sandbox.StateIdle, "", 30}, // a done sandbox keeps its state
	} {
		var err error
		b, _, err = s.AdvanceSandboxState(ctx, "b1", step.state, step.reason, sec(step.sec))
		must(t, err)
	}

	if b.State != sandbox.StateGone {
		t.Errorf("state = %s, want gone", b.State)
	}
	for _, c := range []struct {
		name string
		got  *time.Time
		sec  int
	}{{"idle_at", b.IdleAt, 1}, {"busy_at", b.BusyAt, 9}, {"stopped_at", b.StoppedAt, 21}} {
		if c.got == nil || !c.got.Equal(sec(c.sec)) {
			t.Errorf("%s = %v, want t0+%ds", c.name, c.got, c.sec)
		}
	}
	if b.ExitReason != "job done" || b.Error != "" {
		t.Errorf("exit reason %q, error %q", b.ExitReason, b.Error)
	}

	if _, _, err := s.AdvanceSandboxState(ctx, "missing", sandbox.StateIdle, "", sec(0)); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown sandbox: err = %v, want ErrNotFound", err)
	}
}
