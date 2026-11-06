package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"runtime"

	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/fleet"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/localnode"
	"github.com/runplane/runplane/internal/policy"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/sysbox"
	"github.com/runplane/runplane/internal/settings"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/update"
	"github.com/runplane/runplane/internal/version"
)

func setupCode(ctx context.Context, args []string) error {
	cfg, err := config.Parse(flag.NewFlagSet("setup-code", flag.ContinueOnError), args, config.DataDir)
	if err != nil {
		return err
	}
	st, err := openDB(cfg, false)
	if err != nil {
		return err
	}
	defer st.Close()
	code, err := auth.NewService(st, nil, nil).NewSetupCode(ctx)
	if errors.Is(err, auth.ErrSetupDone) {
		return errors.New("an admin exists, so the setup is done: log in, or ask an admin for an invite")
	}
	if err != nil {
		return err
	}
	fmt.Println(code)
	return nil
}

// runDoctor runs the doctor on this server: a primary, or an agent when
// agent.json exists. It fails when a check fails.
func runDoctor(ctx context.Context, args []string) error {
	cfg, err := config.Parse(flag.NewFlagSet("doctor", flag.ContinueOnError), args,
		config.Listen, config.AgentListen, config.ServicePort, config.DataDir, config.Mode, config.SSHListen)
	if err != nil {
		return err
	}
	isAgent := exists(cfg.AgentConfigPath())
	settings, node, err := serviceSettings(ctx, cfg, isAgent)
	if err != nil {
		return err
	}
	mode := policy.Mode(settings, cfg.Mode)
	opts := doctorOptions(cfg, mode, isAgent, settings.Domain)
	opts.MinFreeDiskPercent = node.MinFreeDiskPercent
	if profile, ok := probeProfile(mode); ok {
		docker, err := client.New(client.FromEnv)
		if err != nil {
			return fmt.Errorf("connect to Docker: %w", err)
		}
		defer docker.Close()
		probe := sysbox.ProbeOptions{Docker: docker, Profile: profile, DataDir: cfg.DataDir, DNS: node.DNS}
		opts.Internet = func(ctx context.Context) error { return sysbox.Probe(ctx, probe) }
	}
	r := doctor.Run(ctx, opts)
	if err := doctor.Format(os.Stdout, r.Checks); err != nil {
		return err
	}
	if r.Failed() {
		return errors.New("a check failed")
	}
	return nil
}

// serviceSettings returns the settings of the primary and of the node that
// the service on this server uses. An agent uses the last node settings that
// its primary sent. A primary before its first start, and an agent before
// its first connection, use the default settings.
func serviceSettings(ctx context.Context, cfg config.Config, isAgent bool) (store.Settings, agent.Settings, error) {
	settings := store.DefaultSettings()
	switch {
	case isAgent && exists(cfg.AgentSettingsPath()):
		node, err := localnode.ReadSettings(cfg.AgentSettingsPath())
		return settings, node, err
	case !isAgent && exists(cfg.DBPath()):
		var err error
		if settings, err = readSettings(ctx, cfg); err != nil {
			return store.Settings{}, agent.Settings{}, err
		}
	}
	return settings, fleet.NodeSettings(settings), nil
}

// probeProfile returns the profile of the sandbox internet probe of a mode.
// The probe needs the runplane network and the firewall rules, which the
// running service makes. In VM mode only the service can add the rule.
func probeProfile(mode sandbox.Mode) (sysbox.Profile, bool) {
	switch mode {
	case sandbox.ModeContainer:
		return sysbox.Container, true
	case sandbox.ModeDev:
		return sysbox.Dev, true
	}
	return sysbox.Profile{}, false
}

// errAgentUpdate is the error of runplane update on an agent. An agent runs the
// release of its primary, so that the two speak the same protocol, and only
// the primary knows that release.
var errAgentUpdate = errors.New("this server is an agent, so it runs the release of its primary: " +
	"select Update agent on the Servers page of the dashboard, or run curl -fsSL <dashboard URL>/install.sh | sudo sh")

func updateCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	check := flags.Bool("check", false, "only report the latest release")
	cfg, err := config.Parse(flags, args, config.DataDir)
	if err != nil {
		return err
	}
	if exists(cfg.AgentConfigPath()) {
		return errAgentUpdate
	}
	u := update.NewService(update.Config{Updater: update.New(cfg.KernelPath())})
	s, err := u.Check(ctx)
	if err != nil {
		return err
	}
	if !s.Available {
		fmt.Printf("runplane %s is the latest version.\n", s.Current)
		return nil
	}
	fmt.Printf("runplane %s is available (this is %s): %s\n", s.Latest, s.Current, s.URL)
	if *check {
		return nil
	}
	if _, err := u.Apply(ctx); err != nil {
		return err
	}
	fmt.Printf("Installed runplane %s.\n", s.Latest)
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		fmt.Println("Restart runplane to run the new version.")
		return nil
	}
	// try-restart restarts the unit only if it runs.
	out, err := exec.CommandContext(ctx, "systemctl", "try-restart", primaryUnit+".service").CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart runplane: %w: %s", err, out)
	}
	fmt.Println("runplane restarted.")
	return nil
}

// resetPassword gives a user a new random password and ends the sessions
// of the user.
func resetPassword(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	cfg, err := config.Parse(flags, args, config.DataDir)
	if err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("give the email address of the user: runplane reset-password <email>")
	}
	st, err := openDB(cfg, false)
	if err != nil {
		return err
	}
	defer st.Close()
	password, err := auth.NewService(st, nil, nil).ResetPassword(ctx, audit.Actor{}, flags.Arg(0))
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no user has the email address %s", flags.Arg(0))
	}
	if err != nil {
		return err
	}
	fmt.Println(password)
	return nil
}

// setMode stores the sandbox mode in the settings of the primary. After the
// first start, the settings win over RUNPLANE_MODE, so install.sh calls
// setMode for --mode. The mode applies when runplane starts again.
func setMode(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("set-mode", flag.ContinueOnError)
	cfg, err := config.Parse(flags, args, config.DataDir, config.DevInsecure)
	if err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("give the mode: runplane set-mode container|vm|dev")
	}
	st, err := openDB(cfg, false)
	if err != nil {
		return err
	}
	defer st.Close()
	next, err := newSettings(st, &events.Bus{}, cfg, nil).SetMode(ctx, audit.Actor{}, sandbox.Mode(flags.Arg(0)))
	if err != nil {
		return err
	}
	fmt.Printf("runplane uses %s mode after its next start.\n", next.Mode)
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

// setDomain stores the domain setting of the primary, for install.sh
// --domain. It makes the database when it does not exist, because a new
// install sets the domain before the first start. HTTPS for the domain
// starts when runplane starts again.
func setDomain(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("set-domain", flag.ContinueOnError)
	cfg, err := config.Parse(flags, args, config.DataDir)
	if err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("give the domain: runplane set-domain ci.example.com")
	}
	st, err := openDB(cfg, true)
	if err != nil {
		return err
	}
	defer st.Close()
	next, err := newSettings(st, &events.Bus{}, cfg, nil).Update(ctx, audit.Actor{}, func(s *store.Settings) error {
		s.Domain = flags.Arg(0)
		return nil
	})
	if err != nil {
		return err
	}
	if next.Domain == "" {
		fmt.Println("runplane stops HTTPS after its next start.")
	} else {
		fmt.Printf("runplane serves https://%s after its next start.\n", next.Domain)
	}
	return nil
}

func newSettings(st *store.Store, bus *events.Bus, cfg config.Config, log *slog.Logger) *settings.Service {
	kvm := func() bool {
		ok, _ := host.OpenKVM()
		return ok
	}
	return settings.New(settings.Config{Store: st, Bus: bus, Log: log, Mode: cfg.Mode, DevInsecure: cfg.DevInsecure, KVM: kvm})
}

var errAgent = errors.New("this server is an agent: run this command on the primary")

// openDB opens the database of the primary. It rejects an agent, which
// reads only runplane.env and agent.json: a database there changes nothing.
// With create, it makes the database when it does not exist.
func openDB(cfg config.Config, create bool) (*store.Store, error) {
	if exists(cfg.AgentConfigPath()) {
		return nil, errAgent
	}
	if !create && !exists(cfg.DBPath()) {
		return nil, fmt.Errorf("no database at %s", cfg.DBPath())
	}
	return store.Open(cfg.DBPath())
}

func readSettings(ctx context.Context, cfg config.Config) (store.Settings, error) {
	st, err := openDB(cfg, false)
	if err != nil {
		return store.Settings{}, err
	}
	defer st.Close()
	return st.GetSettings(ctx)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
