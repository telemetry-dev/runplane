package shell

import (
	"context"
	"fmt"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// LookupStore returns a Config.Lookup that reads the sandboxes and jobs in
// st. The 8-character start of an ID has only its time part, so sandboxes
// that started in the same second share it. Of several matches, it takes the
// one debug sandbox.
func LookupStore(st *store.Store) func(ctx context.Context, name string) (Target, error) {
	return func(ctx context.Context, name string) (Target, error) {
		list, err := st.FindLiveSandboxes(ctx, name)
		if err != nil {
			return Target{}, err
		}
		var debug []store.Sandbox
		for _, b := range list {
			if b.Debug {
				debug = append(debug, b)
			}
		}
		var b store.Sandbox
		switch {
		case len(debug) == 1:
			b = debug[0]
		case len(debug) > 1:
			return Target{}, ErrAmbiguous
		case len(list) > 0:
			b = list[0] // not a debug sandbox: the gateway refuses it
		default:
			return Target{}, ErrNotFound
		}

		t := Target{SandboxID: b.ID, State: b.State, Debug: b.Debug, HoldUntil: b.HoldUntil}
		if b.JobID == "" {
			return t, nil
		}
		j, err := st.GetJob(ctx, b.JobID)
		if err != nil {
			return Target{}, err
		}
		t.Actor, t.Repository, t.Job = j.Actor, j.Repository, j.JobName
		return t, nil
	}
}

// Drivers opens shells with the driver of the server that operates the
// sandbox. It fits Config.Opener and api.Deps.Shell.
type Drivers struct {
	Store *store.Store
	// Driver returns the driver of a connected server, or nil.
	Driver func(serverID string) sandbox.Driver
}

func (d Drivers) Open(ctx context.Context, sandboxID string, size sandbox.TermSize) (sandbox.Terminal, error) {
	b, err := d.Store.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	drv := d.Driver(b.ServerID)
	if drv == nil {
		return nil, fmt.Errorf("the server of sandbox %s is not connected", sandboxID)
	}
	return drv.Shell(ctx, sandboxID, size)
}
