package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/backup"
	"github.com/runplane/runplane/internal/cache"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/datadisk"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/fleet"
	"github.com/runplane/runplane/internal/github"
	"github.com/runplane/runplane/internal/joblog"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/pools"
	"github.com/runplane/runplane/internal/runner/versions"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/shell"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/tlsutil"
	"github.com/runplane/runplane/internal/update"
	"github.com/runplane/runplane/internal/version"
)

// errRestart ends serve after an update, so that systemd starts the new binary.
var errRestart = errors.New("restart")

func serve(ctx context.Context, args []string) error {
	cfg, err := config.Parse(flag.NewFlagSet("serve", flag.ContinueOnError), args, config.Listen, config.AgentListen,
		config.ServicePort, config.DataDir, config.Mode, config.Dev, config.DevInsecure, config.SSHListen)
	if err != nil {
		return err
	}
	// The services end serve with a cause: errRestart after an update, or the
	// error of a failed service.
	ctx, end := context.WithCancelCause(ctx)
	defer end(nil)
	log := newLogger(ctx, cfg)
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	key, err := secret.LoadOrCreate(cfg.SecretKeyPath())
	if err != nil {
		return err
	}
	box, err := secret.NewBox(key)
	if err != nil {
		return err
	}
	bus := &events.Bus{}
	settingsService := newSettings(st, bus, cfg, log.With("component", "settings"))
	mode, err := settingsService.InitMode(ctx)
	if err != nil {
		return err
	}
	if cfg.Mode != "" && cfg.Mode != mode {
		log.Warn("the mode in the dashboard settings overrides RUNPLANE_MODE", "settings", mode, "env", cfg.Mode)
	}

	deps := api.Deps{
		Store: st, Auth: auth.NewService(st, subkey(key, "runplane device tokens"), log.With("component", "auth")),
		Settings: settingsService, Pools: pools.New(pools.Config{Store: st, Bus: bus, Log: log.With("component", "pools")}),
		Bus: bus, Box: box, Log: log, Config: cfg,
	}
	p, err := startPrimary(&deps, key, mode, end)
	if err != nil {
		return fmt.Errorf("start services: %w", err)
	}
	defer p.stop()
	if err := logSetupCode(ctx, st, deps.Auth, log); err != nil {
		return err
	}

	handler := api.New(deps)
	p.handler = handler
	if settings := *p.settings.Load(); settings.Domain != "" {
		p.serveHTTPS(settings)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	srv.RegisterOnShutdown(handler.Close)
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen for the dashboard: %w", err)
	}
	log.Info("runplane started", "version", version.String(), "listen", ln.Addr().String(), "mode", mode, "data", cfg.DataDir)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
	}
	log.Info("runplane stops")
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		log.Warn("stop http server", "err", err)
	}
	if err := context.Cause(ctx); !errors.Is(err, context.Canceled) && !errors.Is(err, errRestart) {
		return err
	}
	return nil
}

// logSetupCode logs a new setup code while no admin exists, so the admin
// can find it in the service log. It keeps a code that is still valid,
// because install.sh printed it.
func logSetupCode(ctx context.Context, st *store.Store, a *auth.Service, log *slog.Logger) error {
	n, err := st.CountUsers(ctx)
	if err != nil || n > 0 {
		return err
	}
	open, err := st.HasOpenSetupCode(ctx, time.Now())
	if err != nil {
		return err
	}
	if open {
		log.Info("no admin exists: open the dashboard and enter the setup code. Run 'runplane setup-code' for a new setup code.")
		return nil
	}
	code, err := a.NewSetupCode(ctx)
	if errors.Is(err, auth.ErrSetupDone) {
		return nil
	}
	if err != nil {
		return err
	}
	log.Info("no admin exists: open the dashboard and enter the setup code", "code", code, "valid", auth.SetupCodeTTL)
	return nil
}

