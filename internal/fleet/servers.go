package fleet

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// ErrLocalServer means that RemoveServer got the server of the primary.
var ErrLocalServer = errors.New("the primary server cannot be removed")

// HostCheck is the doctor report of the server of the primary, for the setup
// page.
type HostCheck struct {
	doctor.Report
	Mode sandbox.Mode
}

func (f *Fleet) startLocal(ctx context.Context) error {
	info, err := f.local.Facts(ctx)
	if err != nil {
		f.log.Warn("read host facts", "err", err)
	}
	v, err := f.st.GetLocalServer(ctx)
	if errors.Is(err, store.ErrNotFound) {
		now := f.now()
		v = store.Server{
			ID: ids.New(), Name: cmp.Or(info.Hostname, "primary"), Kind: store.ServerLocal, Status: store.ServerOnline,
			CreatedAt: now, UpdatedAt: now,
		}
		err = f.st.CreateServer(ctx, v)
	}
	if err != nil {
		return fmt.Errorf("record the local server: %w", err)
	}
	hello := agent.Hello{Version: f.version, Mode: f.local.Driver().Mode(), Host: info, Addr: info.IPv4}
	if err := f.connect(ctx, v.ID, f.local, hello); err != nil {
		return fmt.Errorf("record the local server: %w", err)
	}
	return nil
}

// startAgents starts the 60 seconds after which an agent without a
// connection is unreachable. An install over SSH does not survive a restart.
func (f *Fleet) startAgents(ctx context.Context) error {
	list, err := f.st.ListServers(ctx)
	if err != nil {
		return err
	}
	now := f.now()
	for _, v := range list {
		f.mu.Lock()
		_, up := f.nodes[v.ID]
		_, installing := f.installs[v.ID]
		if v.Kind == store.ServerAgent && !up {
			f.downSince[v.ID] = now
		}
		f.mu.Unlock()
		if v.Kind == store.ServerAgent && v.Status == store.ServerInstalling && !installing {
			f.setStatusIf(ctx, v.ID, store.ServerInstalling, store.ServerUnreachable,
				"The install stopped because runplane restarted. Remove the server and add it again.")
		}
	}
	return nil
}

// AddSession takes a new agent session: the OnSession hook of agent.Listener.
func (f *Fleet) AddSession(s *agent.Session) {
	id := s.ServerID()
	f.mu.Lock()
	removed := f.removed[id]
	f.mu.Unlock()
	if removed {
		s.Close()
		return
	}
	if err := f.connect(f.bg, id, s, s.Hello()); err != nil {
		f.log.Warn("record the agent connection", "server_id", id, "err", err)
		s.Close()
	}
}

// connect records a server that connects: its facts, and the status online
// unless an admin drains or disables it. Then it gives the server its
// settings and adds its node, and an agent of another version gets the
// release of the primary.
func (f *Fleet) connect(ctx context.Context, id string, n agent.Node, h agent.Hello) error {
	v, err := f.st.GetServer(ctx, id)
	if err != nil {
		return err
	}
	now := f.now()
	if err := f.st.UpdateServerFacts(ctx, id, serverFacts(h.Host, h.Mode, h.Version, h.Addr), now); err != nil {
		return err
	}
	f.setRegion(ctx, id, h.Host.Region)
	// last_seen_at is the last contact of an agent.
	if !n.Local() {
		if err := f.st.TouchServer(ctx, id, now); err != nil {
			return err
		}
	}
	if !adminStatus(v.Status) {
		v.Status = store.ServerOnline
	}
	detail := ""
	if h.Version != f.version {
		detail = fmt.Sprintf("This server has runplane %s and the primary has %s.", h.Version, f.version)
	}
	if err := f.st.SetServerStatus(ctx, id, v.Status, detail, now); err != nil {
		return err
	}

	nd := &node{Node: n, id: id, addr: h.Addr, version: h.Version, name: v.Name, arch: h.Host.Arch, status: v.Status}
	// The server gets its settings before the scheduler can start a
	// sandbox on it.
	if st, err := f.st.GetSettings(ctx); err != nil {
		f.log.Warn("read settings for a server", "err", err)
	} else {
		f.configure(ctx, nd, NodeSettings(st))
	}
	nd.limits = f.readLimits(ctx, nd)
	if n.Local() {
		f.mu.Lock()
		f.localAddr = h.Addr
		f.mu.Unlock()
	}
	f.add(nd)
	f.endUpdate(ctx, id, h.Version)
	f.publishServer(ctx, id)
	f.log.Info("server connected", "server_id", id, "name", v.Name, "addr", h.Addr, "version", h.Version, "arch", nd.arch, "mode", h.Mode)
	f.goBg(func(ctx context.Context) {
		// The scheduler compares the sandboxes of the server with its rows.
		f.capacityChanged()
		f.kick()
		if f.beginUpdate(ctx, id, h.Version, false) == nil {
			f.runUpdate(ctx, nd)
		}
	})
	return nil
}

