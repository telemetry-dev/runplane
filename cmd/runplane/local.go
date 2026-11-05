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
	node   *agent.LocalNode
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
	l := &local{close: func() {}}
	// allow takes the rule that tells if an address is a sandbox, and gives
	// the rule of the service port in the mode.
	allow := func(isSandbox func(netip.Addr) bool) func(netip.Addr) bool { return isSandbox }
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

	containers := sysbox.Options{
		Docker: docker, DataDir: cfg.DataDir, Sandboxes: cfg.SandboxesDir(), ScratchTemplates: cfg.ScratchTemplatesDir(),
		ServicePort: cfg.ServicePort, InitPath: initbin.Path, Settings: lc.settings,
		Runner: func(ctx context.Context, version, arch string) (string, bool, error) {
			b, err := bundles.Ensure(ctx, version, arch)
			return b.Dir, b.Patched, err
		},
	}
	switch lc.mode {
	case sandbox.ModeContainer:
		rules := netrules.New(log.With("component", "netrules"), netip.Prefix{})
		l.onClose(func() { rules.Close() })
		containers.Profile, containers.NetRules, containers.Logger = sysbox.Container, rules, log.With("driver", "sysbox")
		drv, err := sysbox.New(containers)
		if err != nil {
			return nil, fmt.Errorf("start the container driver: %w", err)
		}
		l.driver = drv
	case sandbox.ModeDev:
		containers.Logger = log.With("driver", "devdocker")
		drv, err := devdocker.New(cfg.DevInsecure, containers)
		if err != nil {
			return nil, fmt.Errorf("start the dev driver: %w", err)
		}
		l.driver, allow = drv, devdocker.Allow
	case sandbox.ModeVM:
		rules := netrules.New(log.With("component", "netrules"), netrules.DefaultTapSubnet)
		l.onClose(func() { rules.Close() })
		drv, err := firecracker.New(firecracker.Options{
			DataDir: cfg.DataDir, Sandboxes: cfg.SandboxesDir(), ScratchTemplates: cfg.ScratchTemplatesDir(), JailDir: cfg.JailDir(),
			Kernel: cfg.KernelPath(), ServicePort: cfg.ServicePort, RootDisk: images.RootDisk,
			RunnerDrive: func(ctx context.Context, version, arch string) (string, bool, error) {
				b, err := bundles.Ensure(ctx, version, arch)
				return b.Ext4Path, b.Patched, err
			},
			NetRules: rules, Settings: lc.settings, Logger: log.With("driver", "firecracker"),
		})
		if err != nil {
			return nil, fmt.Errorf("start the VM driver: %w", err)
		}
		l.driver = drv
	default:
		return nil, fmt.Errorf("sandbox mode %q: set RUNPLANE_MODE to container, vm, or dev", lc.mode)
	}
	// The driver closes before the netrules manager and Docker.
	l.onClose(func() { l.driver.Close() })

	isSandbox := func(ip netip.Addr) bool {
		list, err := l.driver.List(context.Background())
		_, ok := sandbox.FindByIP(list, ip)
		return err == nil && ok
	}
	l.node, err = agent.NewLocal(agent.LocalConfig{
		Driver: l.driver, Images: images, Bundles: bundles, Updater: update.New(cfg.KernelPath()),
		Mirror: mirror.Config{Dir: cfg.MirrorDir(), Allow: allow(isSandbox), Logger: log.With("component", "mirror")},
		Doctor: host.Options{
			DataDir: cfg.DataDir, GuestKernel: cfg.KernelPath(), Listen: lc.listen, Serving: true, Unit: lc.unit,
			BaseImage: baseImage,
		},
		Logger: log.With("component", "node"),
	})
	if err != nil {
		return nil, err
	}
	// The registry mirror stops after the service port and before the driver.
	l.onClose(func() { l.node.Close() })
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

// serveServicePort serves the service port of the sandboxes on all
// addresses: /v2/ goes to the registry mirror, all other paths to the
// Actions cache. The returned function stops the listener.
func serveServicePort(cfg config.Config, mir, cache http.Handler, log *slog.Logger) (func(), error) {
	ln, err := net.Listen("tcp", servicePortAddr(cfg))
	if err != nil {
		return nil, fmt.Errorf("listen on the service port: %w", err)
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
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			srv.Close()
		}
		<-done
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
