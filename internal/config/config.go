// Package config reads the process configuration from the environment and
// flags, and gives the paths of all runplane files.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/runplane/runplane/internal/sandbox"
)

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

	// GitHubAPIURL and GitHubURL point runplane at another GitHub, for
	// example the fake GitHub of the tests. Empty means github.com.
	GitHubAPIURL string
	GitHubURL    string
	// DevRunnerDir is a runner bundle folder that every runner version
	// resolves to, unpatched. Tests set it to testdata/fakerunner.
	DevRunnerDir string
}

// Parse reads the environment, then the flags in args. Define the other flags
// of the subcommand on fs before the call.
func Parse(fs *flag.FlagSet, args []string) (Config, error) {
	c, err := Load()
	if err != nil {
		return Config{}, err
	}
	c.Bind(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Load reads the RUNPLANE_* environment variables over the defaults.
func Load() (Config, error) {
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
	str("RUNPLANE_GITHUB_API_URL", &c.GitHubAPIURL)
	str("RUNPLANE_GITHUB_URL", &c.GitHubURL)
	str("RUNPLANE_DEV_RUNNER_DIR", &c.DevRunnerDir)
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

// Bind adds flags to fs that override the values in c. The flag names are the
// variable names without RUNPLANE_, in lowercase, with dashes: -listen, -data-dir.
func (c *Config) Bind(fs *flag.FlagSet) {
	fs.StringVar(&c.Listen, "listen", c.Listen, "dashboard and API address (RUNPLANE_LISTEN)")
	fs.StringVar(&c.AgentListen, "agent-listen", c.AgentListen, "TLS address for agents (RUNPLANE_AGENT_LISTEN)")
	fs.IntVar(&c.ServicePort, "service-port", c.ServicePort, "cache and registry mirror port for sandboxes (RUNPLANE_SERVICE_PORT)")
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "data folder (RUNPLANE_DATA_DIR)")
	fs.StringVar(&c.ConfigDir, "config-dir", c.ConfigDir, "configuration folder (RUNPLANE_CONFIG_DIR)")
	fs.StringVar((*string)(&c.Mode), "mode", string(c.Mode), "sandbox mode: container, vm, or dev (RUNPLANE_MODE)")
	fs.BoolVar(&c.Dev, "dev", c.Dev, "relaxed cookies and CORS for the Vite dev server (RUNPLANE_DEV)")
	fs.BoolVar(&c.DevInsecure, "dev-insecure", c.DevInsecure, "let the devdocker driver operate (RUNPLANE_DEV_INSECURE)")
	fs.StringVar(&c.SSHListen, "ssh-listen", c.SSHListen, "SSH gateway address for debug shells (RUNPLANE_SSH_LISTEN)")
	fs.StringVar(&c.GitHubAPIURL, "github-api-url", c.GitHubAPIURL, "GitHub REST API URL, for tests (RUNPLANE_GITHUB_API_URL)")
	fs.StringVar(&c.GitHubURL, "github-url", c.GitHubURL, "GitHub web URL, for tests (RUNPLANE_GITHUB_URL)")
	fs.StringVar(&c.DevRunnerDir, "dev-runner-dir", c.DevRunnerDir, "runner bundle folder for all runner versions, for tests (RUNPLANE_DEV_RUNNER_DIR)")
}

// Validate checks the values and makes the folders absolute, because
// drivers give them to Docker and Firecracker as mount sources.
func (c *Config) Validate() error {
	switch c.Mode {
	case "", sandbox.ModeContainer, sandbox.ModeVM, sandbox.ModeDev:
	default:
		return fmt.Errorf("mode %q: use container, vm, or dev", c.Mode)
	}
	if c.ServicePort < 1 || c.ServicePort > 65535 {
		return fmt.Errorf("service port %d: use 1 to 65535", c.ServicePort)
	}
	for _, u := range []*string{&c.GitHubAPIURL, &c.GitHubURL} {
		if *u == "" {
			continue
		}
		p, err := url.Parse(*u)
		if err != nil || p.Host == "" || (p.Scheme != "http" && p.Scheme != "https") {
			return fmt.Errorf("GitHub URL %q: use an http or https URL", *u)
		}
		*u = strings.TrimRight(*u, "/")
	}
	dirs := []*string{&c.DataDir, &c.ConfigDir}
	if c.DevRunnerDir != "" {
		dirs = append(dirs, &c.DevRunnerDir)
	}
	for _, dir := range dirs {
		abs, err := filepath.Abs(*dir)
		if err != nil {
			return fmt.Errorf("resolve folder %q: %w", *dir, err)
		}
		*dir = abs
	}
	return nil
}
