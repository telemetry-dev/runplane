package fleet

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

func TestAgentUnreachableAfterOneMinute(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	a, id := h.connectAgent("build-1", newFakeNode("x64"))
	servers := events.Subscribe[events.ServerState](ctx, h.bus)

	a.stop()
	waitFor(t, "the end of the session", func() bool { return h.f.Driver(id) == nil })
	h.clock.add(59 * time.Second)
	h.f.syncServers(ctx)
	if got := h.server(id).Status; got != store.ServerOnline {
		t.Fatalf("status %q 59 seconds after the disconnect, want online", got)
	}

	h.clock.add(2 * time.Second)
	h.f.syncServers(ctx)
	if got := h.server(id).Status; got != store.ServerUnreachable {
		t.Fatalf("status %q 61 seconds after the disconnect, want unreachable", got)
	}
	waitEvent(t, servers, func(v events.ServerState) bool {
		return v.ID == id && v.Status == store.ServerUnreachable
	})

	a.start()
	waitFor(t, "the agent to connect again", func() bool { return h.f.Driver(id) != nil })
	if got := h.server(id).Status; got != store.ServerOnline {
		t.Errorf("status %q after the agent connected again, want online", got)
	}
}

func TestOnlineServers(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	_, agentID := h.connectAgent("build-x64", newFakeNode("x64", "sb-on-agent"))
	localID := h.localID()

	arches := map[string]string{}
	for _, s := range h.f.Online() {
		arches[s.ServerID] = s.Arch
		if s.ServerID != agentID {
			continue
		}
		// The host of the agent operates the sandboxes on the agent.
		list, err := s.Driver.List(ctx)
		if err != nil || len(list) != 1 || list[0].ID != "sb-on-agent" {
			t.Errorf("sandboxes of the agent host: %+v, %v", list, err)
		}
	}
	if want := map[string]string{localID: "arm64", agentID: "x64"}; !maps.Equal(arches, want) {
		t.Fatalf("online servers and arches %v, want %v", arches, want)
	}

	// An admin drains the agent.
	setStatus := func(status string) {
		if _, err := h.f.UpdateServer(ctx, audit.Actor{}, agentID, ServerChange{Status: &status}); err != nil {
			t.Fatal(err)
		}
	}
	before := h.capacity.Load()
	setStatus(store.ServerDraining)
	waitFor(t, "the drain", func() bool { return len(h.f.Online()) == 1 })
	if got := h.f.Online()[0].ServerID; got != localID {
		t.Errorf("online server %s after the drain, want the local server %s", got, localID)
	}
	if h.f.Driver(agentID) == nil {
		t.Error("a draining server has no driver, so its sandboxes cannot stop")
	}
	if h.capacity.Load() == before {
		t.Error("no CapacityChanged call after the drain")
	}
	setStatus(store.ServerOnline)
	waitFor(t, "the agent online again", func() bool { return len(h.f.Online()) == 2 })
}

// An agent that connects again before the unreachable check writes its
// status stays online.
func TestConnectedAgentStaysOnline(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	_, id := h.connectAgent("build-x64", newFakeNode("x64"))

	if err := h.st.SetServerStatus(ctx, id, store.ServerUnreachable, "No connection to the agent.", h.clock.now()); err != nil {
		t.Fatal(err)
	}
	h.bus.Publish(events.ServerState(h.server(id)))
	h.f.syncServers(ctx)
	if got := h.server(id).Status; got != store.ServerOnline {
		t.Errorf("status %q of a connected agent, want online", got)
	}
	waitFor(t, "the agent in Online", func() bool {
		return slices.ContainsFunc(h.f.Online(), func(s Host) bool { return s.ServerID == id })
	})
}

func TestOldSessionEndKeepsNewSession(t *testing.T) {
	h := newHarness(t, newFakeNode("arm64"))
	t.Cleanup(h.f.stop)
	old := &node{Node: newFakeNode("x64"), id: "01srv", status: store.ServerOnline}
	cur := &node{Node: newFakeNode("x64"), id: "01srv", status: store.ServerOnline}
	h.f.add(old)
	h.f.add(cur)

	// The agent connected again before the end of its old session arrived.
	h.f.down(t.Context(), old)
	if h.f.Driver("01srv") != cur.Driver() {
		t.Fatal("the end of the old session removed the new session")
	}
	h.f.down(t.Context(), cur)
	if h.f.Driver("01srv") != nil {
		t.Error("the end of the current session kept the server connected")
	}
}