// primary holds the services of runplane serve.
type primary struct {
	cfg      config.Config
	log      *slog.Logger
	st       *store.Store
	settings atomic.Pointer[store.Settings]

	fleet   *fleet.Fleet
	sched   *scheduler.Scheduler
	gh      *github.Service
	acme    *tlsutil.ACME
	handler http.Handler // the dashboard, for HTTPS

	wg    sync.WaitGroup // work that no stop function ends
	stops []func()       // stop runs them in reverse order
}

// startPrimary starts the services of the primary and sets them in d. They
// run until stop, which serve calls after the HTTP server stopped. key is
// the secret key. end ends serve: with errRestart after an update, so that
// systemd starts runplane again, or with the error of a failed service.
func startPrimary(d *api.Deps, key []byte, mode sandbox.Mode, end func(error)) (_ *primary, err error) {
	cfg, log, st := d.Config, d.Log, d.Store
	p := &primary{cfg: cfg, log: log, st: st, acme: tlsutil.NewACME(cfg.TLSDir(), log.With("component", "https"))}
	defer func() {
		if err != nil {
			p.stop()
		}
	}()
	settings, err := st.GetSettings(context.Background())
	if err != nil {
		return nil, err
	}
	p.settings.Store(&settings)
	p.followSettings(d.Bus)

	// Release reads with an installation token have a higher rate limit.
	token := func(ctx context.Context) (string, error) { return p.gh.Token(ctx) }
	local, err := newLocal(context.Background(), cfg, localConfig{
		mode: mode, settings: fleet.NodeSettings(settings).Settings, domain: settings.Domain, token: token,
	}, log)
	if err != nil {
		return nil, err
	}
	p.stops = append(p.stops, local.close)
	p.run(func(ctx context.Context) { local.node.Run(ctx) })

	notifier := notify.New(notify.Config{Store: st, Box: d.Box, Bus: d.Bus, Log: log.With("component", "notify")})
	p.run(notifier.Run)

	cert, fingerprint, err := tlsutil.AgentCert(cfg.TLSDir())
	if err != nil {
		return nil, fmt.Errorf("make the agent certificate: %w", err)
	}
	caches, err := p.serveCaches(key, local)
	if err != nil {
		return nil, err
	}
	ghRef := &githubRef{}
	runners := versions.New(versions.Config{
		Store: st, Bus: d.Bus, GitHub: ghRef, Notifier: notifier, Log: log.With("component", "runners"),
		InUse: func() []string { return p.fleet.RunnerVersions() },
	})
	// The fleet calls the scheduler only after Run starts.
	p.fleet = fleet.New(fleet.Config{
		Store: st, Bus: d.Bus, Log: log.With("component", "fleet"), Config: cfg, Local: local.node,
		AgentFingerprint: fingerprint, Version: version.Version, Repo: version.Repo, PublicKey: update.PublicKey,
		OnEvent: p.onEvent, CapacityChanged: func() { p.sched.CapacityChanged() }, Runners: runners,
	})
	jobLogs := joblog.New(joblog.Config{Dir: cfg.LogsDir(), Store: st, Driver: p.fleet.Driver, Log: log.With("component", "joblog")})
	p.sched = scheduler.New(scheduler.Config{
		Store: st, Bus: d.Bus, Fleet: schedulerFleet{p.fleet}, GitHub: ghRef, Log: log.With("component", "scheduler"),
		JobLogs: jobLogs,
	})
	p.gh = github.New(github.Config{
		Store: st, Box: d.Box, Bus: d.Bus, Log: log.With("component", "github"), Scheduler: p.sched,
	})
	ghRef.Service = p.gh
	// The scheduler stops after the fleet, so that no driver event comes
	// after it, and the fleet after GitHub, which attaches the scale sets.
	p.run(p.sched.Run)
	p.run(func(ctx context.Context) {
		if err := p.fleet.Run(ctx); err != nil {
			end(fmt.Errorf("run the fleet: %w", err))
		}
	})
	p.run(p.gh.Run)
	p.run(runners.Run)
	p.pruneDaily("sessions, setup codes, and invites", d.Auth.Prune)
	p.pruneDaily("server tokens and host samples", p.fleet.Prune)

	if err := p.listenAgents(cert, caches.Proxied()); err != nil {
		return nil, err
	}
	opener := shell.Drivers{Store: st, Driver: p.fleet.Driver}
	if err := p.serveSSH(opener); err != nil {
		// The debug shell in the browser still works.
		log.Error("no SSH access to debug shells", "err", err)
	}

	notifyBg := func(e notify.Event) {
		p.wg.Go(func() {
			if err := notifier.Notify(context.Background(), e); err != nil {
				log.Warn("send notification", "type", e.Type, "err", err)
			}
		})
	}
	backups := backup.New(backup.Config{
		Store: st, Box: d.Box, Bus: d.Bus, Dir: cfg.BackupsDir(), Notify: notifyBg, Log: log.With("component", "backup"),
	})
	p.run(backups.Run)
	updates := update.NewService(update.Config{
		Updater: update.New(cfg.KernelPath()), Restart: func() { end(errRestart) }, Store: st, Notify: notifyBg,
		Log: log.With("component", "update"),
	})
	p.run(updates.Run)
	p.stops = append(p.stops, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.acme.Stop(ctx)
	})

	d.Scheduler = p.sched
	d.GitHub = p.gh
	d.Servers = p.fleet
	d.Images = p.fleet
	d.Caches = caches
	d.Mirrors = p.fleet
	d.Backups = backups
	d.Updater = updates
	d.Notifier = notifier
	d.Logs = jobLogs
	d.Shell = opener
	d.HostChecker = p.fleet
	d.InstallScript = p.fleet
	d.HTTPS = p.acme
	return p, nil
}

