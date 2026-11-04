package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/netip"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/version"
)

func init() {
	commands["agent"] = command{"run an agent: operate sandboxes for a primary (agent.json)", runAgent}
}

// runAgent operates the sandboxes of this server for the primary in
// agent.json. The primary sends the settings when the agent connects.
func runAgent(ctx context.Context, args []string) error {
	cfg, err := config.Parse(flag.NewFlagSet("agent", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	log := newLogger(ctx, cfg)
	switch {
	case cfg.Mode == "":
		return errors.New("no sandbox mode: set RUNPLANE_MODE to container, vm, or dev")
	case cfg.Mode == sandbox.ModeDev && !cfg.DevInsecure:
		return errors.New("mode dev runs jobs in privileged containers; set RUNPLANE_DEV_INSECURE=1 to allow it on a development machine")
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	l, err := newLocal(ctx, cfg, localConfig{mode: cfg.Mode, unit: "runplane-agent", listen: []string{servicePortAddr(cfg)}}, log)
	if err != nil {
		return err
	}
	defer l.close()
	nodeCtx, stopNode := context.WithCancel(context.Background())
	nodeDone := make(chan struct{})
	go func() { l.node.Run(nodeCtx); close(nodeDone) }()
	defer func() { stopNode(); <-nodeDone }()

	allow := func(ip netip.Addr) bool { return hasSandbox(l.driver, ip) }
	if cfg.Mode == sandbox.ModeDev {
		// Docker sends the requests of dev sandboxes from the loopback address.
		allow = func(ip netip.Addr) bool { return ip.IsLoopback() || hasSandbox(l.driver, ip) }
	}
	mir, err := mirror.New(mirror.Config{Dir: cfg.MirrorDir(), MaxBytes: mirrorLimit(cfg.DataDir), Allow: allow, Logger: log.With("component", "mirror")})
	if err != nil {
		return fmt.Errorf("start the registry mirror: %w", err)
	}
	defer mir.Close()
	proxy := agent.NewCacheProxy(l.driver, log.With("component", "cache"))
	stopPort, err := serveServicePort(cfg, mir, proxy, log)
	if err != nil {
		return err
	}
	defer stopPort()

	log.Info("runplane agent started", "version", version.String(), "mode", cfg.Mode, "config", cfg.AgentConfigPath())
	err = agent.Run(ctx, agent.Config{Path: cfg.AgentConfigPath(), Node: l.node, CacheProxy: proxy, Mirror: mir.Stats, Logger: log})
	if errors.Is(err, agent.ErrUpdated) {
		// systemd starts the new binary. The sandboxes keep operating, and
		// the driver takes them over after the restart.
		log.Info("runplane agent restarts to run the new version")
		return nil
	}
	return err
}

// hasSandbox reports if a sandbox of the driver has the address.
func hasSandbox(drv sandbox.Driver, ip netip.Addr) bool {
	list, err := drv.List(context.Background())
	if err != nil {
		return false
	}
	for _, b := range list {
		if b.IP == ip.String() {
			return true
		}
	}
	return false
}