// A connected agent records its region apart from its provider, runs the
// doctor and keeps the result, adds its registry mirror to the one of the
// primary in the stats of GET /api/caches, and gives the limits of its
// driver for the placement of the pools.
func TestConnectedAgents(t *testing.T) {
	ctx := t.Context()
	local := newFakeNode("arm64")
	local.info.Region = "fsn1"
	local.mirror = agent.MirrorStats{Hits: 2, Misses: 1, HitBytes: 20, MissBytes: 10, SizeBytes: 30}
	h := newHarness(t, local)
	h.run()
	a, b := newFakeNode("arm64"), newFakeNode("x64")
	a.info.Region, a.info.Kernel = "nbg1", "6.1.0-agent"
	a.mirror = agent.MirrorStats{Hits: 5, Misses: 2, HitBytes: 500, MissBytes: 200, SizeBytes: 900}
	a.limits = sandbox.Limits{OverheadMB: 64, MaxVCPU: 32, MaxSandboxes: 100}
	b.mirror = agent.MirrorStats{Hits: 1, Misses: 3, HitBytes: 100, MissBytes: 300, SizeBytes: 400}
	agentA, idA := h.connectAgent("arm-1", a)
	h.connectAgent("amd-1", b)

	regions, err := h.st.ServerRegions(ctx)
	if err != nil || regions[h.localID()] != "fsn1" || regions[idA] != "nbg1" {
		t.Errorf("regions %v, %v; want fsn1 for the primary and nbg1 for the agent", regions, err)
	}
	if v := h.server(idA); v.Provider != "Hetzner" {
		t.Errorf("provider %q, want Hetzner without the region", v.Provider)
	}
	want := agent.MirrorStats{Hits: 8, Misses: 6, HitBytes: 620, MissBytes: 510, SizeBytes: 1330}
	if got := h.f.MirrorStats(ctx); got != want {
		t.Errorf("mirror stats %+v, want %+v", got, want)
	}
	if got, ok := h.f.Limits(idA); !ok || got != a.limits {
		t.Errorf("limits of the agent %+v, %v; want %+v", got, ok, a.limits)
	}
	ran, err := h.f.Doctor(ctx, idA)
	if err != nil || len(ran.Checks) != 1 || ran.Checks[0].Detail != "6.1.0-agent" || ran.Checks[0].Status != "ok" {
		t.Errorf("doctor of the agent: %+v, %v", ran, err)
	}
	// The server page shows the last doctor result of each server.
	if kept, err := h.st.GetDoctorResult(ctx, idA); err != nil || !kept.CheckedAt.Equal(ran.CheckedAt) || !reflect.DeepEqual(kept.Checks, ran.Checks) {
		t.Errorf("kept doctor result of the agent: %+v, %v; want %+v", kept, err, ran)
	}
	if _, err := h.f.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if kept, err := h.st.GetDoctorResult(ctx, h.localID()); err != nil || len(kept.Checks) != 1 || kept.Checks[0].Detail != local.info.Kernel {
		t.Errorf("kept doctor result of the primary after the setup check: %+v, %v", kept, err)
	}
	agentA.stop()
	waitFor(t, "the end of the session", func() bool { return h.f.Driver(idA) == nil })
	if _, err := h.f.Doctor(ctx, idA); !errors.Is(err, ErrNotConnected) {
		t.Errorf("doctor of a disconnected server: %v, want ErrNotConnected", err)
	}
	if _, ok := h.f.Limits(idA); ok {
		t.Error("a disconnected server has limits")
	}
}

func TestRemoveServer(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	node := newFakeNode("x64", "sb1", "sb2")
	a, id := h.connectAgent("old", node)
	// An unused token of the server, as an install over SSH leaves it.
	leftover, _, err := h.f.newToken(ctx, "old", "", id)
	if err != nil {
		t.Fatal(err)
	}

	if err := h.f.RemoveServer(ctx, audit.Actor{}, h.localID()); !errors.Is(err, ErrLocalServer) {
		t.Errorf("RemoveServer of the local server: %v, want ErrLocalServer", err)
	}
	if err := h.f.RemoveServer(ctx, audit.Actor{}, id); err != nil {
		t.Fatal(err)
	}
	if got := node.args("Stop"); !reflect.DeepEqual(got, []any{"sb1", "sb2"}) {
		t.Errorf("stopped sandboxes %v, want sb1 and sb2", got)
	}
	waitFor(t, "the end of the session", func() bool { return h.f.Driver(id) == nil })
	if _, err := h.st.GetServer(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the row of the removed server: %v, want ErrNotFound", err)
	}

	file := a.file()
	if _, _, err := h.f.Authenticate(ctx, agent.Hello{ServerID: file["server_id"], Secret: file["secret"], Addr: "127.0.0.1"}); err == nil {
		t.Error("the removed agent can connect with its secret")
	}
	if _, _, err := h.f.Authenticate(ctx, agent.Hello{Token: leftover, Addr: "127.0.0.1"}); err == nil {
		t.Error("the unused token of the removed server registers a server")
	}
}