// run starts fn in the background. stop cancels its context and waits for it.
func (p *primary) run(fn func(ctx context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(ctx)
	}()
	p.stops = append(p.stops, func() { cancel(); <-done })
}

// pruneDaily runs prune at once and then each day, until stop. Each owner
// of rows that expire prunes them itself.
func (p *primary) pruneDaily(what string, prune func(ctx context.Context, now time.Time) error) {
	p.run(func(ctx context.Context) {
		tick := time.NewTicker(24 * time.Hour)
		defer tick.Stop()
		for {
			if err := prune(ctx, time.Now()); err != nil && ctx.Err() == nil {
				p.log.Warn("delete expired rows", "rows", what, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	})
}

// stop ends the services in the reverse order of their start.
func (p *primary) stop() {
	for i := len(p.stops) - 1; i >= 0; i-- {
		p.stops[i]()
	}
	p.wg.Wait()
}

// serveCaches starts the Actions cache server and the registry mirror, and
// serves them on the service port.
func (p *primary) serveCaches(key []byte, l *local) (*cache.Server, error) {
	resolver := cache.NewSandboxResolver(p.st, l.driver.List)
	space := func() datadisk.Space {
		st := p.settings.Load()
		return diskSpace(p.cfg.CacheDir(), datadisk.Settings{CacheMaxGB: st.CacheMaxGB, MinFreePercent: st.MinFreeDiskPercent})
	}
	srv, err := cache.New(cache.Config{
		Dir: p.cfg.CacheDir(), Store: p.st, Resolver: resolver, Space: space,
		SigningKey: subkey(key, "runplane cache urls"), Logger: p.log.With("component", "cache"),
	})
	if err != nil {
		return nil, fmt.Errorf("start the cache server: %w", err)
	}
	p.run(srv.Run)
	p.pruneDaily("cache stats", srv.Prune)
	stopPort, err := serveServicePort(p.cfg, l.node.Mirror(), srv, p.log)
	if err != nil {
		return nil, err
	}
	p.stops = append(p.stops, stopPort)
	return srv, nil
}

func (p *primary) listenAgents(cert tls.Certificate, cacheHandler http.Handler) error {
	ln, err := net.Listen("tcp", p.cfg.AgentListen)
	if err != nil {
		return fmt.Errorf("listen for agents: %w", err)
	}
	l := &agent.Listener{
		Cert: cert, Authenticate: p.fleet.Authenticate, OnSession: p.fleet.AddSession, CacheHandler: cacheHandler,
		Logger: p.log.With("component", "agents"),
	}
	p.run(func(ctx context.Context) {
		if err := l.Serve(ctx, ln); err != nil {
			p.log.Error("agent listener", "err", err)
		}
	})
	return nil
}

func (p *primary) serveSSH(opener shell.Opener) error {
	gw, err := shell.New(shell.Config{
		Addr: p.cfg.SSHListen, HostKeyPath: p.cfg.SSHHostKeyPath(), Opener: opener, Lookup: shell.LookupStore(p.st),
		AdminKeys: shell.AuthorizedKeysFile(p.cfg.AdminKeysPath(), p.log), Audit: p.st.InsertAudit,
		Log: p.log.With("component", "ssh"),
	})
	if err != nil {
		return fmt.Errorf("start the SSH gateway: %w", err)
	}
	p.stops = append(p.stops, func() { gw.Close() })
	p.run(func(ctx context.Context) {
		if err := gw.Serve(ctx); err != nil {
			p.log.Error("ssh gateway", "err", err)
		}
	})
	return nil
}

// onEvent passes a driver event to the scheduler, and records the address
// of a sandbox when its runner listens.
func (p *primary) onEvent(serverID string, ev sandbox.Event) {
	p.sched.HandleEvent(serverID, ev)
	if ev.Type != sandbox.EventState || ev.State != sandbox.StateIdle {
		return
	}
	drv := p.fleet.Driver(serverID)
	if drv == nil {
		return
	}
	p.wg.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p.recordIP(ctx, serverID, ev.SandboxID, drv.List)
	})
}

// recordIP saves the address that the driver of serverID lists for a
// sandbox. An agent sends its own list, so the store takes only a sandbox
// of that server: an agent cannot give a sandbox of the primary its address.
func (p *primary) recordIP(ctx context.Context, serverID, sandboxID string, list func(context.Context) ([]sandbox.Sandbox, error)) {
	boxes, err := list(ctx)
	if err != nil {
		p.log.Debug("list sandboxes for an address", "server_id", serverID, "err", err)
		return
	}
	for _, b := range boxes {
		if b.ID == sandboxID && b.IP != "" {
			if err := p.st.SetSandboxIP(ctx, serverID, b.ID, b.IP); err != nil {
				p.log.Debug("record sandbox address", "server_id", serverID, "sandbox", b.ID, "err", err)
			}
		}
	}
}

// followSettings gives each settings change to the cache server and HTTPS.
// The fleet, the scheduler, and GitHub follow the changes themselves.
func (p *primary) followSettings(bus *events.Bus) {
	sub, cancel := context.WithCancel(context.Background())
	changes := events.Subscribe[events.SettingsChanged](sub, bus)
	p.run(func(ctx context.Context) {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-changes:
				p.settings.Store(&c.New)
				if c.Old.Domain != c.New.Domain || c.Old.ACMEEmail != c.New.ACMEEmail {
					p.serveHTTPS(c.New)
				}
				if c.Old.Mode != c.New.Mode {
					p.log.Warn("the new sandbox mode applies after a restart of runplane", "mode", c.New.Mode)
				}
			}
		}
	})
}

// serveHTTPS serves the dashboard with a Let's Encrypt certificate for the
// domain setting, or stops HTTPS when the domain is empty.
func (p *primary) serveHTTPS(st store.Settings) {
	if st.Domain == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.acme.Stop(ctx)
		return
	}
	if err := p.acme.Start(st.Domain, st.ACMEEmail, p.handler); err != nil {
		p.log.Error("start HTTPS", "domain", st.Domain, "err", err)
		return
	}
	p.log.Info("HTTPS started", "domain", st.Domain)
}

// subkey derives a key for one use from the secret key.
func subkey(key []byte, use string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(use))
	return m.Sum(nil)
}
