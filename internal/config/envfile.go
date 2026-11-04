package config

import (
	"cmp"
	"os"
	"path/filepath"
	"strings"
)

const defaultConfigDir = "/etc/runplane"

// LoadEnvFile sets the RUNPLANE_* variables of runplane.env, the environment
// file of the systemd units, that the environment does not have. sudo starts
// a command without them, so the commands that an operator runs by hand call
// LoadEnvFile before Parse to see the configuration of the service. It reads
// the file in RUNPLANE_CONFIG_DIR, or in /etc/runplane. A file that is
// missing or that the user cannot read changes nothing.
func LoadEnvFile() {
	dir := cmp.Or(os.Getenv("RUNPLANE_CONFIG_DIR"), defaultConfigDir)
	b, err := os.ReadFile(filepath.Join(dir, "runplane.env"))
	if err != nil {
		return
	}
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
