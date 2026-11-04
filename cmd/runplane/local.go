package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/moby/moby/client"

	"github.com/runplane/runplane/images"
	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/cache"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/initbin"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/devdocker"
	"github.com/runplane/runplane/internal/sandbox/firecracker"
	"github.com/runplane/runplane/internal/sandbox/netrules"
	"github.com/runplane/runplane/internal/sandbox/sysbox"
)

// local is the server that this process operates: its sandbox driver, its
// images and runner bundles, and the node that the fleet or the agent
// protocol uses.
type local struct {
	node    *node
	driver  sandbox.Driver
	images  *image.Manager
	bundles *image.Bundles
	close   func()
}

type localConfig struct {
	mode     sandbox.Mode
	settings sandbox.Settings // the first driver settings
	unit     string           // systemd unit of this process, for the doctor
	listen   []string         // TCP addresses of this process, for the doctor
	// token gives a GitHub token for the runner release reads. Optional.
	token func(context.Context) (string, error)
}

func newLocal(ctx context.Context, cfg config.Config, lc localConfig, log *slog.Logger) (_ *local, err error) {
	docker, err := dockerClient(ctx, lc.mode)
	if err != nil {
		return nil, err
	}
	l := &local{close: func() {}}
	defer func() {
		if err != nil {
			l.close()
		}
	}()
	if docker != nil {
		l.onClose(func() { docker.Close() })
	}
	l.images, err = image.New(image.Config{
		Mode: lc.mode, ImagesDir: cfg.ImagesDir(), Docker: docker, Init: initBinary, Logger: log.With("component", "image"),
	})
	if err != nil {
		return nil, fmt.Errorf("start the image manager: %w", err)
	}
	l.bundles = image.NewBundles(image.BundleConfig{
		Mode: lc.mode, Dir: cfg.RunnersDir(), Token: lc.token, APIURL: cfg.GitHubAPIURL, WebURL: cfg.GitHubURL,
		Logger: log.With("component", "runners"),
	})
	runners := runnerSource{bundles: l.bundles, dir: cfg.DevRunnerDir}
	l.node = &node{runners: runners}
	if runners.dir != "" {
		log.Warn("every runner version comes from RUNPLANE_DEV_RUNNER_DIR, unpatched", "dir", runners.dir)
	}

	var probe func(context.Context) error
	switch lc.mode {
	case sandbox.ModeContainer:
		rules := netrules.New(log.With("component", "netrules"))
		l.onClose(func() { rules.Close() })
		drv, err := sysbox.New(sysbox.Options{
			Docker: docker, Config: cfg, Mode: sandbox.ModeContainer, Runtime: "sysbox-runc", Scratch: sysbox.ScratchLoop,
			Bundle: runners.Ensure, InitPath: initbin.Path, NetRules: rules, Settings: lc.settings,
			Logger: log.With("driver", "sysbox"),
		})
		if err != nil {
			return nil, fmt.Errorf("start the container driver: %w", err)
		}
		l.driver, probe = drv, drv.Probe
	case sandbox.ModeDev:
		// Docker sends the requests of all sandboxes from one address on
		// macOS, so the cache server cannot tell the sandboxes apart.
		log.Warn("dev mode: sandboxes get no Actions cache, because the cache server cannot tell them apart by address")
		l.node.noCache = true
		lc.settings.ActionsCache = false
		drv, err := devdocker.New(devdocker.Options{
			Docker: docker, Config: cfg, Bundle: runners.Ensure, InitPath: initbin.Path, Settings: lc.settings,
			Logger: log.With("driver", "devdocker"),
		})
		if err != nil {
			return nil, fmt.Errorf("start the dev driver: %w", err)
		}
		l.driver, probe = drv, drv.Probe
	case sandbox.ModeVM:
		if cfg.DevRunnerDir != "" {
			return nil, errors.New("RUNPLANE_DEV_RUNNER_DIR works only in container and dev mode: VM mode needs runner drives")
		}
		drv, err := firecracker.New(firecracker.Options{
			DataDir: cfg.DataDir, Kernel: cfg.KernelPath(), ServicePort: cfg.ServicePort, RootDisk: l.images.RootDisk,
			RunnerDrive: func(ctx context.Context, version, arch string) (string, error) {
				b, err := l.bundles.Ensure(ctx, version, arch)
				return b.Ext4Path, err
			},
			CacheEnv: cache.RunnerEnv, Settings: lc.settings, Logger: log.With("driver", "firecracker"),
		})
		if err != nil {
			return nil, fmt.Errorf("start the VM driver: %w", err)
		}
		l.driver, probe = drv, drv.Probe
	default:
		return nil, fmt.Errorf("sandbox mode %q: set RUNPLANE_MODE to container, vm, or dev", lc.mode)
	}
	// The driver closes before the netrules manager and Docker.
	l.onClose(func() { l.driver.Close() })

	l.node.LocalNode, err = agent.NewLocal(agent.LocalConfig{
		Driver: l.driver, Images: l.images, Bundles: l.bundles, Updater: newUpdater(cfg),
		Doctor: host.Options{
			DataDir: cfg.DataDir, GuestKernel: cfg.KernelPath(), Listen: lc.listen, Serving: true,
			MinFreeDiskPercent: float64(lc.settings.MinFreeDiskPercent), Unit: lc.unit, Internet: probe,
			BaseImage: baseImage,
		},
		Logger: log.With("component", "node"),
	})
	if err != nil {
		return nil, err
	}
	return l, nil
}

// onClose makes close run fn first, then what close ran before.
func (l *local) onClose(fn func()) {
	prev := l.close
	l.close = func() { fn(); prev() }
}

// dockerClient connects to Docker, which container and dev mode need. VM
// mode uses Docker only to build a builtin image that it cannot pull, so
// there the client is nil when Docker does not answer.
func dockerClient(ctx context.Context, mode sandbox.Mode) (*client.Client, error) {
	docker, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("connect to Docker: %w", err)
	}
	if mode != sandbox.ModeVM {
		return docker, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := docker.Ping(ctx, client.PingOptions{}); err != nil {
		docker.Close()
		return nil, nil
	}
	return docker, nil
}

// baseImage tries to pull the default builtin image, for the doctor.
func baseImage(ctx context.Context) (string, error) {
	ref := image.BuiltinRef(images.Default)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return ref, image.CanPull(ctx, ref)
}

// initBinary returns runplane-init for the VM root disks.
func initBinary() ([]byte, error) {
	if b, err := initbin.Bytes(); b != nil || err != nil {
		return b, err
	}
	path, err := initbin.Path()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// runnerSource gives the runner bundles: downloaded releases, or the one
// folder of RUNPLANE_DEV_RUNNER_DIR for every version.
type runnerSource struct {
	bundles *image.Bundles
	dir     string
}

func (r runnerSource) Ensure(ctx context.Context, version, arch string) (image.Bundle, error) {
	if r.dir != "" {
		return image.Bundle{Version: version, Arch: arch, Dir: r.dir}, nil
	}
	return r.bundles.Ensure(ctx, version, arch)
}

// node is the LocalNode with the runner source of this process. In dev
// mode it also keeps the Actions cache off for the sandboxes.
type node struct {
	*agent.LocalNode
	runners runnerSource
	noCache bool
}

func (n *node) EnsureRunner(ctx context.Context, version, arch string) (image.Bundle, error) {
	return n.runners.Ensure(ctx, version, arch)
}

func (n *node) Configure(ctx context.Context, s sandbox.Settings) error {
	if n.noCache {
		s.ActionsCache = false
	}
	return n.LocalNode.Configure(ctx, s)
}
