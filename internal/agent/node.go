// Package agent connects the servers of runplane. The primary operates its
// own server through a LocalNode and each agent through a Session: Listener
// accepts the agent connections on the primary, and Run operates the agent
// end on the other servers.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/datadisk"
	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/runner"
	"github.com/runplane/runplane/internal/sandbox"
)

// Node is one server as the primary sees it: its sandbox driver, its host,
// its images and runner bundles, its registry mirror, and its runplane
// binary.
type Node interface {
	// Local reports if the node is the server of this process. The other
	// nodes are agents.
	Local() bool
	Driver() sandbox.Driver
	Configure(ctx context.Context, s Settings) error
	Facts(ctx context.Context) (host.Info, error)
	// Doctor runs the doctor on the server. minFreeDiskPercent is the
	// min_free_disk_percent setting.
	Doctor(ctx context.Context, minFreeDiskPercent float64) (doctor.Report, error)
	// EnsureImage makes an image ready on the server. progress, if not nil,
	// gets each step.
	EnsureImage(ctx context.Context, ref string, progress func(image.Progress)) (sandbox.ImageRef, error)
	EnsureRunner(ctx context.Context, version, arch string) (runner.Bundle, error)
	// GCImages removes the images whose digest is not in keep.
	GCImages(ctx context.Context, keep []string) error
	// PruneRunners removes the runner bundles except the two newest and the
	// versions in keep.
	PruneRunners(ctx context.Context, keep []string) error
	MirrorStats(ctx context.Context) (mirror.Stats, error)
	// Update replaces the runplane binary with a release. The server runs
	// the new binary after a restart.
	Update(ctx context.Context, version string) error
	// Stats delivers one host sample every 10 seconds. A reader that falls
	// behind gets only the latest sample.
	Stats() <-chan host.Stats
	// Close ends the connection of an agent, whose sandboxes keep
	// operating. A local node stops its registry mirror.
	Close() error
}

var (
	_ Node = (*LocalNode)(nil)
	_ Node = (*Session)(nil)
)

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

// Updater installs a runplane release on this server: an *update.Updater.
type Updater interface {
	Apply(ctx context.Context, version string) error
}

type LocalConfig struct {
	Driver  sandbox.Driver
	Images  *image.Manager
	Bundles *runner.Bundles
	Updater Updater
	// Mirror configures the registry mirror of the server. The node gives
	// the mirror its room on the disk of Mirror.Dir, with the
	// min_free_disk_percent of the last settings.
	Mirror mirror.Config
	// Doctor holds the doctor options of this server. Its DataDir also
	// selects the disk of the stats. The node adds what it knows at each
	// run: this process listens on Doctor.Listen, MinFreeDiskPercent comes
	// with the run, and the internet check is the Probe of Driver.
	Doctor doctor.Options
	Logger *slog.Logger
}

// LocalNode is the server that runplane operates on.
type LocalNode struct {
	cfg    LocalConfig
	mirror *mirror.Mirror
	stats  chan host.Stats

	mu      sync.Mutex
	minFree float64 // min_free_disk_percent
}

func NewLocal(cfg LocalConfig) (*LocalNode, error) {
	if cfg.Driver == nil || cfg.Images == nil || cfg.Bundles == nil || cfg.Updater == nil {
		return nil, errors.New("new local node: Driver, Images, Bundles, and Updater are required")
	}
	n := &LocalNode{cfg: cfg, stats: make(chan host.Stats, 1)}
	mc := cfg.Mirror
	mc.Space = n.mirrorSpace
	m, err := mirror.New(mc)
	if err != nil {
		return nil, fmt.Errorf("start the registry mirror: %w", err)
	}
	n.mirror = m
	return n, nil
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

// Mirror serves the registry mirror of the server on its service port.
func (n *LocalNode) Mirror() http.Handler { return n.mirror }

func (n *LocalNode) Local() bool { return true }

func (n *LocalNode) Driver() sandbox.Driver { return n.cfg.Driver }

func (n *LocalNode) Configure(ctx context.Context, s Settings) error {
	n.mu.Lock()
	n.minFree = s.MinFreeDiskPercent
	n.mu.Unlock()
	return n.cfg.Driver.Configure(ctx, s.Settings)
}

func (n *LocalNode) Facts(ctx context.Context) (host.Info, error) {
	return doctor.Facts(ctx, n.cfg.Doctor)
}

func (n *LocalNode) Doctor(ctx context.Context, minFreeDiskPercent float64) (doctor.Report, error) {
	opts := n.cfg.Doctor
	opts.Serving = true
	opts.MinFreeDiskPercent = minFreeDiskPercent
	opts.Internet = n.cfg.Driver.Probe
	return doctor.Run(ctx, opts), nil
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

func (n *LocalNode) MirrorStats(context.Context) (mirror.Stats, error) { return n.mirror.Stats(), nil }

func (n *LocalNode) Update(ctx context.Context, version string) error {
	return n.cfg.Updater.Apply(ctx, version)
}

func (n *LocalNode) Stats() <-chan host.Stats { return n.stats }

// Close stops the registry mirror. Call it after the service port stops.
func (n *LocalNode) Close() error { return n.mirror.Close() }

// mirrorSpace is the room of the registry mirror on its disk now. A disk
// whose size runplane cannot read gives the mirror its smallest room.
func (n *LocalNode) mirrorSpace() datadisk.Space {
	n.mu.Lock()
	minFree := n.minFree
	n.mu.Unlock()
	total, free, err := host.DiskSpace(n.cfg.Mirror.Dir)
	if err != nil {
		total, free = 0, 0
	}
	return datadisk.Limits(datadisk.Settings{MinFreePercent: minFree}, total, free)
}

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
