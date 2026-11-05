package fleet

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

// agentUpdate is the last update of an agent to the release of the primary.
// An agent must run the release of the primary, so that the two speak the
// same protocol.
type agentUpdate struct {
	to string
	// running is true from the start until the agent connects again or
	// the update fails.
	running bool
	err     string // why the update failed
}

// UpdateState tells whether a server runs the release of the primary: the
// api Servers.UpdateState.
func (f *Fleet) UpdateState(v store.Server) string {
	to := version.Version
	f.mu.Lock()
	u := f.updates[v.ID]
	f.mu.Unlock()
	switch {
	case u.running:
		return api.UpdateUpdating
	case v.Kind == store.ServerLocal || v.Version == to:
		return api.UpdateCurrent
	case !releaseVersion.MatchString(to) || v.Version == "":
		return api.UpdateNone
	case u.to == to && u.err != "":
		return api.UpdateFailed
	}
	return api.UpdateAvailable
}

// UpdateAgent makes a connected agent install the release of the primary:
// the api Servers.UpdateAgent. The update runs in the background.
func (f *Fleet) UpdateAgent(_ context.Context, serverID string) error {
	n := f.node(serverID)
	if n == nil || n.sess == nil {
		return huma.Error409Conflict("The server is not connected, so it cannot update.")
	}
	if !f.beginUpdate(n.id, n.version, true) {
		return huma.Error409Conflict("The server runs the release of the primary, or an update runs.")
	}
	f.goBg(func(ctx context.Context) { f.runUpdate(ctx, n) })
	return nil
}

// beginUpdate marks an update of the agent id, which runs version from, as
// running. It reports false if the primary is not a release, the agent runs
// its version, or an update runs. Without again, it also reports false for a
// version that the fleet tried before on this agent, so that each connection
// does not try again.
func (f *Fleet) beginUpdate(id, from string, again bool) bool {
	to := version.Version
	if !releaseVersion.MatchString(to) || from == to {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.updates[id]; u.running || u.to == to && !again {
		return false
	}
	f.updates[id] = agentUpdate{to: to, running: true}
	return true
}

// runUpdate sends the update to the agent. The agent replaces its binary,
// restarts, and connects again. Its sandboxes keep running.
func (f *Fleet) runUpdate(ctx context.Context, n *node) {
	to := version.Version
	f.publishServer(ctx, n.id)
	f.log.Info("update agent", "server_id", n.id, "from", n.version, "to", to)
	uctx, cancel := context.WithTimeout(ctx, updateTimeout)
	err := n.Update(uctx, to)
	cancel()
	if err != nil {
		f.log.Warn("update agent", "server_id", n.id, "to", to, "err", err)
		f.failUpdate(ctx, n.id, fmt.Sprintf("The update to runplane %s failed: %v.", to, err))
	}
}

// endUpdate ends the running update of an agent that connects with version.
func (f *Fleet) endUpdate(ctx context.Context, serverID, version string) {
	f.mu.Lock()
	u := f.updates[serverID]
	if u.running && version == u.to {
		f.updates[serverID] = agentUpdate{to: u.to}
	}
	f.mu.Unlock()
	if u.running && version != u.to {
		f.failUpdate(ctx, serverID, fmt.Sprintf("The agent connected again with runplane %s, not %s.", version, u.to))
	}
}

// failUpdate ends the running update of a server with an error, which
// also goes to the status detail of the server.
func (f *Fleet) failUpdate(ctx context.Context, serverID, why string) {
	f.mu.Lock()
	u := f.updates[serverID]
	if u.running {
		f.updates[serverID] = agentUpdate{to: u.to, err: why}
	}
	f.mu.Unlock()
	if !u.running {
		return
	}
	v, err := f.st.GetServer(ctx, serverID)
	if err != nil {
		return
	}
	detail := truncate(why+" Run sudo runplane update on this server.", 500)
	if err := f.st.SetServerStatus(ctx, serverID, v.Status, detail); err != nil {
		f.log.Warn("set server status", "server_id", serverID, "err", err)
	}
	f.publishServer(ctx, serverID)
}
