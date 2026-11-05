// Command runplane runs the runplane primary and its tools.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

type command struct {
	summary string
	run     func(ctx context.Context, args []string) error
}

// commands are the subcommands. Other files of this package can add more in init.
var commands = map[string]command{
	"serve":      {"run the primary: dashboard, API, and sandboxes", serve},
	"setup-code": {"print a new one-time setup code for the first admin", setupCode},
	"openapi":    {"print the OpenAPI spec of the API as JSON", openapi},
	"version":    {"print the version", printVersion},
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "runplane:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("give a command")
	}
	if name := args[0]; name == "help" || name == "-h" || name == "-help" || name == "--help" {
		usage(os.Stdout)
		return nil
	}
	cmd, ok := commands[args[0]]
	if !ok {
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
	return cmd.run(ctx, args[1:])
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: runplane <command> [flags]\n\nCommands:")
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(w, "  %-15s %s\n", name, commands[name].summary)
	}
	fmt.Fprintln(w, "\nRun runplane <command> -h for the flags of a command.")
}

func serve(ctx context.Context, args []string) error {
	cfg, err := config.Parse(flag.NewFlagSet("serve", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	// The services end serve with a cause: errRestart after an update, so
	// that systemd starts the new binary, or the error of a failed service.
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
	mode, err := initMode(ctx, st, cfg)
	if err != nil {
		return err
	}
	switch {
	case mode == "":
		return errors.New("no sandbox mode: set RUNPLANE_MODE to container, vm, or dev")
	case mode == sandbox.ModeDev && !cfg.DevInsecure:
		return errors.New("mode dev runs jobs in privileged containers; set RUNPLANE_DEV_INSECURE=1 to allow it on a development machine")
	case cfg.Mode != "" && cfg.Mode != mode:
		log.Warn("the mode in the dashboard settings overrides RUNPLANE_MODE", "settings", mode, "env", cfg.Mode)
	}

	deps := api.Deps{Store: st, Auth: auth.NewService(st, subkey(key, "runplane device tokens")), Bus: &events.Bus{}, Box: box, Log: log, Config: cfg}
	p, err := startPrimary(&deps, key, mode, func() { end(errRestart) }, end)
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

// newLogger returns the logger of a process that stops when ctx ends.
func newLogger(ctx context.Context, cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	if cfg.Dev {
		level = slog.LevelDebug
	}
	return slog.New(quietHandler{slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}), ctx.Done()})
}

// quietHandler drops the records with a canceled context in an attribute
// once stop is closed: at a stop, each loop of runplane reports one, and
// they say nothing.
type quietHandler struct {
	slog.Handler
	stop <-chan struct{}
}

func (h quietHandler) Handle(ctx context.Context, r slog.Record) error {
	select {
	case <-h.stop:
		if canceled(r) {
			return nil
		}
	default:
	}
	return h.Handler.Handle(ctx, r)
}

func (h quietHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return quietHandler{h.Handler.WithAttrs(attrs), h.stop}
}

func (h quietHandler) WithGroup(name string) slog.Handler {
	return quietHandler{h.Handler.WithGroup(name), h.stop}
}

// canceled reports if an attribute of r is an error of a canceled context.
func canceled(r slog.Record) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		err, ok := a.Value.Any().(error)
		found = ok && errors.Is(err, context.Canceled)
		return !found
	})
	return found
}

// initMode returns the sandbox mode. The settings keep the mode after the
// first start, so a change in the dashboard outlives the environment of the
// install. At the first start, the mode comes from the configuration.
func initMode(ctx context.Context, st *store.Store, cfg config.Config) (sandbox.Mode, error) {
	settings, err := st.GetSettings(ctx)
	if err != nil {
		return "", err
	}
	if settings.Mode != "" || cfg.Mode == "" {
		return settings.Mode, nil
	}
	settings, err = st.UpdateSettings(ctx, func(s *store.Settings) error {
		s.Mode = cfg.Mode
		return nil
	})
	return settings.Mode, err
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

func setupCode(ctx context.Context, args []string) error {
	config.LoadEnvFile()
	cfg, err := config.Parse(flag.NewFlagSet("setup-code", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	code, err := auth.NewService(st, nil).NewSetupCode(ctx)
	if errors.Is(err, auth.ErrSetupDone) {
		return errors.New("an admin exists, so the setup is done: log in, or ask an admin for an invite")
	}
	if err != nil {
		return err
	}
	fmt.Println(code)
	return nil
}

func openapi(_ context.Context, args []string) error {
	if err := flag.NewFlagSet("openapi", flag.ContinueOnError).Parse(args); err != nil {
		return err
	}
	spec, err := api.OpenAPI()
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(spec)
	return err
}

func printVersion(_ context.Context, args []string) error {
	if err := flag.NewFlagSet("version", flag.ContinueOnError).Parse(args); err != nil {
		return err
	}
	fmt.Println("runplane", version.String())
	return nil
}
