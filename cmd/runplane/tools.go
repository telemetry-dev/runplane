package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"slices"

	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/sysbox"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/update"
	"github.com/runplane/runplane/internal/version"
)

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

// doctor runs the checks of the host package for this server: a primary,
// or an agent when agent.json exists. It fails when a check fails.
func doctor(ctx context.Context, args []string) error {
	config.LoadEnvFile()
	cfg, err := config.Parse(flag.NewFlagSet("doctor", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	mode := cfg.Mode
	var dns []string
	opts := host.Options{DataDir: cfg.DataDir, GuestKernel: cfg.KernelPath(), BaseImage: baseImage}
	if exists(cfg.AgentConfigPath()) {
		opts.Unit, opts.Listen = "runplane-agent", []string{servicePortAddr(cfg)}
	} else {
		var settings store.Settings
		if exists(cfg.DBPath()) {
			if settings, err = readSettings(ctx, cfg); err != nil {
				return err
			}
		}
		// The dashboard settings win over the environment.
		mode = cmp.Or(settings.Mode, mode)
		opts.Unit, opts.Listen = "runplane", primaryAddrs(cfg, settings.Domain)
		opts.MinFreeDiskPercent = settings.MinFreeDiskPercent
		dns = settings.DNSServers
	}
	// The probe needs the runplane network and the firewall rules, which the
	// running service makes. In VM mode only the service can add the rule.
	opts.Internet = func(context.Context) error { return host.ErrProbeInService }
	if mode == sandbox.ModeContainer || mode == sandbox.ModeDev {
		probe, err := internetProbe(cfg, mode, dns)
		if err != nil {
			return err
		}
		defer probe.Docker.Close()
		opts.Internet = func(ctx context.Context) error { return sysbox.Probe(ctx, probe) }
	}
	checks := host.Doctor(ctx, mode, opts)
	if err := host.FormatDoctor(os.Stdout, checks); err != nil {
		return err
	}
	if slices.ContainsFunc(checks, func(c host.Check) bool { return c.Status == host.StatusFail }) {
		return errors.New("a check failed")
	}
	return nil
}

// internetProbe returns the options of the sandbox internet probe for the
// container and dev mode drivers, with the runtimes and the cgroup parent
// of sysbox.New and devdocker.New.
func internetProbe(cfg config.Config, mode sandbox.Mode, dns []string) (sysbox.ProbeOptions, error) {
	o := sysbox.ProbeOptions{DataDir: cfg.DataDir, Runtime: "runc"}
	if mode == sandbox.ModeContainer {
		o.Runtime, o.CgroupParent = "sysbox-runc", "runplane.slice"
	}
	for _, v := range dns {
		if a, err := netip.ParseAddr(v); err == nil && a.Is4() {
			o.DNS = append(o.DNS, a)
		}
	}
	docker, err := client.New(client.FromEnv)
	if err != nil {
		return o, fmt.Errorf("connect to Docker: %w", err)
	}
	o.Docker = docker
	return o, nil
}

func updateCmd(ctx context.Context, args []string) error {
	config.LoadEnvFile()
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	check := flags.Bool("check", false, "only report the latest release")
	cfg, err := config.Parse(flags, args)
	if err != nil {
		return err
	}
	u := update.New(cfg.KernelPath())
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
	if err := u.Apply(ctx, s.Latest); err != nil {
		return err
	}
	fmt.Printf("Installed runplane %s.\n", s.Latest)
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		fmt.Println("Restart runplane to run the new version.")
		return nil
	}
	// try-restart restarts only the units that run.
	out, err := exec.CommandContext(ctx, "systemctl", "try-restart", "runplane.service", "runplane-agent.service").CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart runplane: %w: %s", err, out)
	}
	fmt.Println("runplane restarted.")
	return nil
}

// resetPassword gives a user a new random password and ends the sessions
// of the user.
func resetPassword(ctx context.Context, args []string) error {
	config.LoadEnvFile()
	flags := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	cfg, err := config.Parse(flags, args)
	if err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("give the email address of the user: runplane reset-password <email>")
	}
	if !exists(cfg.DBPath()) {
		return fmt.Errorf("no database at %s", cfg.DBPath())
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	u, err := st.GetUserByEmail(ctx, flags.Arg(0))
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no user has the email address %s", flags.Arg(0))
	}
	if err != nil {
		return err
	}
	password := rand.Text()
	if err := st.SetUserPassword(ctx, u.ID, auth.HashPassword(password)); err != nil {
		return err
	}
	if err := st.DeleteUserSessions(ctx, u.ID); err != nil {
		return err
	}
	fmt.Println(password)
	return nil
}

// setMode stores the sandbox mode in the settings of the primary. After the
// first start, the settings win over RUNPLANE_MODE, so install.sh calls
// setMode for --mode. The mode applies when runplane starts again.
func setMode(ctx context.Context, args []string) error {
	config.LoadEnvFile()
	flags := flag.NewFlagSet("set-mode", flag.ContinueOnError)
	cfg, err := config.Parse(flags, args)
	if err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("give the mode: runplane set-mode container|vm|dev")
	}
	mode := sandbox.Mode(flags.Arg(0))
	switch mode {
	case sandbox.ModeContainer:
	case sandbox.ModeVM:
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("VM mode needs KVM: %w", err)
		}
		f.Close()
	case sandbox.ModeDev:
		if !cfg.DevInsecure {
			return errDevInsecure
		}
	default:
		return fmt.Errorf("mode %q: use container, vm, or dev", mode)
	}
	if !exists(cfg.DBPath()) {
		return fmt.Errorf("no database at %s", cfg.DBPath())
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = st.UpdateSettings(ctx, func(s *store.Settings) error {
		s.Mode = mode
		// Container mode always rejects public repositories, and the
		// dashboard refuses to save the settings while this one is on.
		if mode == sandbox.ModeContainer {
			s.AllowPublicRepos = false
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("runplane uses %s mode after its next start.\n", mode)
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

func readSettings(ctx context.Context, cfg config.Config) (store.Settings, error) {
	st, err := store.Open(cfg.DBPath())
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
