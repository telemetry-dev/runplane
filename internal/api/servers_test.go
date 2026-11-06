package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/fleet"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// fakeServers records the token names and the agent updates, and returns
// a fixed doctor result and update states.
type fakeServers struct {
	Servers
	names   []string
	doctor  store.DoctorResult
	states  map[string]fleet.UpdateState // update state by server ID
	updated []string                     // server IDs of the UpdateAgent calls
	limits  map[string]sandbox.Limits    // the connected servers
}

func (f *fakeServers) Limits(id string) (sandbox.Limits, bool) {
	l, ok := f.limits[id]
	return l, ok
}

func (f *fakeServers) UpdateState(v store.Server) fleet.UpdateState { return f.states[v.ID] }

func (f *fakeServers) Actions(v store.Server) fleet.Actions {
	return fleet.Actions{Update: f.states[v.ID] == fleet.UpdateAvailable}
}

// UpdateAgent refuses an update as the fleet does for each state.
func (f *fakeServers) UpdateAgent(_ context.Context, _ audit.Actor, id string) error {
	switch f.states[id] {
	case fleet.UpdateCurrent:
		return fleet.ErrUpToDate
	case fleet.UpdateUpdating:
		return fleet.ErrUpdateInProgress
	case fleet.UpdateNone:
		return fleet.ErrCannotUpdate
	}
	f.updated = append(f.updated, id)
	f.states[id] = fleet.UpdateUpdating
	return nil
}

