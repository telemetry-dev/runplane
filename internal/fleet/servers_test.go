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
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

func TestAgentUnreachableAfterOneMinute(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	a, id := h.connectAgent("build-1", newFakeNode("x64"))
	seen := watch(t, h.bus)

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
	waitFor(t, "a server.state event with the status unreachable", func() bool {
		return slices.ContainsFunc(seen(), func(e events.Event) bool {
			v, ok := e.Data.(store.Server)
			return e.Type == events.ServerState && ok && v.ID == id && v.Status == store.ServerUnreachable
		})
	})
	waitFor(t, "the notification", func() bool { return len(h.notes.events()) > 0 })
	if got := h.notes.events(); len(got) != 1 || got[0].Type != notify.ServerUnreachable || got[0].Data["server_id"] != id {
		t.Errorf("notifications %+v, want one server.unreachable for %s", got, id)
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
	agentNode := newFakeNode("x64")
	agentNode.driver.Start(ctx, sandbox.Spec{ID: "sb-on-agent"})
	_, agentID := h.connectAgent("build-x64", agentNode)
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
		h.bus.Publish(events.Event{Type: events.ServerState, Data: h.server(agentID)})
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

func TestFirstStartHasDefaultImage(t *testing.T) {
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	waitFor(t, "the default image", func() bool {
		ref, ok := h.f.Image(h.localID(), "")
		return ok && ref.Ref == "builtin:ubuntu-24.04" && ref.Digest == "sha256:b0b0"
	})
}

func TestRemoveServer(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	node := newFakeNode("x64")
	node.driver.Start(ctx, sandbox.Spec{ID: "sb1"})
	node.driver.Start(ctx, sandbox.Spec{ID: "sb2"})
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
	if got := node.driver.stoppedIDs(); !slices.Equal(got, []string{"sb1", "sb2"}) {
		t.Errorf("stopped sandboxes %v, want sb1 and sb2", got)
	}
	waitFor(t, "the end of the session", func() bool { return h.f.Driver(id) == nil })
	if err := h.st.DeleteServer(ctx, id); err != nil { // as the API does
		t.Fatal(err)
	}

	file := a.file()
	if _, _, err := h.f.Authenticate(ctx, agent.Hello{ServerID: file.ServerID, Secret: file.Secret, Addr: "127.0.0.1"}); err == nil {
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
	h.connectAgent("build-x64", agentNode)

	waitFor(t, "the settings of the new agent", func() bool { return len(agentNode.configured()) == 1 })
	defaults := sandbox.Settings{
		ReserveVCPU: 1, ReserveMemoryMB: 2048, Overcommit: 1, MinFreeDiskPercent: 15, DNS: []string{"1.1.1.1", "8.8.8.8"},
		EgressAllowlist: []string{}, RegistryMirror: true, ActionsCache: true,
	}
	if got := agentNode.configured()[0]; !reflect.DeepEqual(got, defaults) {
		t.Errorf("settings at connect:\n%+v\nwant\n%+v", got, defaults)
	}

	prev := store.DefaultSettings()
	next := prev
	next.ReserveVCPU, next.ReserveMemoryMB, next.CPUOvercommit = 2, 4096, 1.5
	next.MinFreeDiskPercent = 12.5
	next.DNSServers, next.EgressAllowlist = []string{"9.9.9.9"}, []string{"10.0.0.0/8"}
	next.BandwidthMbps, next.RegistryMirror, next.ActionsCache = 100, false, false
	h.f.SettingsChanged(ctx, prev, next)
	want := sandbox.Settings{
		ReserveVCPU: 2, ReserveMemoryMB: 4096, Overcommit: 1.5, MinFreeDiskPercent: 13, DNS: []string{"9.9.9.9"},
		EgressAllowlist: []string{"10.0.0.0/8"}, BandwidthMbps: 100,
	}
	for name, n := range map[string]*fakeNode{"local": local, "agent": agentNode} {
		waitFor(t, "the new settings on the "+name+" server", func() bool {
			list := n.configured()
			return len(list) > 0 && reflect.DeepEqual(list[len(list)-1], want)
		})
	}
}

func TestHostStats(t *testing.T) {
	ctx := t.Context()
	local := newFakeNode("arm64")
	h := newHarness(t, local)
	h.run()
	seen := watch(t, h.bus)
	id := h.localID()
	at := h.clock.now().Add(-time.Second)

	sample := func(diskPercent float64) {
		local.stats <- host.Stats{At: at, CPUPercent: 52, MemoryPercent: 38, DiskPercent: diskPercent, Resources: host.Resources{DiskBytes: 500e9}}
		at = at.Add(10 * time.Second)
	}
	statsEvents := func() int {
		n := 0
		for _, e := range seen() {
			if st, ok := e.Data.(store.ServerStat); ok && e.Type == events.HostStats && st.ServerID == id {
				n++
			}
		}
		return n
	}
	sample(90)
	waitFor(t, "the host.stats event", func() bool { return statsEvents() == 1 })
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
	waitFor(t, "four samples", func() bool { return statsEvents() == 4 })
	waitFor(t, "two notifications", func() bool { return len(h.notes.events()) >= 2 })
	var free []float64
	for _, e := range h.notes.events() {
		if e.Type != notify.DiskLow || e.Data["server_id"] != id {
			t.Errorf("notification %+v, want disk.low for %s", e, id)
		}
		free = append(free, e.Data["free_percent"].(float64))
	}
	slices.Sort(free)
	if !slices.Equal(free, []float64{5, 10}) {
		t.Errorf("disk.low notifications for free space %v, want 10%% and 5%%", free)
	}
}

func TestDoctorOnConnectedServer(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	node := newFakeNode("x64")
	node.info.Kernel = "6.1.0-agent"
	a, id := h.connectAgent("build-x64", node)

	checks, err := h.f.Doctor(ctx, id)
	if err != nil || len(checks) != 1 || checks[0].Detail != "6.1.0-agent" || checks[0].Status != "ok" {
		t.Fatalf("doctor of the agent: %+v, %v", checks, err)
	}
	a.stop()
	waitFor(t, "the end of the session", func() bool { return h.f.Driver(id) == nil })
	if _, err := h.f.Doctor(ctx, id); err == nil {
		t.Error("doctor of a disconnected server: no error")
	}
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

func TestConnectedAgentStaysOnline(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	_, id := h.connectAgent("build-x64", newFakeNode("x64"))

	// The unreachable check wrote its status just after the agent connected again.
	if err := h.st.SetServerStatus(ctx, id, store.ServerUnreachable, "No connection to the agent."); err != nil {
		t.Fatal(err)
	}
	h.bus.Publish(events.Event{Type: events.ServerState, Data: h.server(id)})
	h.f.syncServers(ctx)
	if got := h.server(id).Status; got != store.ServerOnline {
		t.Errorf("status %q of a connected agent, want online", got)
	}
	waitFor(t, "the agent in Online", func() bool {
		return slices.ContainsFunc(h.f.Online(), func(s sandbox.Host) bool { return s.ServerID == id })
	})
}

// GET /api/caches shows the mirror use of all servers, and each agent has a
// registry mirror of its own.
func TestAgentMirrorStats(t *testing.T) {
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	a, b := newFakeNode("arm64"), newFakeNode("x64")
	a.mirror = mirror.Stats{Hits: 5, Misses: 2, HitBytes: 500, MissBytes: 200, SizeBytes: 900}
	b.mirror = mirror.Stats{Hits: 1, Misses: 3, HitBytes: 100, MissBytes: 300, SizeBytes: 400}
	h.connectAgent("arm-1", a)
	h.connectAgent("amd-1", b)

	want := mirror.Stats{Hits: 6, Misses: 5, HitBytes: 600, MissBytes: 500, SizeBytes: 1300}
	if got := h.f.AgentMirrorStats(t.Context()); got != want {
		t.Errorf("agent mirror stats %+v, want %+v", got, want)
	}
}

// An agent must speak the protocol of the primary, so the fleet updates an
// agent with another version when it connects. It tries each version one
// time for each agent and shows a failure on the server.
func TestUpdateAgentOnConnect(t *testing.T) {
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	ver := version.Version
	t.Cleanup(func() { version.Version = ver })
	version.Version = "v1.4.2"
	ctx := t.Context()
	agentNode := newFakeNode("arm64")
	agentNode.updateErr = errors.New("download failed")
	_, id := h.connectAgent("arm-2", agentNode)

	tests := []struct {
		name, id, agent string
		want            []string
	}{
		{"same release", "a1", "v1.4.2", nil},
		{"newer release", "a2", "v1.5.0", []string{"v1.4.2"}},
		{"development build", "a3", "dev", []string{"v1.4.2"}},
		{"older release", id, "v1.4.1", []string{"v1.4.2"}},
		{"older release again", id, "v1.4.1", nil},
	}
	for _, tt := range tests {
		agentNode.mu.Lock()
		agentNode.updates = nil
		agentNode.mu.Unlock()
		n := &node{Node: agentNode, id: tt.id, version: tt.agent}
		if h.f.beginUpdate(n, false) {
			h.f.runUpdate(ctx, n)
		}
		agentNode.mu.Lock()
		got := slices.Clone(agentNode.updates)
		agentNode.mu.Unlock()
		if !slices.Equal(got, tt.want) {
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
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	ver := version.Version
	t.Cleanup(func() { version.Version = ver })
	version.Version = "v1.4.2"
	ctx := t.Context()
	agentNode := newFakeNode("arm64")
	a, id := h.connectAgent("arm-2", agentNode)
	state := func() string { return h.f.UpdateState(h.server(id)) }
	updates := func() int {
		agentNode.mu.Lock()
		defer agentNode.mu.Unlock()
		return len(agentNode.updates)
	}
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
	if got := h.f.UpdateState(h.server(h.f.localID)); got != api.UpdateCurrent {
		t.Errorf("update state %q of the primary, want current", got)
	}

	olderAgent()
	if got := state(); got != api.UpdateAvailable {
		t.Fatalf("update state %q of an older agent, want available", got)
	}
	agentNode.updateErr = errors.New("download failed")
	if err := h.f.UpdateAgent(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failed update", func() bool { return state() == api.UpdateFailed })
	// An admin can try a failed version again.
	agentNode.mu.Lock()
	agentNode.updateErr = nil
	agentNode.mu.Unlock()
	if err := h.f.UpdateAgent(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second update call", func() bool { return updates() == 2 })
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
	waitFor(t, "the agent to connect again", func() bool { return h.f.Driver(id) != nil })
	waitFor(t, "the end of the update", func() bool { return state() == api.UpdateCurrent })

	// An agent that does not come back fails the update.
	olderAgent()
	if err := h.f.UpdateAgent(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the third update call", func() bool { return updates() == 3 })
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
