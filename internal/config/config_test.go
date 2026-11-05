package config

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestParseEnvAndFlags(t *testing.T) {
	t.Setenv("RUNPLANE_LISTEN", ":9000")
	t.Setenv("RUNPLANE_SERVICE_PORT", "9100")
	t.Setenv("RUNPLANE_DATA_DIR", "rel/data")
	t.Setenv("RUNPLANE_MODE", "vm")
	t.Setenv("RUNPLANE_DEV", "1")

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	c, err := Parse(fs, []string{"-listen", ":9001", "-mode", "container"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9001" || c.Mode != "container" {
		t.Errorf("flags did not override the environment: %+v", c)
	}
	if c.ServicePort != 9100 || !c.Dev || c.AgentListen != ":8485" || c.DevInsecure {
		t.Errorf("environment or defaults lost: %+v", c)
	}
	wd, _ := os.Getwd()
	// Docker bind-mounts the data folder, so it must be absolute.
	if c.DataDir != filepath.Join(wd, "rel/data") || c.ConfigDir != "/etc/runplane" {
		t.Errorf("folders: %q, %q", c.DataDir, c.ConfigDir)
	}
	if c.SSHListen != ":2222" {
		t.Errorf("SSH address: %q", c.SSHListen)
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
	} {
		t.Run(tt.name+"="+tt.value, func(t *testing.T) {
			t.Setenv(tt.name, tt.value)
			fs := flag.NewFlagSet("serve", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			if _, err := Parse(fs, nil); (err == nil) != tt.ok {
				t.Errorf("err = %v, want ok %v", err, tt.ok)
			}
		})
	}
}

func TestEnsureDirs(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	root := t.TempDir()
	c := Config{DataDir: filepath.Join(root, "data"), ConfigDir: filepath.Join(root, "etc")}
	// A folder that exists with a loose mode gets the right mode.
	if err := os.MkdirAll(c.CacheDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.CacheDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		c.ConfigDir: 0o700, c.DataDir: 0o700, c.SandboxesDir(): 0o700, c.ImagesDir(): 0o700,
		c.RunnersDir(): 0o755, c.CacheDir(): 0o700, c.MirrorDir(): 0o700, c.LogsDir(): 0o700,
		c.BackupsDir(): 0o700, c.TLSDir(): 0o700, c.ScratchTemplatesDir(): 0o700,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v", path, info.Mode().Perm(), want)
		}
	}
}

// sudo runplane doctor gets no RUNPLANE_* variables, so it must read the ones
// of the service from runplane.env. The real environment still wins.
func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNPLANE_CONFIG_DIR", dir)
	t.Setenv("RUNPLANE_LISTEN", ":7000")
	// LoadEnvFile sets these; t.Setenv restores them after the test.
	for _, key := range []string{"RUNPLANE_MODE", "RUNPLANE_DATA_DIR", "RUNPLANE_SSH_LISTEN", "NOT_RUNPLANE"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	env := "# Environment of runplane-agent.service\n" +
		"RUNPLANE_MODE=vm\n" +
		"RUNPLANE_LISTEN=:9000\n" +
		"RUNPLANE_DATA_DIR=\"/srv/runplane\"\n" +
		"NOT_RUNPLANE=1\n" +
		"RUNPLANE_SSH_LISTEN=:2200" // no newline at the end
	if err := os.WriteFile(filepath.Join(dir, "runplane.env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}

	LoadEnvFile()
	c, err := Parse(flag.NewFlagSet("doctor", flag.ContinueOnError), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != "vm" || c.DataDir != "/srv/runplane" || c.SSHListen != ":2200" {
		t.Errorf("values of the file lost: mode %q, data %q, ssh %q", c.Mode, c.DataDir, c.SSHListen)
	}
	if c.Listen != ":7000" {
		t.Errorf("listen %q, want :7000 from the environment", c.Listen)
	}
	if _, set := os.LookupEnv("NOT_RUNPLANE"); set {
		t.Error("the file set a variable that is not RUNPLANE_*")
	}
}
