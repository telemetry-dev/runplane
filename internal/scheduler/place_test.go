package scheduler

import (
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

func TestPlacement(t *testing.T) {
	tests := []struct {
		name  string
		pool  func(p *store.Pool, ids map[string]string)
		setup func(e *testEnv, ids map[string]string)
		want  string // server name, "" = no server
	}{
		{
			name: "most free memory of the same arch",
			want: "x64-large",
		},
		{
			name: "arm64 pool",
			pool: func(p *store.Pool, _ map[string]string) { p.Arch = "arm64" },
			want: "arm-huge",
		},
		{
			name: "server_ids limits the servers",
			pool: func(p *store.Pool, ids map[string]string) { p.ServerIDs = []string{ids["x64-small"]} },
			want: "x64-small",
		},
		{
			name:  "image not ready",
			setup: func(e *testEnv, ids map[string]string) { e.fleet.noImage[ids["x64-large"]] = true },
			want:  "x64-small",
		},
		{
			name:  "server offline",
			setup: func(e *testEnv, ids map[string]string) { e.fleet.offline[ids["x64-large"]] = true },
			want:  "x64-small",
		},
		{
			name: "free disk at the minimum",
			setup: func(e *testEnv, ids map[string]string) {
				e.setSettings(func(s *store.Settings) { s.MinFreeDiskPercent = 60 }) // the servers have 60% free
			},
			want: "",
		},
		{
			name: "too large for all servers",
			pool: func(p *store.Pool, _ map[string]string) { p.VCPU = 32 },
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			ids := map[string]string{}
			names := map[string]string{}
			for _, s := range []struct {
				name string
				cap  sandbox.Capacity
			}{
				{"arm-huge", capacity("arm64", 16, 131072)},
				{"x64-small", capacity("x64", 16, 16384)},
				{"x64-large", capacity("x64", 16, 32768)},
			} {
				id, _ := e.addServer(s.name, s.cap)
				ids[s.name], names[id] = id, s.name
			}
			if tt.setup != nil {
				tt.setup(e, ids)
			}
			pool := e.addPool("runplane", func(p *store.Pool) {
				if tt.pool != nil {
					tt.pool(p, ids)
				}
			})
			st, err := e.st.GetSettings(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			p, ok := e.s.place(e.ctx, pool, st)
			got := ""
			if ok {
				got = names[p.host.ServerID]
			}
			if got != tt.want {
				t.Errorf("placed on %q, want %q", got, tt.want)
			}
		})
	}
}

// Two scale sets start at the same time on a server with room for one
// sandbox. The driver counts a sandbox only after Start returns, so the
// second placement must see the reservation of the first.
func TestConcurrentStartsDoNotOverbook(t *testing.T) {
	e := newTestEnv(t)
	_, driver := e.addServer("fsn1-a", capacity("x64", 2, 4096))
	driver.gate = make(chan struct{})
	driver.entered = make(chan string, 2)
	_, scA, _ := e.attach(e.addPool("runplane-a"))
	_, scB, _ := e.attach(e.addPool("runplane-b"))

	msg := func() *scaleset.RunnerScaleSetMessage {
		return &scaleset.RunnerScaleSetMessage{MessageID: 2, Statistics: stats(1)}
	}
	doneA := make(chan error, 1)
	go func() { doneA <- scA.Scale(e.ctx, msg()) }()
	select {
	case <-driver.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first start did not begin")
	}
	doneB := make(chan error, 1)
	go func() { doneB <- scB.Scale(e.ctx, msg()) }()
	select {
	case err := <-doneB:
		if err != nil {
			t.Fatal(err)
		}
	case id := <-driver.entered:
		close(driver.gate)
		t.Fatalf("second start %s on a server with room for one", id)
	case <-time.After(5 * time.Second):
		t.Fatal("second scale set did not finish")
	}
	close(driver.gate)
	if err := <-doneA; err != nil {
		t.Fatal(err)
	}

	if n := len(driver.startedSpecs()); n != 1 {
		t.Fatalf("%d starts on a server with room for one, want 1", n)
	}
	if len(e.s.reserved) != 0 {
		t.Errorf("reservations left after the starts: %v", e.s.reserved)
	}
}