// adminStatus reports a status that only an admin sets and clears.
func adminStatus(status string) bool {
	return status == store.ServerDraining || status == store.ServerDisabled
}

func (f *Fleet) configure(ctx context.Context, n *node, s agent.Settings) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := n.Configure(ctx, s); err != nil {
		f.log.Warn("send settings to a server", "server_id", n.id, "err", err)
	}
}

// readLimits reads the limits of the driver of a server. A server whose
// capacity cannot be read gets no limits.
func (f *Fleet) readLimits(ctx context.Context, n *node) sandbox.Limits {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	c, err := n.Driver().Capacity(ctx)
	if err != nil {
		f.log.Warn("read the capacity of a server", "server_id", n.id, "err", err)
	}
	return c.Limits
}

// syncServers takes the status changes of the rows, and makes each online
// agent unreachable after 60 seconds without a connection.
func (f *Fleet) syncServers(ctx context.Context) {
	list, err := f.st.ListServers(ctx)
	if err != nil {
		f.log.Warn("list servers", "err", err)
		return
	}
	now := f.now()
	for _, v := range list {
		f.mu.Lock()
		_, up := f.nodes[v.ID]
		f.mu.Unlock()
		// An agent can connect while an earlier pass makes it unreachable.
		if up && v.Status == store.ServerUnreachable && f.setStatusIf(ctx, v.ID, v.Status, store.ServerOnline, "") {
			v.Status = store.ServerOnline
		}
		f.serverChanged(v)
		if up || v.Kind != store.ServerAgent || v.Status != store.ServerOnline {
			continue
		}
		f.mu.Lock()
		since, ok := f.downSince[v.ID]
		if !ok {
			// For example an admin set a server online that has no connection.
			since = now
			f.downSince[v.ID] = now
		}
		f.mu.Unlock()
		if now.Sub(since) < unreachableAfter {
			continue
		}
		if !f.setStatusIf(ctx, v.ID, store.ServerOnline, store.ServerUnreachable, "No connection to the agent.") {
			continue
		}
		f.failUpdate(ctx, v.ID, "The agent did not connect again after the update.")
		f.log.Warn("server unreachable", "server_id", v.ID, "name", v.Name, "since", since)
	}
}

// setStatusIf changes the status of a server from one value to another and
// publishes the row. It reports whether the status changed.
func (f *Fleet) setStatusIf(ctx context.Context, id, from, to, detail string) bool {
	ok, err := f.st.SetServerStatusIf(ctx, id, from, to, detail, f.now())
	if err != nil {
		f.log.Warn("set server status", "server_id", id, "status", to, "err", err)
		return false
	}
	if ok {
		f.publishServer(ctx, id)
	}
	return ok
}

func (f *Fleet) refreshFacts(ctx context.Context) {
	var wg sync.WaitGroup
	for _, n := range f.connected() {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, callTimeout)
			info, err := n.Facts(cctx)
			cancel()
			if err != nil {
				f.log.Warn("read host facts", "server_id", n.id, "err", err)
				return
			}
			facts := serverFacts(info, n.Driver().Mode(), n.version, n.addr)
			if n.Local() {
				f.mu.Lock()
				f.localAddr = info.IPv4
				f.mu.Unlock()
			}
			regionChanged := f.setRegion(ctx, n.id, info.Region)
			v, err := f.st.GetServer(ctx, n.id)
			if err != nil || v.ServerFacts == facts && !regionChanged {
				return
			}
			if err := f.st.UpdateServerFacts(ctx, n.id, facts, f.now()); err != nil {
				f.log.Warn("record host facts", "server_id", n.id, "err", err)
				return
			}
			f.publishServer(ctx, n.id)
		})
	}
	wg.Wait()
}

