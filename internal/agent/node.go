// Package agent connects the servers of runplane. The primary operates its
// own server through a LocalNode and each agent through a Session: Listener
// accepts the agent connections on the primary, and Run operates the agent
// end on the other servers.
package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/runner"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/update"
)

// Node is one server as the primary sees it: its sandbox driver, its host,
// its images and runner bundles, and its runplane binary.
type Node interface {
	Driver() sandbox.Driver
	Facts(ctx context.Context) (host.Info, error)
	Doctor(ctx context.Context) ([]host.Check, error)
	// EnsureImage makes an image ready on the server. progress, if not nil,
	// gets each step.
	EnsureImage(ctx context.Context, ref string, progress func(image.Progress)) (sandbox.ImageRef, error)
	EnsureRunner(ctx context.Context, version, arch string) (runner.Bundle, error)
	// GCImages removes the images whose digest is not in keep.
	GCImages(ctx context.Context, keep []string) error
	// PruneRunners removes the runner bundles except the two newest and the
	// versions in keep.
	PruneRunners(ctx context.Context, keep []string) error
	Configure(ctx context.Context, s sandbox.Settings) error
	// Update replaces the runplane binary with a release. The server runs
	// the new binary after a restart.
	Update(ctx context.Context, version string) error
	// Stats delivers one host sample every 10 seconds. A reader that falls
	// behind gets only the latest sample.
	Stats() <-chan host.Stats
}

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

type LocalConfig struct {
	Driver  sandbox.Driver
	Images  *image.Manager
	Bundles *runner.Bundles
	Updater *update.Updater
	// Doctor holds the doctor options of this server. Its DataDir also
	// selects the disk for the facts and the stats. Configure sets
	// MinFreeDiskPercent.
	Doctor host.Options
	Logger *slog.Logger
}

// LocalNode is the server that runplane operates on.
type LocalNode struct {
	cfg   LocalConfig
	mu    sync.Mutex // guards cfg.Doctor
	stats chan host.Stats
}

func NewLocal(cfg LocalConfig) (*LocalNode, error) {
	if cfg.Driver == nil || cfg.Images == nil || cfg.Bundles == nil || cfg.Updater == nil {
		return nil, errors.New("new local node: Driver, Images, Bundles, and Updater are required")
	}
	return &LocalNode{cfg: cfg, stats: make(chan host.Stats, 1)}, nil
}

// Run samples the host stats for Stats until ctx ends.
func (n *LocalNode) Run(ctx context.Context) {
	sampler := host.Sampler{DataDir: n.cfg.Doctor.DataDir}
	for {
		err := sampler.Run(ctx, func(s host.Stats) { sendLatest(n.stats, s) })
		if err == nil {
			return
		}
		n.cfg.Logger.Warn("sample host stats", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func (n *LocalNode) Driver() sandbox.Driver { return n.cfg.Driver }

func (n *LocalNode) Facts(ctx context.Context) (host.Info, error) {
	return host.Facts(ctx, n.cfg.Doctor.DataDir)
}

func (n *LocalNode) Doctor(ctx context.Context) ([]host.Check, error) {
	n.mu.Lock()
	opts := n.cfg.Doctor
	n.mu.Unlock()
	return host.Doctor(ctx, n.cfg.Driver.Mode(), opts), nil
}

func (n *LocalNode) EnsureImage(ctx context.Context, ref string, progress func(image.Progress)) (sandbox.ImageRef, error) {
	img, err := n.cfg.Images.Ensure(ctx, ref, progress)
	if err != nil {
		return sandbox.ImageRef{}, err
	}
	return sandbox.ImageRef{Ref: img.Ref, Digest: img.Digest}, nil
}

func (n *LocalNode) EnsureRunner(ctx context.Context, version, arch string) (runner.Bundle, error) {
	return n.cfg.Bundles.Ensure(ctx, version, arch)
}

func (n *LocalNode) GCImages(ctx context.Context, keep []string) error {
	return n.cfg.Images.GC(ctx, keep)
}

func (n *LocalNode) PruneRunners(_ context.Context, keep []string) error {
	return n.cfg.Bundles.Prune(keep)
}

// Configure keeps the free disk limit for the doctor and passes the
// settings to the driver if it takes them.
func (n *LocalNode) Configure(_ context.Context, s sandbox.Settings) error {
	n.mu.Lock()
	n.cfg.Doctor.MinFreeDiskPercent = float64(s.MinFreeDiskPercent)
	n.mu.Unlock()
	if c, ok := n.cfg.Driver.(sandbox.Configurable); ok {
		return c.Configure(s)
	}
	return nil
}

func (n *LocalNode) Update(ctx context.Context, version string) error {
	return n.cfg.Updater.Apply(ctx, version)
}

func (n *LocalNode) Stats() <-chan host.Stats { return n.stats }

// sendLatest puts v in ch, a channel with a buffer of one, and drops an
// older value that nobody took.
func sendLatest[T any](ch chan T, v T) {
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
