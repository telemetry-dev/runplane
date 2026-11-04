// Package agent connects the servers of runplane (docs/architecture.md
// section 5). The primary operates its own server through a LocalNode and
// each agent through a Session: Listener accepts the agent connections on
// the primary, and Run operates the agent end on the other servers.
package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
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
	EnsureRunner(ctx context.Context, version, arch string) (image.Bundle, error)
	// GCImages removes the images of the server whose digest is not in
	// keep (image.Manager.GC).
	GCImages(ctx context.Context, keep []string) error
	// PruneRunners removes the runner bundles of the server except the two
	// newest and the versions in keep (image.Bundles.Prune).
	PruneRunners(ctx context.Context, keep []string) error
	// Configure gives the server new dashboard settings.
	Configure(ctx context.Context, s sandbox.Settings) error
	// Update replaces the runplane binary with a release. The server runs
	// the new binary after a restart.
	Update(ctx context.Context, version string) error
	// Stats delivers one host sample every 10 seconds. A reader that falls
	// behind gets only the latest sample.
	Stats() <-chan host.Stats
}

var (
	_ Node = (*LocalNode)(nil)
	_ Node = (*Session)(nil)
)

type LocalConfig struct {
	Driver  sandbox.Driver
	Images  *image.Manager
	Bundles *image.Bundles
	Updater *update.Updater // nil: update.New()
	// Doctor holds the doctor options of this server. Its DataDir also
	// selects the disk for the facts and the stats. Configure sets
	// MinFreeDiskPercent.
	Doctor host.Options
	Logger *slog.Logger
}

// LocalNode is the server that runplane operates on.
type LocalNode struct {
	driver  sandbox.Driver
	images  *image.Manager
	bundles *image.Bundles
	updater *update.Updater
	log     *slog.Logger
	stats   chan host.Stats

	mu     sync.Mutex
	doctor host.Options
}

func NewLocal(cfg LocalConfig) (*LocalNode, error) {
	if cfg.Driver == nil || cfg.Images == nil || cfg.Bundles == nil {
		return nil, errors.New("new local node: Driver, Images, and Bundles are required")
	}
	n := &LocalNode{
		driver:  cfg.Driver,
		images:  cfg.Images,
		bundles: cfg.Bundles,
		updater: cfg.Updater,
		log:     orDiscard(cfg.Logger),
		stats:   make(chan host.Stats, 1),
		doctor:  cfg.Doctor,
	}
	if n.updater == nil {
		n.updater = update.New()
	}
	return n, nil
}

// Run samples the host stats for Stats until ctx ends.
func (n *LocalNode) Run(ctx context.Context) {
	sampler := host.Sampler{DataDir: n.doctor.DataDir}
	for {
		err := sampler.Run(ctx, func(s host.Stats) { sendLatest(n.stats, s) })
		if err == nil {
			return
		}
		n.log.Warn("sample host stats", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func (n *LocalNode) Driver() sandbox.Driver { return n.driver }

func (n *LocalNode) Facts(ctx context.Context) (host.Info, error) {
	return host.Facts(ctx, n.doctor.DataDir)
}

func (n *LocalNode) Doctor(ctx context.Context) ([]host.Check, error) {
	n.mu.Lock()
	opts := n.doctor
	n.mu.Unlock()
	return host.Doctor(ctx, n.driver.Mode(), opts), nil
}

func (n *LocalNode) EnsureImage(ctx context.Context, ref string, progress func(image.Progress)) (sandbox.ImageRef, error) {
	img, err := n.images.Ensure(ctx, ref, progress)
	if err != nil {
		return sandbox.ImageRef{}, err
	}
	return sandbox.ImageRef{Ref: img.Ref, Digest: img.Digest}, nil
}

func (n *LocalNode) EnsureRunner(ctx context.Context, version, arch string) (image.Bundle, error) {
	return n.bundles.Ensure(ctx, version, arch)
}

func (n *LocalNode) GCImages(ctx context.Context, keep []string) error {
	return n.images.GC(ctx, keep)
}

func (n *LocalNode) PruneRunners(_ context.Context, keep []string) error {
	return n.bundles.Prune(keep)
}

// Configure keeps the free disk limit for the doctor and passes the
// settings to the driver if it takes them.
func (n *LocalNode) Configure(_ context.Context, s sandbox.Settings) error {
	n.mu.Lock()
	n.doctor.MinFreeDiskPercent = float64(s.MinFreeDiskPercent)
	n.mu.Unlock()
	if c, ok := n.driver.(sandbox.Configurable); ok {
		return c.Configure(s)
	}
	return nil
}

func (n *LocalNode) Update(ctx context.Context, version string) error {
	return n.updater.Apply(ctx, version)
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

func orDiscard(log *slog.Logger) *slog.Logger {
	if log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return log
}
