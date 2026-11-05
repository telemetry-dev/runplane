package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func (c Config) SecretKeyPath() string   { return filepath.Join(c.ConfigDir, "secret.key") }
func (c Config) AgentConfigPath() string { return filepath.Join(c.ConfigDir, "agent.json") }

// AdminKeysPath is the authorized_keys file of the admins: these keys open
// the debug shell of every sandbox through the SSH gateway.
func (c Config) AdminKeysPath() string { return filepath.Join(c.ConfigDir, "authorized_keys") }

func (c Config) DBPath() string              { return filepath.Join(c.DataDir, "runplane.db") }
func (c Config) SandboxesDir() string        { return filepath.Join(c.DataDir, "sandboxes") }
func (c Config) SandboxDir(id string) string { return filepath.Join(c.SandboxesDir(), id) }
func (c Config) ImagesDir() string           { return filepath.Join(c.DataDir, "images") }
func (c Config) RunnersDir() string          { return filepath.Join(c.DataDir, "runners") }
func (c Config) CacheDir() string            { return filepath.Join(c.DataDir, "cache") }
func (c Config) MirrorDir() string           { return filepath.Join(c.DataDir, "mirror") }
func (c Config) LogsDir() string             { return filepath.Join(c.DataDir, "logs") }
func (c Config) BackupsDir() string          { return filepath.Join(c.DataDir, "backups") }
func (c Config) TLSDir() string              { return filepath.Join(c.DataDir, "tls") }

// JobLogPath is the stored GitHub log of a completed job.
func (c Config) JobLogPath(jobID string) string { return filepath.Join(c.LogsDir(), jobID+".log") }

// ScratchTemplatesDir holds the empty ext4 templates of the scratch disks.
func (c Config) ScratchTemplatesDir() string { return filepath.Join(c.DataDir, "scratch-templates") }

// KernelPath is the guest kernel of VM mode. install.sh puts it there.
func (c Config) KernelPath() string { return filepath.Join(c.DataDir, "kernel", "vmlinux") }

// SSHHostKeyPath is the host key of the SSH gateway.
func (c Config) SSHHostKeyPath() string { return filepath.Join(c.DataDir, "ssh_host_ed25519_key") }

// EnsureDirs makes the configuration and data folders and sets their modes.
// It also corrects the mode of folders that exist.
func (c Config) EnsureDirs() error {
	dirs := []struct {
		path string
		mode fs.FileMode
	}{
		{c.ConfigDir, 0o700},
		{c.DataDir, 0o700},
		{c.SandboxesDir(), 0o700},
		{c.ImagesDir(), 0o700},
		{c.RunnersDir(), 0o755},
		{c.CacheDir(), 0o700},
		{c.MirrorDir(), 0o700},
		{c.LogsDir(), 0o700},
		{c.BackupsDir(), 0o700},
		{c.TLSDir(), 0o700},
		{c.ScratchTemplatesDir(), 0o700},
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return fmt.Errorf("make folder: %w", err)
		}
		// MkdirAll applies the umask and skips folders that exist.
		if err := os.Chmod(d.path, d.mode); err != nil {
			return fmt.Errorf("set folder mode: %w", err)
		}
	}
	return nil
}