func (f *fakeServers) NewToken(_ context.Context, _ audit.Actor, _, name string) (fleet.Token, error) {
	f.names = append(f.names, name)
	return fleet.Token{ID: "tok1", Token: "rp_srv_x", Command: "curl", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (f *fakeServers) Doctor(context.Context, string) (store.DoctorResult, error) {
	return f.doctor, nil
}

func TestServerTokenWithoutNameAndItsStatus(t *testing.T) {
	servers := &fakeServers{}
	e := newEnv(t, func(d *Deps) { d.Servers = servers })
	b := e.login("ada@example.com", auth.RoleAdmin)
	ctx := context.Background()

	var tok ServerToken
	b.do("POST", "/api/servers/tokens", nil, &tok).want(t, 200)
	if tok.ID != "tok1" || !slices.Equal(servers.names, []string{""}) {
		t.Fatalf("token %+v, names %q; want token_id tok1 and an empty name for the service", tok, servers.names)
	}

	// An SSH install makes its token with the server row, before any use.
	e.addServer("srv1", "x64", store.ServerInstalling, 0, 0)
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	must(t, e.st.CreateServerToken(ctx, store.ServerToken{
		ID: "t1", TokenHash: "h1", CreatedAt: time.Now(), ExpiresAt: expires, ServerID: "srv1",
	}))
	var st ServerTokenStatus
	b.do("GET", "/api/servers/tokens/t1/status", nil, &st).want(t, 200)
	if st.Used || st.ServerID != "" || !st.ExpiresAt.Equal(expires) {
		t.Fatalf("unused token status = %+v, want not used and no server", st)
	}
	_, err := e.st.ConsumeServerToken(ctx, "h1", time.Now(), store.Server{Status: store.ServerOnline})
	must(t, err)
	b.do("GET", "/api/servers/tokens/t1/status", nil, &st).want(t, 200)
	if !st.Used || st.ServerID != "srv1" {
		t.Fatalf("used token status = %+v, want used by srv1", st)
	}
	b.do("GET", "/api/servers/tokens/nope/status", nil, nil).want(t, 404)
	e.login("bob@example.com", auth.RoleMember).do("GET", "/api/servers/tokens/t1/status", nil, nil).want(t, 403)
}

// The fleet runs the doctor and keeps the result. The API shows the result
// of a run, and later the kept result.
func TestDoctorResult(t *testing.T) {
	checkedAt := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	result := store.DoctorResult{ServerID: "srv1", CheckedAt: checkedAt, Checks: []store.DoctorCheck{
		{ID: "clock", Name: "Time synchronization", Status: "warn", Detail: "Not synchronized",
			Fix: "Turn on time sync.", Command: "sudo timedatectl set-ntp true"},
	}}
	e := newEnv(t, func(d *Deps) { d.Servers = &fakeServers{doctor: result} })
	e.addServer("srv1", "x64", store.ServerOnline, 4, 8192)
	admin := e.login("ada@example.com", auth.RoleAdmin)
	member := e.login("bob@example.com", auth.RoleMember)
	want := []Check{{ID: "clock", Name: "Time synchronization", Status: "warn", Detail: "Not synchronized",
		Fix: "Turn on time sync.", Command: "sudo timedatectl set-ntp true"}}

	var before DoctorResult
	member.do("GET", "/api/servers/srv1/doctor", nil, &before).want(t, 200)
	if before.CheckedAt != nil || len(before.Checks) != 0 {
		t.Fatalf("doctor result before a run = %+v, want none", before)
	}
	var ran DoctorResult
	admin.do("POST", "/api/servers/srv1/doctor", nil, &ran).want(t, 200)
	if ran.CheckedAt == nil || !ran.CheckedAt.Equal(checkedAt) || !slices.Equal(ran.Checks, want) {
		t.Fatalf("doctor run = %+v, want the checks of %v", ran, checkedAt)
	}

	must(t, e.st.SaveDoctorResult(context.Background(), result))
	var kept DoctorResult
	member.do("GET", "/api/servers/srv1/doctor", nil, &kept).want(t, 200)
	if kept.CheckedAt == nil || !kept.CheckedAt.Equal(checkedAt) || !slices.Equal(kept.Checks, want) {
		t.Fatalf("kept doctor result = %+v, want the checks of %v", kept, checkedAt)
	}
	member.do("GET", "/api/servers/nope/doctor", nil, nil).want(t, 404)
}

func TestServerRegion(t *testing.T) {
	e := newEnv(t)
	e.addServer("srv1", "x64", store.ServerOnline, 4, 8192)
	must(t, e.st.UpdateServerFacts(context.Background(), "srv1", store.ServerFacts{Arch: "x64", Region: "fsn1"}, time.Now()))
	var v Server
	e.login("ada@example.com", auth.RoleMember).do("GET", "/api/servers/srv1", nil, &v).want(t, 200)
	if v.Region != "fsn1" {
		t.Fatalf("region %q, want fsn1", v.Region)
	}
}

func TestPoolPlacement(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleAdmin)
	// The default reserve is 1 vCPU and 2048 MB, with no CPU overcommit.
	e.addServer("big", "x64", store.ServerOnline, 4, 16384)  // 3 vCPU, 14336 MB for sandboxes
	e.addServer("small", "x64", store.ServerOnline, 2, 8192) // 1 vCPU, 6144 MB
	e.addServer("drained", "x64", store.ServerDraining, 32, 65536)
	e.addServer("arm", "arm64", store.ServerOnline, 32, 65536)

	var p Pool
	b.do("POST", "/api/pools", map[string]any{
		"label": "build", "arch": "x64", "vcpu": 2, "memory_mb": 8192, "disk_gb": 20, "max_runners": 2,
	}, &p).want(t, 200)
	if p.OnlineServers != 2 || p.PlaceableServers != 1 || !slices.Equal(p.PlaceableServerIDs, []string{"big"}) {
		t.Fatalf("placement = %+v, want 1 of 2 servers: big", p.PoolPlacement)
	}

	var got PoolPlacement
	b.do("GET", "/api/pools/placement?arch=x64&vcpu=1&memory_mb=4096&server_ids=small,drained", nil, &got).want(t, 200)
	if got.OnlineServers != 1 || !slices.Equal(got.PlaceableServerIDs, []string{"small"}) {
		t.Fatalf("placement on small = %+v, want 1 of 1 servers: small", got)
	}

	// Overcommit 2 gives small 2 vCPUs, but its memory still does not fit 8 GB.
	b.do("PATCH", "/api/settings", map[string]any{"cpu_overcommit": 2}, nil).want(t, 200)
	b.do("GET", "/api/pools/placement?arch=x64&vcpu=2&memory_mb=6144", nil, &got).want(t, 200)
	if !slices.Equal(got.PlaceableServerIDs, []string{"big", "small"}) {
		t.Fatalf("placement with overcommit 2 = %+v, want big and small", got)
	}
	b.do("GET", "/api/pools/placement?arch=x64&vcpu=6&memory_mb=4096", nil, &got).want(t, 200)
	if !slices.Equal(got.PlaceableServerIDs, []string{"big"}) {
		t.Fatalf("placement of 6 vCPUs with overcommit 2 = %+v, want big", got)
	}
}

// The placement knows the limits of the driver of each connected server:
// a VM has at most 32 vCPUs, and each VM takes memory for its process.
func TestPoolPlacementKnowsTheDriverLimits(t *testing.T) {
	servers := &fakeServers{limits: map[string]sandbox.Limits{"vm": {OverheadMB: 64, MaxVCPU: 32}}}
	e := newEnv(t, func(d *Deps) { d.Servers = servers })
	b := e.login("ada@example.com", auth.RoleAdmin)
	e.addServer("vm", "x64", store.ServerOnline, 64, 34816)
	e.addServer("other", "x64", store.ServerOnline, 64, 34816)
	var got PoolPlacement
	b.do("GET", "/api/pools/placement?arch=x64&vcpu=48&memory_mb=4096", nil, &got).want(t, 200)
	if !slices.Equal(got.PlaceableServerIDs, []string{"other"}) {
		t.Errorf("placement of 48 vCPUs = %+v, want only the server without a vCPU limit", got)
	}
	// The default reserve leaves 32768 MB.
	b.do("GET", "/api/pools/placement?arch=x64&vcpu=2&memory_mb=32768", nil, &got).want(t, 200)
	if !slices.Equal(got.PlaceableServerIDs, []string{"other"}) {
		t.Errorf("placement of 32768 MB = %+v, want only the server without overhead", got)
	}
}

// The scheduler takes a public job only on VM servers, so the setting must
// not turn on while a server runs another mode.
func TestPublicReposNeedVMOnEachServer(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Config.Mode = sandbox.ModeVM })
	b := e.login("ada@example.com", auth.RoleAdmin)
	ctx := context.Background()
	now := time.Now()
	for _, v := range []store.Server{
		{ID: "primary", Name: "primary", Kind: store.ServerLocal, Status: store.ServerOnline, ServerFacts: store.ServerFacts{Mode: sandbox.ModeVM}},
		{ID: "agent", Name: "hel1-agent", Kind: store.ServerAgent, Status: store.ServerOnline, ServerFacts: store.ServerFacts{Mode: sandbox.ModeContainer}},
		{ID: "new", Name: "new", Kind: store.ServerAgent, Status: store.ServerInstalling}, // no facts yet
	} {
		v.CreatedAt, v.UpdatedAt = now, now
		must(t, e.st.CreateServer(ctx, v))
	}
	on := map[string]any{"allow_public_repos": true}

	res := b.do("PATCH", "/api/settings", on, nil)
	res.wantField(t, 422, "body.allow_public_repos")
	if !strings.Contains(string(res.body), "hel1-agent") {
		t.Fatalf("problem %s does not name hel1-agent", res.body)
	}

	must(t, e.st.DeleteServer(ctx, "agent", time.Now()))
	var out Settings
	b.do("PATCH", "/api/settings", on, &out).want(t, 200)
	if !out.AllowPublicRepos {
		t.Fatal("allow_public_repos is off after the patch")
	}
	// A mode change to container mode with the setting on is refused too.
	b.do("PATCH", "/api/settings", map[string]any{"mode": "container"}, nil).want(t, 422)
}

