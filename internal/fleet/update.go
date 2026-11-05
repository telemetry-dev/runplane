package fleet

import (
	"context"
	"errors"
	"fmt"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

// UpdateState tells whether a server runs the release of the primary.
type UpdateState string

const (
	// UpdateCurrent: the server runs the version of the primary.
	UpdateCurrent UpdateState = "current"
	// UpdateAvailable: UpdateAgent installs the release of the primary.
	UpdateAvailable UpdateState = "available"
	UpdateUpdating  UpdateState = "updating"
	// UpdateFailed: the last update failed. The update_error of the server
	// tells why, and UpdateAgent tries again.
	UpdateFailed UpdateState = "failed"
	// UpdateNone: runplane cannot update the server, because the primary
	// is not a release or the server has not reported its version.
	UpdateNone UpdateState = "none"
)

// The errors of UpdateAgent.
var (
	ErrUpToDate         = errors.New("the server runs the version of the primary")
	ErrUpdateInProgress = errors.New("an update of the server runs")
	ErrCannotUpdate     = errors.New("the primary is not a release, or the server has not reported its version")
	// errTried means that the update to this version failed before, and
	// only an admin tries it again.
	errTried = errors.New("the update to this version failed before")
)

// agentUpdate is the last update of an agent to the release of the primary.
// An agent must run the release of the primary, so that the two speak the
// same protocol.
type agentUpdate struct {
	to string
	// running is true from the start until the agent connects again or
	// the update fails.
	running bool
}

// UpdateState tells whether a server runs the release of the primary.
func (f *Fleet) UpdateState(v store.Server) UpdateState {
	f.mu.Lock()
	running := f.updates[v.ID].running
	f.mu.Unlock()
	switch {
	case running:
		return UpdateUpdating
	case v.Kind == store.ServerLocal || v.Version == f.version:
		return UpdateCurrent
	case !version.IsRelease(f.version) || v.Version == "":
		return UpdateNone
	case v.UpdateError != "":
		return UpdateFailed
	}
	return UpdateAvailable
}

// UpdateAgent makes a connected agent install the release of the primary.
// The update runs in the background.
func (f *Fleet) UpdateAgent(ctx context.Context, serverID string) error {
	n := f.node(serverID)
	if n == nil {
		return ErrNotConnected
	}
	if err := f.beginUpdate(ctx, n.id, n.version, true); err != nil {
		return err
	}
	f.goBg(func(ctx context.Context) { f.runUpdate(ctx, n) })
	return nil
}

// beginUpdate marks an update of the agent id, which runs version from, as
// running, and clears the error of its last update. Without again, it
// refuses a version that the fleet tried before on this agent, so that each
// connection does not try again.
func (f *Fleet) beginUpdate(ctx context.Context, id, from string, again bool) error {
	switch {
	case from == f.version:
		return ErrUpToDate
	case !version.IsRelease(f.version) || from == "":
		return ErrCannotUpdate
	}
	f.mu.Lock()
	u := f.updates[id]
	switch {
	case u.running:
		f.mu.Unlock()
		return ErrUpdateInProgress
	case u.to == f.version && !again:
		f.mu.Unlock()
		return errTried
	}
	f.updates[id] = agentUpdate{to: f.version, running: true}
	f.mu.Unlock()
	f.setUpdateError(ctx, id, "")
	return nil
}

// runUpdate sends the update to the agent. The agent replaces its binary,
// restarts, and connects again. Its sandboxes keep running. The agent
// closes the connection for the restart, maybe before its answer arrives,
// so a closed connection does not end the update: the next connection of
// the agent or the unreachable check does.
func (f *Fleet) runUpdate(ctx context.Context, n *node) {
	f.publishServer(ctx, n.id)
	f.log.Info("update agent", "server_id", n.id, "from", n.version, "to", f.version)
	uctx, cancel := context.WithTimeout(ctx, updateTimeout)
	err := n.Update(uctx, f.version)
	cancel()
	if err != nil && !errors.Is(err, agent.ErrClosed) {
		f.log.Warn("update agent", "server_id", n.id, "to", f.version, "err", err)
		f.failUpdate(ctx, n.id, fmt.Sprintf("The update to runplane %s failed: %v.", f.version, err))
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
	switch {
	case u.running && version != u.to:
		f.failUpdate(ctx, serverID, fmt.Sprintf("The agent connected again with runplane %s, not %s.", version, u.to))
	case version == f.version:
		f.setUpdateError(ctx, serverID, "")
	}
}

// failUpdate ends the running update of a server with an error.
func (f *Fleet) failUpdate(ctx context.Context, serverID, why string) {
	f.mu.Lock()
	u := f.updates[serverID]
	if u.running {
		f.updates[serverID] = agentUpdate{to: u.to}
	}
	f.mu.Unlock()
	if !u.running {
		return
	}
	f.setUpdateError(ctx, serverID, truncate(why+" Select Update agent to try again, or run curl -fsSL <dashboard URL>/install.sh | sudo sh on this server."))
	f.publishServer(ctx, serverID)
}

func (f *Fleet) setUpdateError(ctx context.Context, serverID, msg string) {
	if err := f.st.SetServerUpdateError(ctx, serverID, msg, f.now()); err != nil {
		f.log.Warn("record the update error", "server_id", serverID, "err", err)
	}
}
