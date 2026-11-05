// Package config reads the process configuration from the environment and
// flags, and gives the paths of all runplane files.
package config

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/runplane/runplane/internal/sandbox"
)

const defaultConfigDir = "/etc/runplane"

type Config struct {
	Listen      string // dashboard and API
	AgentListen string // TLS listener for agents (primary)
	ServicePort int    // cache server and registry mirror for sandboxes
	DataDir     string
	ConfigDir   string
	Mode        sandbox.Mode // empty until the install sets it
	Dev         bool         // relaxed cookies and CORS for the Vite dev server
	DevInsecure bool         // lets the devdocker driver operate
	SSHListen   string       // SSH gateway for debug shells (primary)
}

// Parse reads the RUNPLANE_* environment variables over the defaults, then
// the flags in args. Define the other flags of the subcommand on fs before
// the call. The flag names are the variable names without RUNPLANE_, in
// lowercase, with dashes: -listen, -data-dir.
func Parse(fs *flag.FlagSet, args []string) (Config, error) {
	c, err := load()
	if err != nil {
		return Config{}, err
	}
	fs.StringVar(&c.Listen, "listen", c.Listen, "dashboard and API address (RUNPLANE_LISTEN)")
	fs.StringVar(&c.AgentListen, "agent-listen", c.AgentListen, "TLS address for agents (RUNPLANE_AGENT_LISTEN)")
	fs.IntVar(&c.ServicePort, "service-port", c.ServicePort, "cache and registry mirror port for sandboxes (RUNPLANE_SERVICE_PORT)")
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "data folder (RUNPLANE_DATA_DIR)")
	fs.StringVar(&c.ConfigDir, "config-dir", c.ConfigDir, "configuration folder (RUNPLANE_CONFIG_DIR)")
	fs.StringVar((*string)(&c.Mode), "mode", string(c.Mode), "sandbox mode: container, vm, or dev (RUNPLANE_MODE)")
	fs.BoolVar(&c.Dev, "dev", c.Dev, "relaxed cookies and CORS for the Vite dev server (RUNPLANE_DEV)")
	fs.BoolVar(&c.DevInsecure, "dev-insecure", c.DevInsecure, "let the devdocker driver operate (RUNPLANE_DEV_INSECURE)")
	fs.StringVar(&c.SSHListen, "ssh-listen", c.SSHListen, "SSH gateway address for debug shells (RUNPLANE_SSH_LISTEN)")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	switch c.Mode {
	case "", sandbox.ModeContainer, sandbox.ModeVM, sandbox.ModeDev:
	default:
		return Config{}, fmt.Errorf("mode %q: use container, vm, or dev", c.Mode)
	}
	if c.ServicePort < 1 || c.ServicePort > 65535 {
		return Config{}, fmt.Errorf("service port %d: use 1 to 65535", c.ServicePort)
	}
	// Drivers give the folders to Docker and Firecracker as mount sources,
	// so they must be absolute.
	for _, dir := range []*string{&c.DataDir, &c.ConfigDir} {
		abs, err := filepath.Abs(*dir)
		if err != nil {
			return Config{}, fmt.Errorf("resolve folder %q: %w", *dir, err)
		}
		*dir = abs
	}
	return c, nil
}

func load() (Config, error) {
	c := Config{
		Listen:      ":8484",
		AgentListen: ":8485",
		ServicePort: 8486,
		DataDir:     "/var/lib/runplane",
		ConfigDir:   defaultConfigDir,
		SSHListen:   ":2222",
	}
	var errs []error
	str := func(name string, dst *string) {
		if v := os.Getenv(name); v != "" {
			*dst = v
		}
	}
	boolean := func(name string, dst *bool) {
		v := os.Getenv(name)
		if v == "" {
			return
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not 0 or 1", name, v))
			return
		}
		*dst = b
	}
	str("RUNPLANE_LISTEN", &c.Listen)
	str("RUNPLANE_AGENT_LISTEN", &c.AgentListen)
	str("RUNPLANE_DATA_DIR", &c.DataDir)
	str("RUNPLANE_CONFIG_DIR", &c.ConfigDir)
	str("RUNPLANE_MODE", (*string)(&c.Mode))
	str("RUNPLANE_SSH_LISTEN", &c.SSHListen)
	boolean("RUNPLANE_DEV", &c.Dev)
	boolean("RUNPLANE_DEV_INSECURE", &c.DevInsecure)
	if v := os.Getenv("RUNPLANE_SERVICE_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("RUNPLANE_SERVICE_PORT: %q is not a number", v))
		}
		c.ServicePort = port
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("read environment: %w", err)
	}
	return c, nil
}

// LoadEnvFile sets the RUNPLANE_* variables of runplane.env, the environment
// file of the systemd units, that the environment does not have. sudo starts
// a command without them, so the commands that an operator runs by hand call
// LoadEnvFile before Parse to see the configuration of the service. It reads
// the file in RUNPLANE_CONFIG_DIR, or in /etc/runplane. A file that is
// missing or that the user cannot read changes nothing.
func LoadEnvFile() {
	dir := cmp.Or(os.Getenv("RUNPLANE_CONFIG_DIR"), defaultConfigDir)
	b, _ := os.ReadFile(filepath.Join(dir, "runplane.env"))
	for line := range strings.Lines(string(b)) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || !strings.HasPrefix(key, "RUNPLANE_") {
			continue
		}
		if _, set := os.LookupEnv(key); set {
			continue
		}
		// systemd also takes a value in quotes.
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		os.Setenv(key, value)
	}
}
