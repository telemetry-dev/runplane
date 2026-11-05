// Command runplane runs the runplane primary and its tools.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/sandbox"
)

type command struct {
	summary string
	run     func(ctx context.Context, args []string) error
}

var commands = map[string]command{
	"serve":          {"run the primary: dashboard, API, and sandboxes", serve},
	"agent":          {"run an agent: operate sandboxes for a primary (agent.json)", runAgent},
	"doctor":         {"check this server for runplane", runDoctor},
	"update":         {"install the latest release (-check: only report it)", updateCmd},
	"setup-code":     {"print a new one-time setup code for the first admin", setupCode},
	"reset-password": {"set a new random password for a user: reset-password <email>", resetPassword},
	"set-mode":       {"set the sandbox mode of the primary: set-mode container|vm|dev", setMode},
	"set-domain":     {"set the HTTPS domain of the dashboard: set-domain <domain>, or set-domain '' for no HTTPS", setDomain},
	"openapi":        {"print the OpenAPI spec of the API as JSON", openapi},
	"version":        {"print the version", printVersion},
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
	for _, name := range slices.Sorted(maps.Keys(commands)) {
		fmt.Fprintf(w, "  %-15s %s\n", name, commands[name].summary)
	}
	fmt.Fprintln(w, "\nRun runplane <command> -h for the flags of a command.")
}

var errDevInsecure = errors.New("mode dev runs jobs in privileged containers; set RUNPLANE_DEV_INSECURE=1 to allow it on a development machine")

// checkMode refuses to operate sandboxes without a mode, or in dev mode
// without RUNPLANE_DEV_INSECURE.
func checkMode(mode sandbox.Mode, devInsecure bool) error {
	switch {
	case mode == "":
		return errors.New("no sandbox mode: set RUNPLANE_MODE to container, vm, or dev")
	case mode == sandbox.ModeDev && !devInsecure:
		return errDevInsecure
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
