// Package agent connects the servers of runplane. The primary operates each
// server through a Node: its own server through a localnode.Node, and each
// agent through a Session. Listener accepts the agent connections on the
// primary, and Run operates the agent end on the other servers.
package agent

import (
	"context"
	"io"
	"time"

	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/sandbox"
)

// Node is one server as the primary sees it: its sandbox driver, its host,
// its images and runner bundles, its registry mirror, and its runplane
// binary.
type Node interface {
	// Local reports if the node is the server of this process. The other
	// nodes are agents.
	Local() bool
	Driver() Driver
	Configure(ctx context.Context, s Settings) error
	Facts(ctx context.Context) (host.Info, error)
	// Doctor runs the doctor on the server, with the min_free_disk_percent
	// of its last settings.
	Doctor(ctx context.Context) (doctor.Report, error)
	// EnsureImage makes an image ready on the server. progress, if not nil,
	// gets each step.
	EnsureImage(ctx context.Context, ref string, progress func(Progress)) (sandbox.ImageRef, error)
	EnsureRunner(ctx context.Context, version, arch string) (Bundle, error)
	// GCImages removes the images whose digest is not in keep.
	GCImages(ctx context.Context, keep []string) error
	// PruneRunners removes the runner bundles except the two newest and the
	// versions in keep.
	PruneRunners(ctx context.Context, keep []string) error
	MirrorStats(ctx context.Context) (MirrorStats, error)
	// Update replaces the runplane binary with a release. The server runs
	// the new binary after a restart.
	Update(ctx context.Context, version string) error
	// Stats delivers one host sample every 10 seconds. A reader that falls
	// behind gets only the latest sample (SendLatest).
	Stats() <-chan host.Stats
	// Close ends the connection of an agent, whose sandboxes keep
	// operating. A local node stops its registry mirror.
	Close() error
}

// Driver is the part of a sandbox driver that the primary uses on each
// server. The settings and the doctor of a server go through its Node.
type Driver interface {
	Mode() sandbox.Mode
	Start(ctx context.Context, spec sandbox.Spec) error
	Stop(ctx context.Context, id string) error
	List(ctx context.Context) ([]sandbox.Sandbox, error)
	Capacity(ctx context.Context) (sandbox.Capacity, error)
	Events() <-chan sandbox.Event
	Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	Shell(ctx context.Context, id string, size sandbox.TermSize) (sandbox.Terminal, error)
	Hold(ctx context.Context, id string, until time.Time) error
}

var _ Node = (*Session)(nil)

// Forward passes the driver events and the host stats of n on until both
// channels close. It returns ctx.Err() if ctx ends first.
func Forward(ctx context.Context, n Node, onEvent func(sandbox.Event), onStats func(host.Stats)) error {
	events, stats := n.Driver().Events(), n.Stats()
	for events != nil || stats != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			onEvent(ev)
		case s, ok := <-stats:
			if !ok {
				stats = nil
				continue
			}
			onStats(s)
		}
	}
	return nil
}

// SendLatest puts v in ch, a channel with a buffer of one, and drops an
// older value that nobody took.
func SendLatest[T any](ch chan T, v T) {
	for {
		select {
		case ch <- v:
			return
		default:
		}
		select {
		case <-ch:
		default:
		}
	}
}
