// Package localnode is the server that this runplane process operates: its
// sandbox driver, its images and runner bundles, its registry mirror, its
// doctor, and its runplane binary. The primary gives the node to its fleet,
// and an agent serves it to the primary with the agent protocol.
package localnode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/datadisk"
	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/runner"
	"github.com/runplane/runplane/internal/sandbox"
)

// Updater installs a runplane release on this server: an *update.Updater.
type Updater interface {
	Apply(ctx context.Context, version string) error
}

type Config struct {
	Driver  sandbox.Driver
	Images  *image.Manager
	Bundles *runner.Bundles
	Updater Updater
	// MinFreeDiskPercent is the min_free_disk_percent setting until the
	// primary sends the settings with Configure.
	MinFreeDiskPercent float64
	// Mirror configures the registry mirror of the server. The node gives
	// the mirror its room on the disk of Mirror.Dir, with the
	// min_free_disk_percent of the last settings.
	Mirror mirror.Config
	// Doctor holds the doctor options of this server. Its DataDir also
	// selects the disk of the stats. The node adds what it knows at each
	// run: this process listens on Doctor.Listen, MinFreeDiskPercent comes
	// from the last settings, and the internet check is the Probe of Driver.
	Doctor doctor.Options
	Logger *slog.Logger
}

// Node is the server that runplane operates on.
type Node struct {
	cfg    Config
	mirror *mirror.Mirror
	stats  chan host.Stats

	mu      sync.Mutex
	minFree float64 // min_free_disk_percent of the last settings
}

var _ agent.Node = (*Node)(nil)

func New(cfg Config) (*Node, error) {
	if cfg.Driver == nil || cfg.Images == nil || cfg.Bundles == nil || cfg.Updater == nil {
		return nil, errors.New("new local node: Driver, Images, Bundles, and Updater are required")
	}
	n := &Node{cfg: cfg, stats: make(chan host.Stats, 1), minFree: cfg.MinFreeDiskPercent}
	mc := cfg.Mirror
	mc.Space = func() datadisk.Space {
		return datadisk.Of(mc.Dir, datadisk.Settings{MinFreePercent: n.minFreeDiskPercent()})
	}
	m, err := mirror.New(mc)
	if err != nil {
		return nil, fmt.Errorf("start the registry mirror: %w", err)
	}
	n.mirror = m
	return n, nil
}

// Run samples the host stats for Stats until ctx ends.
func (n *Node) Run(ctx context.Context) {
	sampler := host.Sampler{DataDir: n.cfg.Doctor.DataDir}
	for {
		err := sampler.Run(ctx, func(s host.Stats) { agent.SendLatest(n.stats, s) })
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
func (n *Node) Mirror() http.Handler { return n.mirror }

func (n *Node) Local() bool { return true }

func (n *Node) Driver() agent.Driver { return n.cfg.Driver }

func (n *Node) Configure(ctx context.Context, s agent.Settings) error {
	n.mu.Lock()
	n.minFree = s.MinFreeDiskPercent
	n.mu.Unlock()
	return n.cfg.Driver.Configure(ctx, s.Settings)
}

func (n *Node) Facts(ctx context.Context) (host.Info, error) {
	return doctor.Facts(ctx, n.cfg.Doctor)
}

func (n *Node) Doctor(ctx context.Context) (doctor.Report, error) {
	opts := n.cfg.Doctor
	opts.Serving = true
	opts.MinFreeDiskPercent = n.minFreeDiskPercent()
	opts.Internet = n.cfg.Driver.Probe
	return doctor.Run(ctx, opts), nil
}

func (n *Node) EnsureImage(ctx context.Context, ref string, progress func(agent.Progress)) (sandbox.ImageRef, error) {
	var report func(image.Progress)
	if progress != nil {
		report = func(p image.Progress) {
			progress(agent.Progress{Status: imageStatus[p.Status], Done: p.Done, Total: p.Total, Error: p.Error})
		}
	}
	img, err := n.cfg.Images.Ensure(ctx, ref, report)
	if err != nil {
		return sandbox.ImageRef{}, err
	}
	return sandbox.ImageRef{Ref: img.Ref, Digest: img.Digest}, nil
}

// imageStatus gives the status of the agent protocol for each step of
// image.Manager.Ensure.
var imageStatus = map[image.Status]agent.ImageStatus{
	image.StatusPending:    agent.ImagePending,
	image.StatusPulling:    agent.ImagePulling,
	image.StatusBuilding:   agent.ImageBuilding,
	image.StatusConverting: agent.ImageConverting,
	image.StatusReady:      agent.ImageReady,
	image.StatusFailed:     agent.ImageFailed,
}

func (n *Node) EnsureRunner(ctx context.Context, version, arch string) (agent.Bundle, error) {
	b, err := n.cfg.Bundles.Ensure(ctx, version, arch)
	return agent.Bundle{Version: b.Version, Arch: b.Arch, SHA256: b.SHA256, Patched: b.Patched}, err
}

func (n *Node) GCImages(ctx context.Context, keep []string) error {
	return n.cfg.Images.GC(ctx, keep)
}

func (n *Node) PruneRunners(_ context.Context, keep []string) error {
	return n.cfg.Bundles.Prune(keep)
}

func (n *Node) MirrorStats(context.Context) (agent.MirrorStats, error) {
	m := n.mirror.Stats()
	return agent.MirrorStats{Hits: m.Hits, Misses: m.Misses, HitBytes: m.HitBytes, MissBytes: m.MissBytes, SizeBytes: m.SizeBytes}, nil
}

func (n *Node) Update(ctx context.Context, version string) error {
	return n.cfg.Updater.Apply(ctx, version)
}

func (n *Node) Stats() <-chan host.Stats { return n.stats }

// Close stops the registry mirror. Call it after the service port stops.
func (n *Node) Close() error { return n.mirror.Close() }

func (n *Node) minFreeDiskPercent() float64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.minFree
}
