// Package config reads the process configuration from the flags, the
// environment, and runplane.env, and gives the paths of all runplane files.
package config

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"net"
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
	Dev         bool         // debug logs, and relaxed cookies and CORS for the Vite dev server
	DevInsecure bool         // lets the devdocker driver operate
	SSHListen   string       // SSH gateway for debug shells (primary)
}

// Default is the configuration without flags, environment, or
// runplane.env. install.sh writes the same ports and folders to
// runplane.env.
func Default() Config {
	return Config{
		Listen:      ":8484",
		AgentListen: ":8485",
		ServicePort: 8486,
		DataDir:     "/var/lib/runplane",
		ConfigDir:   "/etc/runplane",
		SSHListen:   ":2222",
	}
}

// Flag is a flag of the configuration that a command uses. Each command
// also has -config-dir, which selects runplane.env.
type Flag int

const (
	Listen Flag = iota
	AgentListen
	ServicePort
	DataDir
	Mode
	Dev
	DevInsecure
	SSHListen
	configDir
)

// option is one value of Config. Its variable is RUNPLANE_ and the flag
// name in upper case, with _ for -.
type option struct {
	name  string
	usage string
	field func(*Config) any // *string, *int, or *bool
}

var options = [...]option{
	Listen:      {"listen", "dashboard and API address", func(c *Config) any { return &c.Listen }},
	AgentListen: {"agent-listen", "TLS address for agents", func(c *Config) any { return &c.AgentListen }},
	ServicePort: {"service-port", "cache and registry mirror port for sandboxes", func(c *Config) any { return &c.ServicePort }},
	DataDir:     {"data-dir", "data folder", func(c *Config) any { return &c.DataDir }},
	Mode:        {"mode", "sandbox mode: container, vm, or dev", func(c *Config) any { return (*string)(&c.Mode) }},
	Dev:         {"dev", "debug logs, and relaxed cookies and CORS for the Vite dev server", func(c *Config) any { return &c.Dev }},
	DevInsecure: {"dev-insecure", "let the devdocker driver operate", func(c *Config) any { return &c.DevInsecure }},
	SSHListen:   {"ssh-listen", "SSH gateway address for debug shells", func(c *Config) any { return &c.SSHListen }},
	configDir:   {"config-dir", "configuration folder, with runplane.env", func(c *Config) any { return &c.ConfigDir }},
}

// Parse reads the configuration of a command. A flag wins over the
// RUNPLANE_* variable of the environment, the environment wins over
// runplane.env in the configuration folder, and runplane.env wins over
// Default. systemd gives the services the variables of runplane.env, but
// sudo starts a command without them, so the file gives an operator command
// the configuration of the service.
//
// fs gets -config-dir and the flags in use. Define the other flags of the
// command on fs before the call.
func Parse(fs *flag.FlagSet, args []string, use ...Flag) (Config, error) {
	def := Default()
	for _, f := range append(use, configDir) {
		o := options[f]
		usage := fmt.Sprintf("%s (%s)", o.usage, o.env())
		switch p := o.field(&def).(type) {
		case *string:
			fs.String(o.name, *p, usage)
		case *int:
			fs.Int(o.name, *p, usage)
		case *bool:
			fs.Bool(o.name, *p, usage)
		}
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	flags := map[string]string{}
	fs.Visit(func(f *flag.Flag) { flags[f.Name] = f.Value.String() })

	dir := cmp.Or(flags["config-dir"], os.Getenv("RUNPLANE_CONFIG_DIR"), def.ConfigDir)
	envFile := Config{ConfigDir: dir}.EnvFilePath()
	file := readEnvFile(envFile)
	c := def
	var errs []error
	for _, o := range options {
		v, set := flags[o.name]
		from := "-" + o.name
		switch {
		case set:
		case os.Getenv(o.env()) != "":
			v, from = os.Getenv(o.env()), o.env()
		case file[o.env()] != "":
			v, from = file[o.env()], o.env()+" in "+envFile
		default:
			continue
		}
		if err := o.set(&c, v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", from, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	if err := c.check(); err != nil {
		return Config{}, err
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

func (o option) env() string {
	return "RUNPLANE_" + strings.ToUpper(strings.ReplaceAll(o.name, "-", "_"))
}

func (o option) set(c *Config, v string) error {
	switch p := o.field(c).(type) {
	case *string:
		*p = v
	case *int:
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%q is not a number", v)
		}
		*p = n
	case *bool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%q is not 0 or 1", v)
		}
		*p = b
	}
	return nil
}

func (c Config) check() error {
	if c.Mode != "" {
		if err := c.Mode.Check(); err != nil {
			return err
		}
	}
	if c.ServicePort < 1 || c.ServicePort > 65535 {
		return fmt.Errorf("service port %d: use 1 to 65535", c.ServicePort)
	}
	for _, addr := range []string{c.Listen, c.AgentListen, c.SSHListen} {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return fmt.Errorf("listen address %q: use host:port or :port", addr)
		}
	}
	return nil
}

// readEnvFile reads the RUNPLANE_* variables of runplane.env, the
// environment file of the systemd units. A file that is missing or that the
// user cannot read has none.
func readEnvFile(path string) map[string]string {
	b, _ := os.ReadFile(path)
	vars := map[string]string{}
	for line := range strings.Lines(string(b)) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || !strings.HasPrefix(key, "RUNPLANE_") {
			continue
		}
		// systemd also takes a value in quotes.
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		vars[key] = value
	}
	return vars
}