// The Activity sentences need the old and new name and status of a server.
// An installing server keeps its status, and only a drained or disabled
// server goes online.
func TestUpdateServer(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	_, id := h.connectAgent("fsn1-a", newFakeNode("x64"))
	now := h.clock.now()
	if err := h.st.CreateServer(ctx, store.Server{
		ID: "new", Name: "new", Kind: store.ServerAgent, Status: store.ServerInstalling, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	status := func(s string) ServerChange { return ServerChange{Status: &s} }
	by := audit.Actor{UserID: "u1", IP: "192.0.2.7"}

	if _, err := h.f.UpdateServer(ctx, by, "new", status(store.ServerDraining)); !errors.Is(err, ErrInstalling) {
		t.Errorf("drain of an installing server: %v, want ErrInstalling", err)
	}

	change := status(store.ServerDraining)
	name := "fsn1-c"
	change.Name = &name
	v, err := h.f.UpdateServer(ctx, by, id, change)
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "fsn1-c" || v.Status != store.ServerDraining {
		t.Errorf("server after the change: %s %s, want fsn1-c draining", v.Name, v.Status)
	}
	entries, _, err := h.st.ListAudit(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	// The first start and the agent install also record entries.
	entries = slices.DeleteFunc(entries, func(e store.AuditEntry) bool { return e.Action != "server.update" })
	want := `{"changes":{"name":["fsn1-a","fsn1-c"],"status":["online","draining"]},"name":"fsn1-c","status":"draining"}`
	if len(entries) != 1 || entries[0].Action != "server.update" || string(entries[0].Detail) != want ||
		entries[0].UserID != "u1" || entries[0].IP != "192.0.2.7" {
		t.Errorf("audit entries %+v, want one server.update by u1 with detail %s", entries, want)
	}

	if _, err := h.f.UpdateServer(ctx, by, "new", status(store.ServerOnline)); !errors.Is(err, ErrInstalling) {
		t.Errorf("online for an installing server: %v, want ErrInstalling", err)
	}
	if _, err := h.f.UpdateServer(ctx, by, h.localID(), status(store.ServerOnline)); err != nil {
		t.Errorf("online to online: %v, want no change", err)
	}
	if _, err := h.f.UpdateServer(ctx, by, id, status(store.ServerOnline)); err != nil {
		t.Errorf("resume of a draining server: %v", err)
	}
	gone := "gone"
	if err := h.st.CreateServer(ctx, store.Server{
		ID: gone, Name: gone, Kind: store.ServerAgent, Status: store.ServerUnreachable, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.f.UpdateServer(ctx, by, gone, status(store.ServerOnline)); !errors.Is(err, ErrNotPaused) {
		t.Errorf("online for an unreachable server: %v, want ErrNotPaused", err)
	}
}

func TestServerActions(t *testing.T) {
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	_, id := h.connectAgent("fsn1-a", newFakeNode("x64"))
	for _, tc := range []struct {
		v    store.Server
		want Actions
	}{
		{h.server(h.localID()), Actions{Drain: true, RunDoctor: true}},
		{h.server(id), Actions{Drain: true, Remove: true, RunDoctor: true}},
		{store.Server{ID: "gone", Kind: store.ServerAgent, Status: store.ServerUnreachable}, Actions{Drain: true, Remove: true}},
		{store.Server{ID: "new", Kind: store.ServerAgent, Status: store.ServerInstalling}, Actions{Remove: true}},
		{store.Server{ID: "rest", Kind: store.ServerAgent, Status: store.ServerDraining}, Actions{Resume: true, Remove: true}},
		{store.Server{ID: "off", Kind: store.ServerAgent, Status: store.ServerDisabled}, Actions{Drain: true, Resume: true, Remove: true}},
	} {
		if got := h.f.Actions(tc.v); got != tc.want {
			t.Errorf("actions of %s %s: %+v, want %+v", tc.v.ID, tc.v.Status, got, tc.want)
		}
	}
}

func TestSettingsReachServers(t *testing.T) {
	ctx := t.Context()
	local := newFakeNode("arm64")
	h := newHarness(t, local)
	h.run()
	agentNode := newFakeNode("x64")
	h.connectAgent("build-x64", agentNode)

	waitFor(t, "the settings of the new agent", func() bool { return len(agentNode.args("Configure")) == 1 })
	defaults := agent.Settings{
		Settings: sandbox.Settings{
			DNS: []string{"1.1.1.1", "8.8.8.8"}, EgressAllowlist: []string{}, RegistryMirror: true, ActionsCache: true,
		},
		MinFreeDiskPercent: 15,
	}
	if got := agentNode.args("Configure")[0]; !reflect.DeepEqual(got, defaults) {
		t.Errorf("settings at connect:\n%+v\nwant\n%+v", got, defaults)
	}

	prev := store.DefaultSettings()
	next := prev
	next.ReserveVCPU, next.ReserveMemoryMB, next.CPUOvercommit = 2, 4096, 1.5
	next.MinFreeDiskPercent = 12.5
	next.DNSServers, next.EgressAllowlist = []string{"9.9.9.9"}, []string{"10.0.0.0/8"}
	next.BandwidthMbps, next.RegistryMirror, next.ActionsCache = 100, false, false
	// As the settings service does: it saves the settings, then publishes the change.
	if _, err := h.st.UpdateSettings(ctx, func(s *store.Settings) error {
		*s = next
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.bus.Publish(events.SettingsChanged{Old: prev, New: next})
	want := agent.Settings{
		Settings:           sandbox.Settings{DNS: []string{"9.9.9.9"}, EgressAllowlist: []string{"10.0.0.0/8"}, BandwidthMbps: 100},
		MinFreeDiskPercent: 12.5,
	}
	for name, n := range map[string]*fakeNode{"local": local, "agent": agentNode} {
		waitFor(t, "the new settings on the "+name+" server", func() bool {
			list := n.args("Configure")
			return len(list) > 0 && reflect.DeepEqual(list[len(list)-1], want)
		})
	}
}

func TestHostStats(t *testing.T) {
	ctx := t.Context()
	local := newFakeNode("arm64")
	h := newHarness(t, local)
	h.run()
	stats := events.Subscribe[events.HostStats](ctx, h.bus)
	id := h.localID()
	at := h.clock.now().Add(-time.Second)
	sample := func(diskPercent float64) {
		local.stats <- host.Stats{At: at, CPUPercent: 52, MemoryPercent: 38, DiskPercent: diskPercent, Resources: host.Resources{DiskBytes: 500e9}}
		at = at.Add(10 * time.Second)
		waitEvent(t, stats, func(st events.HostStats) bool {
			return st.ServerID == id && st.DiskPercent == diskPercent
		})
	}

	sample(90)
	h.f.flushStats(ctx)
	latest, err := h.st.LatestServerStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st := latest[id]; st.CPUPercent != 52 || st.MemoryPercent != 38 || st.DiskPercent != 90 {
		t.Errorf("stored sample %+v, want the one sample", st)
	}
}

// release makes the primary and its agents run the release v1.4.2.
func release(c *Config) { c.Version = "v1.4.2" }

// An agent must speak the protocol of the primary, so the fleet updates an
// agent with another version when it connects. It tries each version one
// time for each agent and shows a failure on the server.
func TestUpdateAgentOnConnect(t *testing.T) {
	h := newHarness(t, newFakeNode("arm64"), release)
	h.run()
	agentNode := newFakeNode("arm64")
	agentNode.updateErr = errors.New("download failed")
	_, id := h.connectAgent("arm-2", agentNode)

	tests := []struct {
		name, id, agent string
		want            []any
	}{
		{"same release", "a1", "v1.4.2", nil},
		{"newer release", "a2", "v1.5.0", []any{"v1.4.2"}},
		{"development build", "a3", "dev", []any{"v1.4.2"}},
		{"older release", id, "v1.4.1", []any{"v1.4.2"}},
		{"older release again", id, "v1.4.1", nil},
	}
	for _, tt := range tests {
		before := len(agentNode.args("Update"))
		n := &node{Node: agentNode, id: tt.id, version: tt.agent}
		if h.f.beginUpdate(t.Context(), n.id, n.version, false) == nil {
			h.f.runUpdate(t.Context(), n)
		}
		if got := agentNode.args("Update")[before:]; !slices.Equal(got, tt.want) {
			t.Errorf("%s: updates %v, want %v", tt.name, got, tt.want)
		}
	}
	if v := h.server(id); !strings.Contains(v.UpdateError, "The update to runplane v1.4.2 failed: download failed.") {
		t.Errorf("update error %q, want the failed update", v.UpdateError)
	}
}

// An admin updates an agent with POST /api/servers/{id}/update. The update
// runs until the agent connects again with the new version.
func TestUpdateAgentByHand(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"), release)
	h.run()
	agentNode := newFakeNode("arm64")
	a, id := h.connectAgent("arm-2", agentNode)
	state := func() UpdateState { return h.f.UpdateState(h.server(id)) }
	// olderAgent makes the connected agent look like an agent of v1.4.1.
	olderAgent := func() {
		h.f.mu.Lock()
		h.f.nodes[id].version = "v1.4.1"
		h.f.mu.Unlock()
		v := h.server(id)
		v.ServerFacts.Version = "v1.4.1"
		if err := h.st.UpdateServerFacts(ctx, id, v.ServerFacts, h.clock.now()); err != nil {
			t.Fatal(err)
		}
	}

	if got := state(); got != UpdateCurrent {
		t.Fatalf("update state %q of an agent of the same version, want current", got)
	}
	if err := h.f.UpdateAgent(ctx, audit.Actor{}, id); err == nil {
		t.Fatal("UpdateAgent started an update of an agent of the same version")
	}
	if got := h.f.UpdateState(h.server(h.localID())); got != UpdateCurrent {
		t.Errorf("update state %q of the primary, want current", got)
	}

	olderAgent()
	if got := state(); got != UpdateAvailable {
		t.Fatalf("update state %q of an older agent, want available", got)
	}
	agentNode.set(func(n *fakeNode) { n.updateErr = errors.New("download failed") })
	if err := h.f.UpdateAgent(ctx, audit.Actor{}, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failed update", func() bool { return state() == UpdateFailed })
	// An admin can try a failed version again.
	agentNode.set(func(n *fakeNode) { n.updateErr = nil })
	if err := h.f.UpdateAgent(ctx, audit.Actor{}, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second update call", func() bool { return len(agentNode.args("Update")) == 2 })
	if got := state(); got != UpdateUpdating {
		t.Fatalf("update state %q before the agent restarts, want updating", got)
	}
	entries, _, err := h.st.ListAudit(ctx, "", 10)
	if err != nil || len(entries) == 0 || entries[0].Action != "server.update_agent" || entries[0].Target != id ||
		string(entries[0].Detail) != `{"from":"v1.4.1","name":"arm-2","to":"v1.4.2"}` {
		t.Errorf("audit entries %+v, %v; want the server.update_agent of arm-2 from v1.4.1 to v1.4.2 first", entries, err)
	}
	if err := h.f.UpdateAgent(ctx, audit.Actor{}, id); err == nil {
		t.Fatal("UpdateAgent started a second update while one runs")
	}

	// The agent restarts with the release of the primary.
	a.stop()
	waitFor(t, "the end of the session", func() bool { return h.f.Driver(id) == nil })
	a.start()
	waitFor(t, "the end of the update", func() bool { return h.f.Driver(id) != nil && state() == UpdateCurrent })

	// An agent that does not come back fails the update.
	olderAgent()
	if err := h.f.UpdateAgent(ctx, audit.Actor{}, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the third update call", func() bool { return len(agentNode.args("Update")) == 3 })
	a.stop()
	waitFor(t, "the end of the session", func() bool { return h.f.Driver(id) == nil })
	h.clock.add(61 * time.Second)
	h.f.syncServers(ctx)
	if got := state(); got != UpdateFailed {
		t.Fatalf("update state %q of an agent that did not come back, want failed", got)
	}
	if v := h.server(id); v.Status != store.ServerUnreachable || !strings.Contains(v.UpdateError, "did not connect again") {
		t.Errorf("status %q, update error %q; want unreachable after the update", v.Status, v.UpdateError)
	}
}