// setRegion records the cloud region of a server and reports whether it changed.
func (f *Fleet) setRegion(ctx context.Context, id, region string) bool {
	changed, err := f.st.SetServerRegion(ctx, id, region)
	if err != nil {
		f.log.Warn("record server region", "server_id", id, "err", err)
	}
	return changed
}

// ServerChange is a change of a server by an admin. A nil field keeps its
// value.
type ServerChange struct {
	Name   *string
	Status *string // online, draining, or disabled
}

// UpdateServer renames a server or changes its status, and returns the
// row. A status that the server cannot get now gives ErrInstalling or
// ErrNotPaused.
func (f *Fleet) UpdateServer(ctx context.Context, by audit.Actor, serverID string, c ServerChange) (store.Server, error) {
	old, err := f.st.GetServer(ctx, serverID)
	if err != nil {
		return store.Server{}, err
	}
	now := f.now()
	if c.Name != nil {
		name := strings.TrimSpace(*c.Name)
		if name == "" {
			var p field.Problems
			p.Add("name", "Enter a name.", *c.Name)
			return store.Server{}, p
		}
		if name != old.Name {
			if err := f.st.SetServerName(ctx, old.ID, name, now); err != nil {
				return store.Server{}, err
			}
		}
	}
	if c.Status != nil && *c.Status != old.Status {
		if err := statusChange(old.Status, *c.Status); err != nil {
			return store.Server{}, err
		}
		if err := f.st.SetServerStatus(ctx, old.ID, *c.Status, "", now); err != nil {
			return store.Server{}, err
		}
	}
	v, err := f.st.GetServer(ctx, old.ID)
	if err != nil {
		return store.Server{}, err
	}
	f.serverChanged(v)
	f.bus.Publish(events.ServerState(v))
	changes := map[string][2]any{}
	if v.Name != old.Name {
		changes["name"] = [2]any{old.Name, v.Name}
	}
	if v.Status != old.Status {
		changes["status"] = [2]any{old.Status, v.Status}
	}
	if len(changes) > 0 {
		f.audit.Record(ctx, by, "server.update", v.ID, map[string]any{"name": v.Name, "status": v.Status, "changes": changes})
	}
	return v, nil
}

// The errors of a status that a server cannot get now.
var (
	ErrInstalling = errors.New("the server is still installing")
	ErrNotPaused  = errors.New("only a draining or disabled server can go online")
)

// statusChange tells why a server cannot change from one status to another
// by an admin.
func statusChange(from, to string) error {
	switch {
	case from == store.ServerInstalling:
		return ErrInstalling
	case to == store.ServerOnline && !adminStatus(from):
		return fmt.Errorf("%w, and the server is %s", ErrNotPaused, from)
	}
	return nil
}

// Actions are the changes that an admin can make to a server now.
type Actions struct {
	Drain     bool
	Resume    bool // online again after a drain or disable
	Remove    bool
	Update    bool // install the release of the primary on the agent
	RunDoctor bool
}

// Actions tells which changes an admin can make to the server v now.
func (f *Fleet) Actions(v store.Server) Actions {
	connected := f.node(v.ID) != nil
	update := f.UpdateState(v)
	return Actions{
		Drain:     v.Status != store.ServerDraining && statusChange(v.Status, store.ServerDraining) == nil,
		Resume:    v.Status != store.ServerOnline && statusChange(v.Status, store.ServerOnline) == nil,
		Remove:    v.Kind != store.ServerLocal,
		Update:    connected && (update == UpdateAvailable || update == UpdateFailed),
		RunDoctor: connected,
	}
}

