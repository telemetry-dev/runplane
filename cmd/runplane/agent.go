package main

import (
	"context"
	"errors"
	"flag"
	"net/netip"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/version"
)

// runAgent operates the sandboxes of this server for the primary in
// agent.json. The primary sends the settings when the agent connects.
func runAgent(ctx context.Context, args []string) error {
	cfg, err := config.Parse(flag.NewFlagSet("agent", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	log := newLogger(ctx, cfg)
	if err := checkMode(cfg.Mode, cfg.DevInsecure); err != nil {
		return err
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

	proxy := agent.NewCacheProxy(l.driver, log.With("component", "cache"))
	hasSandbox := func(ip netip.Addr) bool {
		list, err := l.driver.List(context.Background())
		_, ok := sandbox.FindByIP(list, ip)
		return err == nil && ok
	}
	mir, stopPort, err := serveServicePort(cfg, l.allow(hasSandbox), proxy, log)
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
