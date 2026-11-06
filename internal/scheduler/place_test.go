package scheduler

import (
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

func TestPlacement(t *testing.T) {
	tests := []struct {
		name  string
		setup func(e *testEnv, p *store.Pool, ids map[string]string) // ids: server ID by name
		want  string                                                 // server name, "" = no server
	}{
		{"most free memory of the same arch", func(*testEnv, *store.Pool, map[string]string) {}, "x64-large"},
		{"arm64 pool", func(_ *testEnv, p *store.Pool, _ map[string]string) { p.Arch = "arm64" }, "arm-huge"},
		{"server_ids limits the servers", func(_ *testEnv, p *store.Pool, ids map[string]string) {
			p.ServerIDs = []string{ids["x64-small"]}
		}, "x64-small"},
		{"image not ready", func(e *testEnv, _ *store.Pool, ids map[string]string) { e.fleet.noImage[ids["x64-large"]] = true }, "x64-small"},
		{"server offline", func(e *testEnv, _ *store.Pool, ids map[string]string) { e.fleet.disconnected[ids["x64-large"]] = true }, "x64-small"},
		{"server draining", func(e *testEnv, _ *store.Pool, ids map[string]string) { e.fleet.notOnline[ids["x64-large"]] = true }, "x64-small"},
		{"free disk at the minimum", func(e *testEnv, _ *store.Pool, _ map[string]string) {
			e.setSettings(func(s *store.Settings) { s.MinFreeDiskPercent = 60 }) // the servers have 60% free
		}, ""},
		{"too large for all servers", func(_ *testEnv, p *store.Pool, _ map[string]string) { p.VCPU = 32 }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			byName, drivers := map[string]string{}, map[string]*fakeDriver{}
			for _, s := range []struct {
				name string
				cap  sandbox.Capacity
			}{
				{"arm-huge", capacity("arm64", 16, 131072)},
				{"x64-small", capacity("x64", 16, 16384)},
				{"x64-large", capacity("x64", 16, 32768)},
			} {
				byName[s.name], drivers[s.name] = e.addServer(s.name, s.cap)
			}
			pool := e.addPool("runplane", func(p *store.Pool) {
				p.MinIdle = 1
				tt.setup(e, p, byName)
			})
			_, sc, _ := e.attach(pool)
			e.scale(sc, msg(0))
			got := ""
			for name, d := range drivers {
				if len(d.specs) > 0 {
					got = name
				}
			}
			if got != tt.want {
				t.Errorf("started on %q, want %q", got, tt.want)
			}
		})
	}
}

// Two scale sets start at the same time on a server with room for one
// sandbox. The driver counts a sandbox only after Start returns, so the
// second placement must count the first by its reservation: its vCPUs, its
// memory with the overhead of the driver, and the sandbox itself. The
// reservation ends with the start.
func TestConcurrentStartsDoNotOverbook(t *testing.T) {
	tests := []struct {
		name string
		cap  sandbox.Capacity // room for one sandbox of 2 vCPU and 4096 MB, not two
	}{
		{"vCPUs", capacity("x64", 2, 65536)},
		{"memory with the overhead", limited(capacity("x64", 64, 2*4096+64), sandbox.Limits{OverheadMB: 64})},
		{"sandbox limit", limited(capacity("x64", 64, 65536), sandbox.Limits{MaxSandboxes: 1})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			serverID, driver := e.addServer("fsn1-a", tt.cap)
			driver.gate = make(chan struct{})
			driver.entered = make(chan string, 2)
			_, scA, _ := e.attach(e.addPool("runplane-a"))
			_, scB, _ := e.attach(e.addPool("runplane-b"))

			doneA := make(chan error, 1)
			go func() { doneA <- scA.Scale(e.ctx, msg(1)) }()
			var a string
			select {
			case a = <-driver.entered:
			case <-time.After(timeout):
				t.Fatal("first start did not begin")
			}
			doneB := make(chan error, 1)
			go func() { doneB <- scB.Scale(e.ctx, msg(1)) }()
			select {
			case err := <-doneB:
				if err != nil {
					t.Fatal(err)
				}
			case id := <-driver.entered:
				close(driver.gate)
				t.Fatalf("second start %s on a server with room for one", id)
			case <-time.After(timeout):
				t.Fatal("second scale set did not finish")
			}
			close(driver.gate)
			if err := <-doneA; err != nil {
				t.Fatal(err)
			}
			if n := len(driver.specs); n != 1 {
				t.Fatalf("%d starts on a server with room for one, want 1", n)
			}

			// When the first sandbox goes away, the second scale set gets its room.
			if err := driver.Stop(e.ctx, a); err != nil {
				t.Fatal(err)
			}
			e.event(serverID, a, stateEvent(sandbox.StateGone, "exited"))
			e.scale(scB, msg(1))
			if n := len(driver.specs); n != 2 {
				t.Errorf("%d starts after the first sandbox went away, want 2", n)
			}
		})
	}
}

func limited(c sandbox.Capacity, l sandbox.Limits) sandbox.Capacity {
	c.Limits = l
	return c
}