// An agent can connect in container mode while allow_public_repos is on.
// The setting stays on, the agent waits, and the API tells why. The
// placement does not count the agent, as the scheduler does not.
func TestServerWaitsWhilePublicReposAreOn(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Config.Mode = sandbox.ModeVM })
	b := e.login("ada@example.com", auth.RoleAdmin)
	ctx := context.Background()
	now := time.Now()
	addServer := func(id string, mode sandbox.Mode) {
		must(t, e.st.CreateServer(ctx, store.Server{ID: id, Name: id, Kind: store.ServerAgent, Status: store.ServerOnline,
			ServerFacts: store.ServerFacts{Arch: "x64", Mode: mode, VCPU: 8, MemoryMB: 16384}, CreatedAt: now, UpdatedAt: now}))
	}
	addServer("primary", sandbox.ModeVM)
	b.do("PATCH", "/api/settings", map[string]any{"allow_public_repos": true}, nil).want(t, 200)
	addServer("cheap-vps", sandbox.ModeContainer)
	const placement = "/api/pools/placement?arch=x64&vcpu=2&memory_mb=4096"

	var st Settings
	b.do("GET", "/api/settings", nil, &st).want(t, 200)
	if !st.AllowPublicRepos || !strings.Contains(st.PublicReposRefusal, "cheap-vps") {
		t.Errorf("settings: allow_public_repos %v, public_repos_refusal %q; want on, with the name cheap-vps", st.AllowPublicRepos, st.PublicReposRefusal)
	}
	var agent, primary Server
	b.do("GET", "/api/servers/cheap-vps", nil, &agent).want(t, 200)
	b.do("GET", "/api/servers/primary", nil, &primary).want(t, 200)
	if agent.WaitReason == "" || primary.WaitReason != "" {
		t.Errorf("wait_reason: cheap-vps %q, primary %q; want a reason only for cheap-vps", agent.WaitReason, primary.WaitReason)
	}
	var got PoolPlacement
	b.do("GET", placement, nil, &got).want(t, 200)
	if got.OnlineServers != 2 || got.WaitingServers != 1 || !slices.Equal(got.PlaceableServerIDs, []string{"primary"}) {
		t.Errorf("placement = %+v, want 2 online servers, 1 that waits, and primary", got)
	}

	b.do("PATCH", "/api/settings", map[string]any{"allow_public_repos": false}, nil).want(t, 200)
	b.do("GET", "/api/servers/cheap-vps", nil, &agent).want(t, 200)
	b.do("GET", placement, nil, &got).want(t, 200)
	if agent.WaitReason != "" || got.WaitingServers != 0 || len(got.PlaceableServerIDs) != 2 {
		t.Errorf("after the setting went off: wait_reason %q, placement %+v; want no wait", agent.WaitReason, got)
	}
}

