package config

import (
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

var allFlags = []Flag{Listen, AgentListen, ServicePort, DataDir, Mode, Dev, DevInsecure, SSHListen}

// cleanEnv clears the RUNPLANE_* variables of the shell, and points Parse
// to a configuration folder without runplane.env, so that the environment
// and the runplane.env of this machine have no effect.
func cleanEnv(t *testing.T) string {
	t.Helper()
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); strings.HasPrefix(key, "RUNPLANE_") {
			t.Setenv(key, "")
		}
	}
	dir := t.TempDir()
	t.Setenv("RUNPLANE_CONFIG_DIR", dir)
	return dir
}

func TestParseEnvAndFlags(t *testing.T) {
	dir := cleanEnv(t)
	t.Setenv("RUNPLANE_LISTEN", ":9000")
	t.Setenv("RUNPLANE_SERVICE_PORT", "9100")
	t.Setenv("RUNPLANE_DATA_DIR", "rel/data")
	t.Setenv("RUNPLANE_MODE", "vm")
	t.Setenv("RUNPLANE_DEV", "1")

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	c, err := Parse(fs, []string{"-listen", ":9001", "-mode", "container"}, allFlags...)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9001" || c.Mode != "container" {
		t.Errorf("flags did not override the environment: %+v", c)
	}
	def := Default()
	if c.ServicePort != 9100 || !c.Dev || c.AgentListen != def.AgentListen || c.SSHListen != def.SSHListen || c.DevInsecure {
		t.Errorf("environment or defaults lost: %+v", c)
	}
	wd, _ := os.Getwd()
	// Docker bind-mounts the data folder, so it must be absolute.
	if c.DataDir != filepath.Join(wd, "rel/data") || c.ConfigDir != dir {
		t.Errorf("folders: %q, %q", c.DataDir, c.ConfigDir)
	}
}

func TestParseValues(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		ok          bool
	}{
		{"RUNPLANE_MODE", "container", true},
		{"RUNPLANE_MODE", "vm", true},
		{"RUNPLANE_MODE", "dev", true},
		{"RUNPLANE_MODE", "kvm", false},
		{"RUNPLANE_DEV", "yes please", false},
		{"RUNPLANE_SERVICE_PORT", "http", false},
		{"RUNPLANE_SERVICE_PORT", "70000", false},
		{"RUNPLANE_SSH_LISTEN", "127.0.0.1:2200", true},
		{"RUNPLANE_SSH_LISTEN", "2200", false},
	} {
		t.Run(tt.name+"="+tt.value, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv(tt.name, tt.value)
			fs := flag.NewFlagSet("serve", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			if _, err := Parse(fs, nil); (err == nil) != tt.ok {
				t.Errorf("err = %v, want ok %v", err, tt.ok)
			}
		})
	}
}

// A flag that the command does not use is an error, not a value that has
// no effect.
func TestParseHasOnlyTheFlagsInUse(t *testing.T) {
	cleanEnv(t)
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if _, err := Parse(fs, []string{"-ssh-listen", ":2200"}, DataDir); err == nil {
		t.Error("reset-password takes -ssh-listen")
	}
	fs = flag.NewFlagSet("serve", flag.ContinueOnError)
	if c, err := Parse(fs, []string{"-ssh-listen", ":2200"}, DataDir, SSHListen); err != nil || c.SSHListen != ":2200" {
		t.Errorf("serve with -ssh-listen: %q, %v", c.SSHListen, err)
	}
}

// The folders hold keys, password hashes, backups, and job logs, so only
// root can open them. The runners folder is the exception: the sandboxes
// read the runner bundles as other users. EnsureDirs also corrects the mode
// of a folder that exists, because MkdirAll keeps it.
func TestEnsureDirs(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	root := t.TempDir()
	c := Config{DataDir: filepath.Join(root, "data"), ConfigDir: filepath.Join(root, "etc")}
	if err := os.MkdirAll(c.CacheDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.CacheDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	modes := map[string]fs.FileMode{}
	for _, top := range []string{c.ConfigDir, c.DataDir} {
		err := filepath.WalkDir(top, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			modes[path] = info.Mode().Perm()
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for path, mode := range modes {
		want := fs.FileMode(0o700)
		if path == c.RunnersDir() {
			want = 0o755
		}
		if mode != want {
			t.Errorf("%s: mode %v, want %v", path, mode, want)
		}
	}
	for _, path := range []string{c.SandboxesDir(), c.ImagesDir(), c.RunnersDir(), c.CacheDir(), c.MirrorDir(),
		c.LogsDir(), c.BackupsDir(), c.TLSDir(), c.ScratchTemplatesDir()} {
		if _, ok := modes[path]; !ok {
			t.Errorf("EnsureDirs made no folder %s", path)
		}
	}
}

// sudo runplane doctor gets no RUNPLANE_* variables, so it must read the ones
// of the service from runplane.env in the configuration folder of
// -config-dir or RUNPLANE_CONFIG_DIR. The real environment and the flags
// still win.
func TestParseReadsTheEnvFile(t *testing.T) {
	dir := t.TempDir()
	env := "# Environment of runplane-agent.service\n" +
		"RUNPLANE_MODE=vm\n" +
		"RUNPLANE_LISTEN=:9000\n" +
		"RUNPLANE_DATA_DIR=\"/srv/runplane\"\n" +
		"RUNPLANE_AGENT_LISTEN=:9100\n" +
		"NOT_RUNPLANE=1\n" +
		"RUNPLANE_SSH_LISTEN=:2200" // no newline at the end
	if err := os.WriteFile(filepath.Join(dir, "runplane.env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		env  string // RUNPLANE_CONFIG_DIR
		args []string
	}{
		{"-config-dir", t.TempDir(), []string{"-config-dir", dir, "-agent-listen", ":9200"}},
		{"RUNPLANE_CONFIG_DIR", dir, []string{"-agent-listen", ":9200"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("RUNPLANE_CONFIG_DIR", tt.env)
			t.Setenv("RUNPLANE_LISTEN", ":7000")
			c, err := Parse(flag.NewFlagSet("doctor", flag.ContinueOnError), tt.args, allFlags...)
			if err != nil {
				t.Fatal(err)
			}
			if c.Mode != "vm" || c.DataDir != "/srv/runplane" || c.SSHListen != ":2200" || c.ConfigDir != dir {
				t.Errorf("values of the file lost: mode %q, data %q, ssh %q, config %q", c.Mode, c.DataDir, c.SSHListen, c.ConfigDir)
			}
			if c.Listen != ":7000" {
				t.Errorf("listen %q, want :7000 from the environment", c.Listen)
			}
			if c.AgentListen != ":9200" {
				t.Errorf("agent listen %q, want :9200 from the flag", c.AgentListen)
			}
		})
	}
}