// RemoveServer drains an agent, stops its sandboxes, closes its connection,
// and deletes its row. The delete can change the servers of pools, or
// disable them.
func (f *Fleet) RemoveServer(ctx context.Context, by audit.Actor, serverID string) error {
	v, err := f.st.GetServer(ctx, serverID)
	if err != nil {
		return err
	}
	if v.Kind == store.ServerLocal {
		return ErrLocalServer
	}
	f.mu.Lock()
	f.removed[v.ID] = true
	delete(f.downSince, v.ID)
	n := f.nodes[v.ID]
	if n != nil {
		n.status = store.ServerDraining
	}
	cancelInstall := f.installs[v.ID]
	f.mu.Unlock()
	if cancelInstall != nil {
		cancelInstall()
	}
	if err := f.st.DeleteServerTokens(ctx, v.ID); err != nil {
		return err
	}
	if err := f.st.SetServerStatus(ctx, v.ID, store.ServerDraining, "Removing the server.", f.now()); err != nil {
		return err
	}
	f.publishServer(ctx, v.ID)
	f.capacityChanged()
	if n != nil {
		d := n.Driver()
		list, err := d.List(ctx)
		if err != nil {
			f.log.Warn("list sandboxes of a removed server", "server_id", v.ID, "err", err)
		}
		for _, sb := range list {
			if err := d.Stop(ctx, sb.ID); err != nil {
				f.log.Warn("stop sandbox of a removed server", "server_id", v.ID, "sandbox", sb.ID, "err", err)
			}
		}
		n.Close()
		f.log.Info("server removed", "server_id", v.ID, "name", v.Name, "sandboxes", len(list))
	}
	if err := f.st.DeleteServer(ctx, v.ID, f.now()); err != nil {
		return err
	}
	f.bus.Publish(events.PoolsChanged{})
	f.audit.Record(ctx, by, "server.delete", v.ID, map[string]any{"name": v.Name})
	return nil
}

// Doctor runs the doctor checks on a connected server.
func (f *Fleet) Doctor(ctx context.Context, serverID string) ([]doctor.Check, error) {
	n := f.node(serverID)
	if n == nil {
		return nil, ErrNotConnected
	}
	r, err := f.doctor(ctx, n)
	return r.Checks, err
}

// Check runs the doctor on the server of the primary, for the setup page.
func (f *Fleet) Check(ctx context.Context) (HostCheck, error) {
	r, err := f.doctor(ctx, f.local)
	if err != nil {
		return HostCheck{}, err
	}
	return HostCheck{Report: r, Mode: f.local.Driver().Mode()}, nil
}

func (f *Fleet) doctor(ctx context.Context, n agent.Node) (doctor.Report, error) {
	st, err := f.st.GetSettings(ctx)
	if err != nil {
		return doctor.Report{}, err
	}
	r, err := n.Doctor(ctx, st.MinFreeDiskPercent)
	if err != nil {
		return doctor.Report{}, fmt.Errorf("run the doctor: %w", err)
	}
	return r, nil
}

func (f *Fleet) settingsChanged(c events.SettingsChanged) {
	s := NodeSettings(c.New)
	if reflect.DeepEqual(NodeSettings(c.Old), s) {
		return
	}
	for _, n := range f.connected() {
		f.goBg(func(ctx context.Context) { f.configure(ctx, n, s) })
	}
}

// NodeSettings are the settings that each server gets from the primary.
func NodeSettings(st store.Settings) agent.Settings {
	return agent.Settings{
		Settings: sandbox.Settings{
			DNS:             st.DNSServers,
			EgressAllowlist: st.EgressAllowlist,
			BandwidthMbps:   st.BandwidthMbps,
			RegistryMirror:  st.RegistryMirror,
			ActionsCache:    st.ActionsCache,
		},
		MinFreeDiskPercent: st.MinFreeDiskPercent,
	}
}

func serverFacts(info host.Info, mode sandbox.Mode, ver, addr string) store.ServerFacts {
	return store.ServerFacts{
		Address:            cmp.Or(addr, info.IPv4),
		Provider:           info.Provider,
		OS:                 info.OS,
		Kernel:             info.Kernel,
		Arch:               info.Arch,
		Mode:               mode,
		VCPU:               info.VCPU,
		MemoryMB:           info.MemoryMB,
		DiskBytes:          info.DiskBytes,
		DiskKind:           info.DiskKind,
		KVM:                info.KVM,
		DockerVersion:      info.DockerVersion,
		SysboxVersion:      info.SysboxVersion,
		FirecrackerVersion: info.FirecrackerVersion,
		Version:            ver,
	}
}
