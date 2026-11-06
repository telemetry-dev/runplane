package firecracker

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/sandbox/lifecycle"
)

const (
	// Slice holds the scopes of the VMs. install.sh writes its unit.
	Slice      = "runplane.slice"
	unitPrefix = "runplane-vm-"
)

func unitName(id string) string { return unitPrefix + id }

// serviceUnit returns the systemd service in a /proc/self/cgroup file, for
// example "runplane.service", or "" when the process is not in a service.
func serviceUnit(cgroup []byte) string {
	for line := range strings.Lines(string(cgroup)) {
		// cgroup v2: "0::/system.slice/runplane.service"
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok && strings.HasSuffix(p, ".service") {
			return filepath.Base(p)
		}
	}
	return ""
}

func (d *Driver) scopeDir(id string) string {
	return filepath.Join(d.cgroupRoot, Slice, unitName(id)+".scope")
}

func (d *Driver) scopeAlive(id string) bool {
	b, err := os.ReadFile(filepath.Join(d.scopeDir(id), "cgroup.procs"))
	return err == nil && len(bytes.TrimSpace(b)) > 0
}

// scopeIDs returns the sandbox IDs of the VM scopes in the runplane slice.
func (d *Driver) scopeIDs() []string {
	entries, err := os.ReadDir(filepath.Join(d.cgroupRoot, Slice))
	if err != nil && !os.IsNotExist(err) {
		d.log.Warn("read the VM scopes", "err", err)
	}
	var ids []string
	for _, e := range entries {
		id, unit := strings.CutPrefix(e.Name(), unitPrefix)
		id, scope := strings.CutSuffix(id, ".scope")
		if e.IsDir() && unit && scope {
			ids = append(ids, id)
		}
	}
	return ids
}

func readUsage(dir string) (lifecycle.Usage, error) {
	var u lifecycle.Usage
	cpu, err := readKeyed(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return u, err
	}
	stat, err := readKeyed(filepath.Join(dir, "memory.stat"))
	if err != nil {
		return u, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return u, err
	}
	if u.Memory, err = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err != nil {
		return u, fmt.Errorf("read memory.current: %w", err)
	}
	u.CPU = time.Duration(cpu["usage_usec"]) * time.Microsecond
	u.Cache = int64(stat["inactive_file"])
	return u, nil
}

// readKeyed reads a cgroup file of "key value" lines.
func readKeyed(path string) (map[string]uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]uint64{}
	for line := range strings.Lines(string(b)) {
		k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			m[k] = n
		}
	}
	return m, nil
}
