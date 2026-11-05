package fleet

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

// startLocal records the server of the primary and connects its node.
func (f *Fleet) startLocal(ctx context.Context) error {
	info, err := f.local.Facts(ctx)
	if err != nil {
		f.log.Warn("read host facts", "err", err)
	}
	facts := serverFacts(info, f.local.Driver().Mode(), version.Version, info.IPv4)
	v, err := f.st.GetLocalServer(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		now := f.now()
		v = store.Server{
			ID: ids.New(), Name: cmp.Or(info.Hostname, "primary"), Kind: store.ServerLocal, Status: store.ServerOnline,
			ServerFacts: facts, CreatedAt: now, UpdatedAt: now,
		}
		if err := f.st.CreateServer(ctx, v); err != nil {
			return fmt.Errorf("record the local server: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read the local server: %w", err)
	default:
		if err := f.st.UpdateServerFacts(ctx, v.ID, facts); err != nil {
			return fmt.Errorf("record the local server: %w", err)
		}
		if !adminStatus(v.Status) && v.Status != store.ServerOnline {
			v.Status = store.ServerOnline
			if err := f.st.SetServerStatus(ctx, v.ID, v.Status, ""); err != nil {
				return fmt.Errorf("record the local server: %w", err)
			}
		}
	}
	f.setRegion(ctx, v.ID, info.Region)
	f.mu.Lock()
	f.localID, f.localAddr = v.ID, info.IPv4
	f.mu.Unlock()
	f.add(&node{Node: f.local, id: v.ID, version: version.Version, name: v.Name, arch: facts.Arch, status: v.Status})
	f.publishServer(ctx, v.ID)
	f.log.Info("local server ready", "server_id", v.ID, "name", v.Name, "arch", facts.Arch, "mode", facts.Mode)
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
	ctx := f.bg
	id, h := s.ServerID(), s.Hello()
	f.mu.Lock()
	removed := f.removed[id]
	f.mu.Unlock()
	if removed {
		s.Close()
		return
	}
	v, err := f.connect(ctx, id, h)
	if err != nil {
		f.log.Warn("record the agent connection", "server_id", id, "err", err)
		s.Close()
		return
	}
	n := &node{Node: s, id: id, sess: s, addr: h.Addr, version: h.Version, name: v.Name, arch: h.Host.Arch, status: v.Status}
	// The driver gets its settings before the scheduler can start a
	// sandbox on it.
	if st, err := f.st.GetSettings(ctx); err != nil {
		f.log.Warn("read settings for an agent", "err", err)
	} else {
		f.configure(ctx, n, DriverSettings(st))
	}
	f.add(n)
	f.endUpdate(ctx, id, h.Version)
	f.publishServer(ctx, id)
	f.log.Info("server connected", "server_id", id, "name", v.Name, "addr", h.Addr, "version", h.Version, "arch", n.arch)
	f.goBg(func(ctx context.Context) {
		// The scheduler compares the sandboxes of the server with its rows.
		f.capacityChanged()
		f.kick()
		if f.beginUpdate(id, h.Version, false) {
			f.runUpdate(ctx, n)
		}
	})
}

// connect records the facts of a connected agent and sets it online, unless
// an admin drains or disables it.
func (f *Fleet) connect(ctx context.Context, id string, h agent.Hello) (store.Server, error) {
	v, err := f.st.GetServer(ctx, id)
	if err != nil {
		return v, err
	}
	if err := f.st.UpdateServerFacts(ctx, id, serverFacts(h.Host, h.Mode, h.Version, h.Addr)); err != nil {
		return v, err
	}
	f.setRegion(ctx, id, h.Host.Region)
	if err := f.st.TouchServer(ctx, id, f.now()); err != nil {
		return v, err
	}
	if !adminStatus(v.Status) {
		v.Status = store.ServerOnline
	}
	detail := ""
	if h.Version != version.Version {
		detail = fmt.Sprintf("This server has runplane %s and the primary has %s.", h.Version, version.Version)
	}
	return v, f.st.SetServerStatus(ctx, id, v.Status, detail)
}

// adminStatus reports a status that only an admin sets and clears.
func adminStatus(status string) bool {
	return status == store.ServerDraining || status == store.ServerDisabled
}

func (f *Fleet) configure(ctx context.Context, n *node, s sandbox.Settings) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := n.Driver().Configure(ctx, s); err != nil {
		f.log.Warn("send settings to a server", "server_id", n.id, "err", err)
	}
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
		lastSeen := since
		if v.LastSeenAt != nil {
			lastSeen = *v.LastSeenAt
		}
		f.notify(func(dashboard string) notify.Event {
			return notify.ServerUnreachableEvent(dashboard, v.ID, v.Name, lastSeen)
		})
	}
}

