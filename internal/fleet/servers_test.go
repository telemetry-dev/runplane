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
	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
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
	if e := recv(t, h.notes); e.Type != notify.ServerUnreachable || e.Data["server_id"] != id || len(h.notes) != 0 {
		t.Errorf("notification %+v and %d more, want one server.unreachable for %s", e, len(h.notes), id)
	}

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

	// An admin drains the agent, as PATCH /api/servers/{id} does.
	setStatus := func(status string) {
		if err := h.st.SetServerStatus(ctx, agentID, status, ""); err != nil {
			t.Fatal(err)
		}
		h.bus.Publish(events.ServerState(h.server(agentID)))
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

	if err := h.st.SetServerStatus(ctx, id, store.ServerUnreachable, "No connection to the agent."); err != nil {
		t.Fatal(err)
	}
	h.bus.Publish(events.ServerState(h.server(id)))
	h.f.syncServers(ctx)
	if got := h.server(id).Status; got != store.ServerOnline {
		t.Errorf("status %q of a connected agent, want online", got)
	}
	waitFor(t, "the agent in Online", func() bool {
		return slices.ContainsFunc(h.f.Online(), func(s scheduler.Host) bool { return s.ServerID == id })
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
// doctor, and adds its registry mirror to the stats of GET /api/caches.
func TestConnectedAgents(t *testing.T) {
	ctx := t.Context()
	local := newFakeNode("arm64")
	local.info.Region = "fsn1"
	h := newHarness(t, local)
	h.run()
	a, b := newFakeNode("arm64"), newFakeNode("x64")
	a.info.Region, a.info.Kernel = "nbg1", "6.1.0-agent"
	a.mirror = mirror.Stats{Hits: 5, Misses: 2, HitBytes: 500, MissBytes: 200, SizeBytes: 900}
	b.mirror = mirror.Stats{Hits: 1, Misses: 3, HitBytes: 100, MissBytes: 300, SizeBytes: 400}
	agentA, idA := h.connectAgent("arm-1", a)
	h.connectAgent("amd-1", b)

	regions, err := h.st.ServerRegions(ctx)
	if err != nil || regions[h.localID()] != "fsn1" || regions[idA] != "nbg1" {
		t.Errorf("regions %v, %v; want fsn1 for the primary and nbg1 for the agent", regions, err)
	}
	if v := h.server(idA); v.Provider != "Hetzner" {
		t.Errorf("provider %q, want Hetzner without the region", v.Provider)
	}
	want := mirror.Stats{Hits: 6, Misses: 5, HitBytes: 600, MissBytes: 500, SizeBytes: 1300}
	if got := h.f.AgentMirrorStats(ctx); got != want {
		t.Errorf("agent mirror stats %+v, want %+v", got, want)
	}
	checks, err := h.f.Doctor(ctx, idA)
	if err != nil || len(checks) != 1 || checks[0].Detail != "6.1.0-agent" || checks[0].Status != "ok" {
		t.Errorf("doctor of the agent: %+v, %v", checks, err)
	}
	agentA.stop()
	waitFor(t, "the end of the session", func() bool { return h.f.Driver(idA) == nil })
	if _, err := h.f.Doctor(ctx, idA); err == nil {
		t.Error("doctor of a disconnected server: no error")
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

	if err := h.f.Remove(ctx, h.localID()); err == nil {
		t.Error("Remove of the local server: no error")
	}
	if err := h.f.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := node.args("Stop"); !reflect.DeepEqual(got, []any{"sb1", "sb2"}) {
		t.Errorf("stopped sandboxes %v, want sb1 and sb2", got)
	}
	waitFor(t, "the end of the session", func() bool { return h.f.Driver(id) == nil })
	if err := h.st.DeleteServer(ctx, id); err != nil { // as the API does
		t.Fatal(err)
	}

	file := a.file()
	if _, _, err := h.f.Authenticate(ctx, agent.Hello{ServerID: file["server_id"], Secret: file["secret"], Addr: "127.0.0.1"}); err == nil {
		t.Error("the removed agent can connect with its secret")
	}
	if _, _, err := h.f.Authenticate(ctx, agent.Hello{Token: leftover, Addr: "127.0.0.1"}); err == nil {
		t.Error("the unused token of the removed server registers a server")
	}
}

func TestSettingsReachServers(t *testing.T) {
	ctx := t.Context()
	local := newFakeNode("arm64")
	h := newHarness(t, local)
	h.run()
	agentNode := newFakeNode("x64")
	_, agentID := h.connectAgent("build-x64", agentNode)

	waitFor(t, "the settings of the new agent", func() bool { return len(agentNode.args("Configure")) == 1 })
	defaults := sandbox.Settings{
		DNS: []string{"1.1.1.1", "8.8.8.8"}, EgressAllowlist: []string{}, RegistryMirror: true, ActionsCache: true,
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
	h.f.SettingsChanged(ctx, prev, next)
	want := sandbox.Settings{DNS: []string{"9.9.9.9"}, EgressAllowlist: []string{"10.0.0.0/8"}, BandwidthMbps: 100}
	for name, n := range map[string]*fakeNode{"local": local, "agent": agentNode} {
		waitFor(t, "the new settings on the "+name+" server", func() bool {
			list := n.args("Configure")
			return len(list) > 0 && reflect.DeepEqual(list[len(list)-1], want)
		})
	}
	// The doctor of each server gets the min_free_disk_percent setting.
	if _, err := h.f.Doctor(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	if got := agentNode.args("Doctor"); !reflect.DeepEqual(got, []any{12.5}) {
		t.Errorf("doctor of the agent got the minimum free disk %v, want 12.5", got)
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
	// With a domain and no public URL, the links go to the domain.
	if _, err := h.st.UpdateSettings(ctx, func(s *store.Settings) error {
		s.Domain = "ci.example.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
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

	// 10% free is below the 15% minimum. A second low sample sends nothing;
	// after a recovery, the next drop sends again.
	sample(91)
	sample(50)
	sample(95)
	var free []float64
	for range 2 {
		e := recv(t, h.notes)
		if e.Type != notify.DiskLow || e.Data["server_id"] != id || e.URL != "https://ci.example.com/servers/"+id {
			t.Errorf("notification %+v, want disk.low for %s with a link to the domain", e, id)
		}
		free = append(free, e.Data["free_percent"].(float64))
	}
	slices.Sort(free)
	if !slices.Equal(free, []float64{5, 10}) || len(h.notes) != 0 {
		t.Errorf("disk.low notifications for free space %v and %d more, want 10%% and 5%%", free, len(h.notes))
	}
}

// setRelease sets the release repository and the version of runplane until
// the test ends.
func setRelease(t *testing.T, repo, ver string) {
	oldRepo, oldVer := version.Repo, version.Version
	t.Cleanup(func() { version.Repo, version.Version = oldRepo, oldVer })
	version.Repo, version.Version = repo, ver
}

// An agent must speak the protocol of the primary, so the fleet updates an
// agent with another version when it connects. It tries each version one
// time for each agent and shows a failure on the server.
func TestUpdateAgentOnConnect(t *testing.T) {
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	setRelease(t, version.Repo, "v1.4.2")
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
		if h.f.beginUpdate(n.id, n.version, false) {
			h.f.runUpdate(t.Context(), n)
		}
		if got := agentNode.args("Update")[before:]; !slices.Equal(got, tt.want) {
			t.Errorf("%s: updates %v, want %v", tt.name, got, tt.want)
		}
	}
	if v := h.server(id); !strings.Contains(v.StatusDetail, "The update to runplane v1.4.2 failed: download failed.") {
		t.Errorf("status detail %q, want the failed update", v.StatusDetail)
	}
}

// An admin updates an agent with POST /api/servers/{id}/update. The update
// runs until the agent connects again with the new version.
func TestUpdateAgentByHand(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	setRelease(t, version.Repo, "v1.4.2")
	agentNode := newFakeNode("arm64")
	a, id := h.connectAgent("arm-2", agentNode)
	state := func() string { return h.f.UpdateState(h.server(id)) }
	// olderAgent makes the connected agent look like an agent of v1.4.1.
	olderAgent := func() {
		h.f.mu.Lock()
		h.f.nodes[id].version = "v1.4.1"
		h.f.mu.Unlock()
		v := h.server(id)
		v.ServerFacts.Version = "v1.4.1"
		if err := h.st.UpdateServerFacts(ctx, id, v.ServerFacts); err != nil {
			t.Fatal(err)
		}
	}

	if got := state(); got != api.UpdateCurrent {
		t.Fatalf("update state %q of an agent of the same version, want current", got)
	}
	if err := h.f.UpdateAgent(ctx, id); err == nil {
		t.Fatal("UpdateAgent started an update of an agent of the same version")
	}
	if got := h.f.UpdateState(h.server(h.localID())); got != api.UpdateCurrent {
		t.Errorf("update state %q of the primary, want current", got)
	}

	olderAgent()
	if got := state(); got != api.UpdateAvailable {
		t.Fatalf("update state %q of an older agent, want available", got)
	}
	agentNode.set(func(n *fakeNode) { n.updateErr = errors.New("download failed") })
	if err := h.f.UpdateAgent(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failed update", func() bool { return state() == api.UpdateFailed })
	// An admin can try a failed version again.
	agentNode.set(func(n *fakeNode) { n.updateErr = nil })
	if err := h.f.UpdateAgent(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second update call", func() bool { return len(agentNode.args("Update")) == 2 })
	if got := state(); got != api.UpdateUpdating {
		t.Fatalf("update state %q before the agent restarts, want updating", got)
	}
	if err := h.f.UpdateAgent(ctx, id); err == nil {
		t.Fatal("UpdateAgent started a second update while one runs")
	}

	// The agent restarts with the release of the primary.
	a.stop()
	waitFor(t, "the end of the session", func() bool { return h.f.Driver(id) == nil })
	a.start()
	waitFor(t, "the end of the update", func() bool { return h.f.Driver(id) != nil && state() == api.UpdateCurrent })

	// An agent that does not come back fails the update.
	olderAgent()
	if err := h.f.UpdateAgent(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the third update call", func() bool { return len(agentNode.args("Update")) == 3 })
	a.stop()
	waitFor(t, "the end of the session", func() bool { return h.f.Driver(id) == nil })
	h.clock.add(61 * time.Second)
	h.f.syncServers(ctx)
	if got := state(); got != api.UpdateFailed {
		t.Fatalf("update state %q of an agent that did not come back, want failed", got)
	}
	if v := h.server(id); v.Status != store.ServerUnreachable || !strings.Contains(v.StatusDetail, "did not connect again") {
		t.Errorf("status %q, detail %q; want unreachable after the update", v.Status, v.StatusDetail)
	}
}
