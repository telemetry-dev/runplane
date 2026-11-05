package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/moby/moby/client"

	"github.com/runplane/runplane/images"
	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/datadisk"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/initbin"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/runner"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/devdocker"
	"github.com/runplane/runplane/internal/sandbox/firecracker"
	"github.com/runplane/runplane/internal/sandbox/netrules"
	"github.com/runplane/runplane/internal/sandbox/sysbox"
	"github.com/runplane/runplane/internal/update"
	"github.com/runplane/runplane/internal/version"
)

// local is the server that this process operates: its sandbox driver, and
// the node that the fleet or the agent protocol uses.
type local struct {
	node   *node
	driver sandbox.Driver
	close  func()
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
	l := &local{node: &node{}, close: func() {}}
	defer func() {
		if err != nil {
			l.close()
		}
	}()
	if docker != nil {
		l.onClose(func() { docker.Close() })
	}
	images, err := image.New(image.Config{
		Mode: lc.mode, ImagesDir: cfg.ImagesDir(), Repo: version.Repo, Docker: docker, Init: initbin.Bytes,
		Logger: log.With("component", "image"),
	})
	if err != nil {
		return nil, fmt.Errorf("start the image manager: %w", err)
	}
	bundles := runner.NewBundles(runner.BundleConfig{
		Mode: lc.mode, Dir: cfg.RunnersDir(), Releases: runner.Releases{Auth: runner.TokenAuth(lc.token)},
		Logger: log.With("component", "runners"),
	})

	var probe func(context.Context) error
	switch lc.mode {
	case sandbox.ModeContainer:
		rules := netrules.New(log.With("component", "netrules"))
		l.onClose(func() { rules.Close() })
		drv, err := sysbox.New(sysbox.Options{
			Docker: docker, Config: cfg, Mode: sandbox.ModeContainer, Runtime: "sysbox-runc", Scratch: sysbox.ScratchLoop,
			Bundle: bundles.Ensure, InitPath: initbin.Path, NetRules: rules, Settings: lc.settings,
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
			Docker: docker, Config: cfg, Bundle: bundles.Ensure, InitPath: initbin.Path, Settings: lc.settings,
			Logger: log.With("driver", "devdocker"),
		})
		if err != nil {
			return nil, fmt.Errorf("start the dev driver: %w", err)
		}
		l.driver, probe = drv, drv.Probe
	case sandbox.ModeVM:
		drv, err := firecracker.New(firecracker.Options{
			DataDir: cfg.DataDir, SandboxesDir: cfg.SandboxesDir(), ScratchDir: cfg.ScratchTemplatesDir(), JailDir: cfg.JailDir(),
			Kernel: cfg.KernelPath(), ServicePort: cfg.ServicePort, RootDisk: images.RootDisk,
			RunnerDrive: func(ctx context.Context, version, arch string) (string, bool, error) {
				b, err := bundles.Ensure(ctx, version, arch)
				return b.Ext4Path, b.Patched, err
			},
			Settings: lc.settings, Logger: log.With("driver", "firecracker"),
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
		Driver: l.driver, Images: images, Bundles: bundles, Updater: update.New(cfg.KernelPath()),
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
	ref := images.Ref(version.Repo, images.Default)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return ref, image.CanPull(ctx, ref)
}

// node is the LocalNode of this process. In dev mode it keeps the Actions
// cache off for the sandboxes. It keeps min_free_disk_percent of the last
// settings for the mirror of an agent.
type node struct {
	*agent.LocalNode
	noCache bool
	minFree atomic.Int64
}

func (n *node) Configure(ctx context.Context, s sandbox.Settings) error {
	if n.noCache {
		s.ActionsCache = false
	}
	n.minFree.Store(int64(s.MinFreeDiskPercent))
	return n.LocalNode.Configure(ctx, s)
}

// serveServicePort starts the registry mirror and serves the service port
// of the sandboxes on all addresses: /v2/ goes to the mirror, all other
// paths to the Actions cache. allow tells if an address is a sandbox, and
// space gives the room of the mirror on the data disk. The returned
// function stops the listener and the mirror.
func serveServicePort(cfg config.Config, mode sandbox.Mode, allow func(netip.Addr) bool, cache http.Handler,
	space func() datadisk.Space, log *slog.Logger) (*mirror.Mirror, func(), error) {
	if mode == sandbox.ModeDev {
		isSandbox := allow
		// Docker sends the requests of dev sandboxes from the loopback address.
		allow = func(ip netip.Addr) bool { return ip.IsLoopback() || isSandbox(ip) }
	}
	mir, err := mirror.New(mirror.Config{Dir: cfg.MirrorDir(), Space: space, Allow: allow, Logger: log.With("component", "mirror")})
	if err != nil {
		return nil, nil, fmt.Errorf("start the registry mirror: %w", err)
	}
	ln, err := net.Listen("tcp", servicePortAddr(cfg))
	if err != nil {
		mir.Close()
		return nil, nil, fmt.Errorf("listen on the service port: %w", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/") {
				mir.ServeHTTP(w, r)
				return
			}
			cache.ServeHTTP(w, r)
		}),
		// Cache blobs can be several GB, so the requests have no read or
		// write time limit.
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Error("service port", "err", err)
		}
	}()
	return mir, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			srv.Close()
		}
		<-done
		mir.Close()
	}, nil
}

func servicePortAddr(cfg config.Config) string { return fmt.Sprintf(":%d", cfg.ServicePort) }

// diskSpace returns the room of the Actions cache and the registry mirror
// on the disk of dir, which is on the data disk.
func diskSpace(dir string, s datadisk.Settings) datadisk.Space {
	total, free, err := host.DiskSpace(dir)
	if err != nil {
		total, free = 0, 0
	}
	return datadisk.Limits(s, total, free)
}
