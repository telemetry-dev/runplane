//go:build linux

// runplane-init is the first process in each runplane sandbox. It gets the
// job config from the host, starts dockerd and the GitHub Actions runner, reports the runner state,
// streams the logs, serves debug shells, and stops the sandbox after the job.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

type guestInit struct {
	vm     bool
	log    *slog.Logger
	logs   *logQueue
	reaper *reaper
	req    *hostRequests
	shells *shellServer
	link   *guestLink
	cfg    guest.Config
}

func main() {
	if os.Getenv("PATH") == "" {
		os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	logs := newLogQueue(logQueueMax)
	log := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, logs.writer(guest.SourceInit)), &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.Attr{} // each log line has its own time
			}
			return a
		},
	}))
	g := &guestInit{log: log, logs: logs, reaper: newReaper(), req: newHostRequests(), shells: newShellServer(log)}
	g.handleSignals()

	vm, err := detectVM()
	if err != nil {
		log.Error("detect sandbox mode", "err", err)
		powerOff(true, 1)
	}
	g.vm = vm
	if vm {
		if err := setupVM(); err != nil {
			log.Error("set up the VM", "err", err)
			powerOff(true, 1)
		}
	} else if _, err := os.Stat(guest.CtlDir); err != nil {
		log.Error("no VM and no control socket folder: is this a runplane sandbox?", "err", err)
		os.Exit(1)
	}

	g.link = newGuestLink(dialer(vm), log, logs, guestMux(g.req, g.shells))
	go g.link.run(context.Background())
	code := g.run()
	g.finish()
	powerOff(vm, code)
}

// run returns the exit code of init in container mode.
func (g *guestInit) run() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-g.req.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	cfg, err := g.link.config(ctx)
	if err != nil {
		g.log.Info("stopped before the config came")
		return 0
	}
	g.cfg = cfg
	g.log.Info("got the config", "sandbox", cfg.SandboxID, "debug", cfg.Debug, "time_limit_seconds", cfg.TimeLimitSeconds)

	code, failure := -1, errNoStart
	var dockerd *child
	if acct, err := g.prepare(); err != nil {
		g.log.Error("prepare the sandbox", "err", err)
	} else {
		dockerd = g.startDockerd() // the runner does not wait for Docker
		code, failure = g.runRunner(acct)
	}
	g.link.event(sandbox.RunnerEvent{Kind: sandbox.RunnerExited, ExitCode: code, Error: failure})
	g.req.waitHold(cfg.Debug, holdGrace)
	if dockerd != nil {
		dockerd.stop()
	}
	if failure != "" {
		return 1
	}
	return 0
}

func (g *guestInit) prepare() (account, error) {
	if g.cfg.RunnerDir == "" {
		return account{}, errors.New("the config has no runner_dir")
	}
	if g.vm {
		if err := configureVM(g.cfg); err != nil {
			return account{}, fmt.Errorf("configure VM: %w", err)
		}
		if err := mountBundle(g.cfg.RunnerDir); err != nil {
			return account{}, err
		}
	}
	acct, err := lookupAccount(guest.RunnerUser, "/etc/passwd", "/etc/group")
	if err != nil {
		return account{}, err
	}
	if !g.vm {
		g.overlayWritablePaths()
	}
	g.shells.setStart(shellStarter(acct, g.reaper))
	if err := g.prepareRunnerDir(g.cfg.RunnerDir, acct); err != nil {
		return account{}, err
	}
	return acct, nil
}

// The host can be away, so finish waits at most 10 seconds.
func (g *guestInit) finish() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g.link.flushEvents(ctx)
	g.log.Info("stopping the sandbox")
	g.link.flushLogs(ctx)
}

// Docker stop sends SIGTERM; in a VM, Ctrl-Alt-Del sends SIGINT.
func (g *guestInit) handleSignals() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, unix.SIGTERM, unix.SIGINT)
	// Without a handler, Go exits on SIGHUP, and on SIGPIPE from a write to
	// stderr. signal.Ignore would pass the ignore on to the runner.
	other := make(chan os.Signal, 1)
	signal.Notify(other, unix.SIGHUP, unix.SIGPIPE)
	go func() {
		for {
			select {
			case s := <-stop:
				g.log.Info("got a signal, stopping", "signal", s.String())
				g.req.shutdown()
			case <-other:
			}
		}
	}()
}

func dialer(vm bool) func(context.Context) (net.Conn, error) {
	if vm {
		return func(ctx context.Context) (net.Conn, error) {
			return vsock.DialContext(ctx, vsock.Host, guest.VsockPort, nil)
		}
	}
	return func(ctx context.Context) (net.Conn, error) {
		return dialHost(ctx, guest.CtlDir)
	}
}