func TestUpdateAgentOnlyWhenAnUpdateApplies(t *testing.T) {
	servers := &fakeServers{states: map[string]fleet.UpdateState{
		"same": fleet.UpdateCurrent, "old": fleet.UpdateAvailable, "broken": fleet.UpdateFailed, "busy": fleet.UpdateUpdating, "dev": fleet.UpdateNone,
	}}
	e := newEnv(t, func(d *Deps) { d.Servers = servers })
	for id := range servers.states {
		e.addServer(id, "x64", store.ServerOnline, 8, 16384)
	}
	admin := e.login("ada@example.com", auth.RoleAdmin)
	member := e.login("bob@example.com", auth.RoleMember)

	member.do("POST", "/api/servers/old/update", nil, nil).want(t, 403)
	for _, id := range []string{"same", "busy", "dev"} {
		admin.do("POST", "/api/servers/"+id+"/update", nil, nil).want(t, 409)
	}
	for _, id := range []string{"old", "broken"} {
		var out Server
		admin.do("POST", "/api/servers/"+id+"/update", nil, &out).want(t, 200)
		if out.UpdateState != string(fleet.UpdateUpdating) {
			t.Errorf("server %s: update state %q after the call, want updating", id, out.UpdateState)
		}
	}
	if !slices.Equal(servers.updated, []string{"old", "broken"}) {
		t.Fatalf("updated %v, want [old broken]", servers.updated)
	}
}

