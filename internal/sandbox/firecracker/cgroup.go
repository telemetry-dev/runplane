package firecracker

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

const (
	slice        = "runplane.slice"
	metricsEvery = 5 * time.Second
)

func unitName(id string) string { return "runplane-vm-" + id }

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

// scopeDir is the cgroup of the systemd scope of a VM.
func (d *Driver) scopeDir(id string) string {
	return filepath.Join(d.cgroupRoot, slice, unitName(id)+".scope")
}

// scopeAlive reports if a process operates in the scope of a VM.
func (d *Driver) scopeAlive(id string) bool {
	b, err := os.ReadFile(filepath.Join(d.scopeDir(id), "cgroup.procs"))
	return err == nil && len(bytes.TrimSpace(b)) > 0
}

// scopeIDs returns the sandbox IDs of the VM scopes in the runplane slice.
func (d *Driver) scopeIDs() []string {
	entries, _ := os.ReadDir(filepath.Join(d.cgroupRoot, slice))
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() && strings.HasPrefix(name, "runplane-vm-") && strings.HasSuffix(name, ".scope") {
			ids = append(ids, strings.TrimSuffix(strings.TrimPrefix(name, "runplane-vm-"), ".scope"))
		}
	}
	return ids
}

// sampleLoop sends the metrics of the busy VMs.
func (d *Driver) sampleLoop() {
	t := time.NewTicker(metricsEvery)
	defer t.Stop()
	for {
		select {
		case <-d.quit:
			return
		case now := <-t.C:
			d.sample(now)
		}
	}
}

func (d *Driver) sample(now time.Time) {
	for _, v := range d.all() {
		if v.getState() != sandbox.StateBusy {
			continue
		}
		u, err := readUsage(d.scopeDir(v.id))
		if err != nil {
			d.log.Debug("read VM usage", "sandbox", v.id, "err", err)
			continue
		}
		if m, ok := v.metrics(u, now); ok {
			d.emit(sandbox.Event{SandboxID: v.id, Type: sandbox.EventMetrics, Metrics: &m})
		}
	}
}

// usage is one reading of the cgroup of a VM.
type usage struct {
	cpuUsec  uint64 // CPU time since the start
	memBytes int64  // memory without inactive file cache, as docker stats shows it
}

func readUsage(dir string) (usage, error) {
	var u usage
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
	current, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return u, fmt.Errorf("read memory.current: %w", err)
	}
	u.cpuUsec = cpu["usage_usec"]
	u.memBytes = max(current-int64(stat["inactive_file"]), 0)
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

// cpuSample is the CPU time of a VM at one moment.
type cpuSample struct {
	at   time.Time
	usec uint64
}

// metrics turns a reading into metrics. The CPU percent needs the previous
// reading, so the first reading gives none.
func (v *vm) metrics(u usage, now time.Time) (sandbox.Metrics, bool) {
	v.mu.Lock()
	prev := v.cpu
	v.cpu = cpuSample{at: now, usec: u.cpuUsec}
	v.mu.Unlock()
	wall := now.Sub(prev.at).Microseconds()
	if prev.at.IsZero() || wall <= 0 || u.cpuUsec < prev.usec {
		return sandbox.Metrics{}, false
	}
	pct := float64(u.cpuUsec-prev.usec) * 100 / float64(wall*int64(v.Spec.VCPU))
	return sandbox.Metrics{CPUPercent: min(pct, 100), MemoryMB: int(u.memBytes >> 20)}, true
}