// setStatusIf changes the status of a server from one value to another and
// publishes the row. It reports whether the status changed.
func (f *Fleet) setStatusIf(ctx context.Context, id, from, to, detail string) bool {
	ok, err := f.st.SetServerStatusIf(ctx, id, from, to, detail)
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
			if n.sess == nil {
				f.mu.Lock()
				f.localAddr = info.IPv4
				f.mu.Unlock()
			}
			regionChanged := f.setRegion(ctx, n.id, info.Region)
			v, err := f.st.GetServer(ctx, n.id)
			if err != nil || v.ServerFacts == facts && !regionChanged {
				return
			}
			if err := f.st.UpdateServerFacts(ctx, n.id, facts); err != nil {
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

// Remove drains an agent, stops its sandboxes, and closes its connection.
// The API then deletes the row.
func (f *Fleet) Remove(ctx context.Context, serverID string) error {
	v, err := f.st.GetServer(ctx, serverID)
	if err != nil {
		return err
	}
	if v.Kind == store.ServerLocal {
		return errors.New("the primary server cannot be removed")
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
	if err := f.st.SetServerStatus(ctx, v.ID, store.ServerDraining, "Removing the server."); err != nil {
		return err
	}
	f.publishServer(ctx, v.ID)
	f.capacityChanged()
	if n == nil {
		return nil
	}
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
	n.sess.Close()
	f.log.Info("server removed", "server_id", v.ID, "name", v.Name, "sandboxes", len(list))
	return nil
}

func (f *Fleet) Doctor(ctx context.Context, serverID string) ([]api.Check, error) {
	n := f.node(serverID)
	if n == nil {
		return nil, errors.New("the server is not connected, so its checks cannot run")
	}
	checks, err := n.Doctor(ctx, f.minFreeDiskPercent())
	if err != nil {
		return nil, fmt.Errorf("run the doctor: %w", err)
	}
	return apiChecks(checks), nil
}

// Check reads the server of the primary and runs its doctor checks for the
// setup page.
func (f *Fleet) Check(ctx context.Context) (api.HostCheck, error) {
	info, err := f.local.Facts(ctx)
	if err != nil {
		return api.HostCheck{}, fmt.Errorf("read host facts: %w", err)
	}
	checks, err := f.local.Doctor(ctx, f.minFreeDiskPercent())
	if err != nil {
		return api.HostCheck{}, fmt.Errorf("run the doctor: %w", err)
	}
	return api.HostCheck{
		Hostname: info.Hostname, Mode: string(f.local.Driver().Mode()), KVM: info.KVM, Arch: info.Arch, VCPU: info.VCPU,
		MemoryMB: info.MemoryMB, DiskBytes: info.DiskBytes, FreeDiskBytes: info.FreeDiskBytes, DiskKind: info.DiskKind,
		OS: info.OS, Checks: apiChecks(checks),
	}, nil
}

// SettingsChanged sends the new driver settings to each connected server.
func (f *Fleet) SettingsChanged(_ context.Context, prev, next store.Settings) {
	f.mu.Lock()
	f.minFree = next.MinFreeDiskPercent
	f.mu.Unlock()
	s := DriverSettings(next)
	if reflect.DeepEqual(DriverSettings(prev), s) {
		return
	}
	for _, n := range f.connected() {
		f.goBg(func(ctx context.Context) { f.configure(ctx, n, s) })
	}
}

// DriverSettings are the settings that the sandbox drivers obey.
func DriverSettings(st store.Settings) sandbox.Settings {
	return sandbox.Settings{
		DNS:             st.DNSServers,
		EgressAllowlist: st.EgressAllowlist,
		BandwidthMbps:   st.BandwidthMbps,
		RegistryMirror:  st.RegistryMirror,
		ActionsCache:    st.ActionsCache,
	}
}

func (f *Fleet) minFreeDiskPercent() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.minFree
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

func apiChecks(checks []host.Check) []api.Check {
	out := make([]api.Check, len(checks))
	for i, c := range checks {
		out[i] = api.Check{ID: c.ID, Name: c.Name, Status: string(c.Status), Detail: c.Detail, Fix: c.Fix, Command: c.Command}
	}
	return out
}