// A pool may still hold the ID of a server that no longer exists. A change
// of the pool keeps working, but a new unknown ID is refused.
func TestPoolWithAGoneServerStillChanges(t *testing.T) {
	e := newEnv(t)
	b := e.login("ada@example.com", auth.RoleAdmin)
	e.addServer("s1", "x64", store.ServerOnline, 8, 16384)
	now := time.Now()
	must(t, e.st.CreatePool(context.Background(), store.Pool{
		ID: "p1", Name: "big", Label: "big", Arch: "x64", VCPU: 2, MemoryMB: 4096, DiskGB: 64, MaxRunners: 2,
		ServerIDs: []string{"s1", "gone"}, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}))
	b.do("PATCH", "/api/pools/p1", map[string]any{"max_runners": 3}, nil).want(t, 200)
	b.do("PATCH", "/api/pools/p1", map[string]any{"server_ids": []string{"s1", "gone"}}, nil).want(t, 200)
	b.do("PATCH", "/api/pools/p1", map[string]any{"server_ids": []string{"s1", "other"}}, nil).wantField(t, 422, "body.server_ids[1]")
}

// The fleet publishes the stored server row. The browser must get the
// Server API object, because the frontend puts the event data in place of
// GET /api/servers/{id}.
func TestServerStateEventIsTheServerObject(t *testing.T) {
	servers := &fakeServers{states: map[string]fleet.UpdateState{"s1": fleet.UpdateAvailable}}
	e := newEnv(t, func(d *Deps) { d.Servers = servers })
	ch := e.login("bob@example.com", auth.RoleMember).events(t)
	ctx := context.Background()
	e.addServer("s1", "x64", store.ServerOnline, 16, 65536)
	now := time.Now()
	must(t, e.st.AppendServerStats(ctx, []store.ServerStat{{ServerID: "s1", At: now, CPUPercent: 42, MemoryPercent: 30, DiskPercent: 12}}))
	for _, b := range []store.Sandbox{
		{ID: "sb1", PoolID: "p", ServerID: "s1", Mode: sandbox.ModeContainer, State: sandbox.StateBusy, VCPU: 4, MemoryMB: 8192, CreatedAt: now},
		{ID: "sb2", PoolID: "p", ServerID: "s1", Mode: sandbox.ModeContainer, State: sandbox.StateIdle, VCPU: 2, MemoryMB: 4096, CreatedAt: now},
	} {
		must(t, e.st.InsertSandbox(ctx, b))
	}
	v, err := e.st.GetServer(ctx, "s1")
	must(t, err)

	e.publish(events.ServerState(v))
	ev := recv(t, ch)
	var got Server
	must(t, json.Unmarshal([]byte(ev.data), &got))
	if ev.name != "server.state" || got.ID != "s1" || got.UpdateState != string(fleet.UpdateAvailable) || !got.Actions.CanUpdate ||
		got.Sandboxes != 2 || got.RunningJobs != 1 || got.VCPUInUse != 6 || got.MemoryInUseMB != 12288 ||
		got.CPUPercent == nil || *got.CPUPercent != 42 || got.StatsAt == nil || !got.StatsAt.Equal(now.Truncate(time.Millisecond)) {
		t.Errorf("%s data %s: want s1 with update_state available and the update action, 2 sandboxes, 1 running job, "+
			"6 vCPUs and 12288 MB in use, and 42%% CPU at the sample time", ev.name, ev.data)
	}
}
