// Package sandbox defines the contract between the scheduler and the drivers
// that operate sandboxes (container mode, VM mode, remote agents).
package sandbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"time"
)

type Mode string

const (
	ModeContainer Mode = "container"
	ModeVM        Mode = "vm"
	ModeDev       Mode = "dev"
)

type State string

const (
	StateStarting State = "starting" // sandbox starts, runner not connected yet
	StateIdle     State = "idle"     // runner listens for a job (warm sandbox)
	StateBusy     State = "busy"     // runner has a job
	StateStopping State = "stopping"
	StateGone     State = "gone"   // removed
	StateFailed   State = "failed" // start or runtime failure; the driver then removes the sandbox
)

// Done reports if the sandbox no longer operates.
func (s State) Done() bool { return s == StateGone || s == StateFailed }

type Spec struct {
	ID            string            `json:"id"`
	PoolID        string            `json:"pool_id"`
	Image         ImageRef          `json:"image"`
	RunnerVersion string            `json:"runner_version"`
	Arch          string            `json:"arch"` // "x64" or "arm64"
	VCPU          int               `json:"vcpu"`
	MemoryMB      int               `json:"memory_mb"`
	DiskGB        int               `json:"disk_gb"`
	TimeLimit     time.Duration     `json:"time_limit"`           // from the job start; see Deadline
	JITConfig     string            `json:"jit_config,omitempty"` // never written to disk
	Env           map[string]string `json:"env,omitempty"`
	Debug         bool              `json:"debug,omitempty"`
}

// IdleGrace is how much longer than its time limit a sandbox may wait for
// a job. The scheduler replaces a waiting sandbox at the time limit, after
// it removes the runner from GitHub; the guest and the drivers stop it at
// the end of the grace.
const IdleGrace = 5 * time.Minute

// Deadline is when a sandbox passes its time limit: limit after the job
// started, or limit plus IdleGrace after the sandbox started while it waits
// for a job. A zero jobStarted means no job yet.
func Deadline(created, jobStarted time.Time, limit time.Duration) time.Time {
	if !jobStarted.IsZero() {
		return jobStarted.Add(limit)
	}
	return created.Add(limit + IdleGrace)
}

type ImageRef struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

type Sandbox struct {
	ID        string    `json:"id"`
	PoolID    string    `json:"pool_id"`
	State     State     `json:"state"`
	IP        string    `json:"ip"`
	CreatedAt time.Time `json:"created_at"`
	Spec      Spec      `json:"spec"` // JITConfig is empty after start
}

// FindByIP returns the sandbox of list that operates at addr. An
// IPv4-mapped IPv6 address matches its IPv4 form.
func FindByIP(list []Sandbox, addr netip.Addr) (Sandbox, bool) {
	addr = addr.Unmap()
	for _, s := range list {
		ip, err := netip.ParseAddr(s.IP)
		if err == nil && ip.Unmap() == addr && !s.State.Done() {
			return s, true
		}
	}
	return Sandbox{}, false
}

// Capacity is the size of a server, what its sandboxes use, and the limits
// of its driver.
type Capacity struct {
	Mode          Mode   `json:"mode"`
	Arch          string `json:"arch"`
	VCPU          int    `json:"vcpu"`
	MemoryMB      int    `json:"memory_mb"`
	DiskBytes     int64  `json:"disk_bytes"`
	FreeDiskBytes int64  `json:"free_disk_bytes"`
	AllocVCPU     int    `json:"alloc_vcpu"`
	AllocMemoryMB int    `json:"alloc_memory_mb"` // with OverheadMB for each sandbox
	Sandboxes     int    `json:"sandboxes"`
	// OverheadMB is the host memory that each sandbox uses on top of its
	// own memory, for example the Firecracker process of a VM.
	OverheadMB   int `json:"overhead_mb"`
	MaxVCPU      int `json:"max_vcpu"`      // of one sandbox; 0 = no limit
	MaxSandboxes int `json:"max_sandboxes"` // 0 = no limit
}

// Reserve is what the dashboard settings keep from the sandboxes of each
// server: vCPUs and memory for the host, and the CPU overcommit.
type Reserve struct {
	VCPU       int
	MemoryMB   int
	Overcommit float64 // 0 means 1
}

// FreeMemoryMB is the memory that new sandboxes can still get. Memory is
// never overcommitted.
func (c Capacity) FreeMemoryMB(r Reserve) int {
	return c.MemoryMB - r.MemoryMB - c.AllocMemoryMB
}

// Room counts the new sandboxes of this size that can start now.
func (c Capacity) Room(vcpu, memoryMB int, r Reserve) int {
	if c.MaxVCPU > 0 && vcpu > c.MaxVCPU {
		return 0
	}
	overcommit := r.Overcommit
	if overcommit <= 0 {
		overcommit = 1
	}
	freeVCPU := int(float64(c.VCPU-r.VCPU)*overcommit) - c.AllocVCPU
	n := min(freeVCPU/max(vcpu, 1), c.FreeMemoryMB(r)/max(memoryMB+c.OverheadMB, 1))
	if c.MaxSandboxes > 0 {
		n = min(n, c.MaxSandboxes-c.Sandboxes)
	}
	return max(n, 0)
}

// FreeDiskPercent is the free part of the data disk, 0 to 100.
func (c Capacity) FreeDiskPercent() float64 {
	if c.DiskBytes <= 0 {
		return 0
	}
	return float64(c.FreeDiskBytes) * 100 / float64(c.DiskBytes)
}

type EventType string

const (
	EventState   EventType = "state"   // State and Reason are set
	EventRunner  EventType = "runner"  // Runner is set
	EventMetrics EventType = "metrics" // Metrics is set
)

type Event struct {
	SandboxID string       `json:"sandbox_id"`
	Type      EventType    `json:"type"`
	Time      time.Time    `json:"time"`
	State     State        `json:"state,omitempty"`
	Reason    string       `json:"reason,omitempty"`
	Runner    *RunnerEvent `json:"runner,omitempty"`
	Metrics   *Metrics     `json:"metrics,omitempty"`
}

// Runner event kinds that runplane-init sends.
const (
	RunnerListening    = "listening"
	RunnerJobStarted   = "job.started"
	RunnerJobCompleted = "job.completed"
	RunnerExited       = "exited"
	RunnerResume       = "resume"
)

type RunnerEvent struct {
	Kind     string `json:"kind"`
	JobName  string `json:"job_name,omitempty"`
	Result   string `json:"result,omitempty"`    // for job.completed
	ExitCode int    `json:"exit_code,omitempty"` // for exited
	// Error is set for exited when the runner failed, for example "the
	// runner did not connect to GitHub". The sandbox then ends as failed.
	Error string `json:"error,omitempty"`
}

// State returns the state that the event moves its sandbox to, and the
// reason, or false if the event does not change the state.
func (e RunnerEvent) State() (State, string, bool) {
	switch e.Kind {
	case RunnerListening:
		return StateIdle, "", true
	case RunnerJobStarted:
		return StateBusy, "", true
	case RunnerExited:
		return StateStopping, cmp.Or(e.Error, fmt.Sprintf("the runner exited with code %d", e.ExitCode)), true
	}
	return "", "", false
}

type Metrics struct {
	CPUPercent float64 `json:"cpu_percent"` // of the sandbox vCPU allocation, 0 to 100
	MemoryMB   int     `json:"memory_mb"`
}

// Driver operates sandboxes on one server.
type Driver interface {
	Mode() Mode
	// Start returns when the sandbox process starts. The state is then
	// starting. A Start that fails sends no events and leaves no sandbox;
	// the caller records the end.
	Start(ctx context.Context, spec Spec) error
	// Stop stops the sandbox and returns when it is gone. It is idempotent.
	// When ctx ends first, Stop returns ctx.Err(), and the stop goes on.
	Stop(ctx context.Context, id string) error
	List(ctx context.Context) ([]Sandbox, error)
	Capacity(ctx context.Context) (Capacity, error)
	// Configure applies new settings while the driver operates.
	Configure(ctx context.Context, s Settings) error
	Events() <-chan Event
	// Logs returns the console and runner log of the sandbox.
	Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	Shell(ctx context.Context, id string, size TermSize) (Terminal, error)
	// Hold keeps a finished debug sandbox until the given time.
	Hold(ctx context.Context, id string, until time.Time) error
	// Probe is the sandbox internet check of the doctor: it fetches the
	// GitHub API through the network and the firewall of a sandbox.
	Probe(ctx context.Context) error
	Close() error
}

// ErrNotFound is the error of Logs, Shell, and Hold for a sandbox that the
// driver does not operate.
var ErrNotFound = errors.New("no such sandbox")

// ErrNoProbe means that the sandbox internet check cannot run yet.
var ErrNoProbe = errors.New("this server has no sandbox image yet")

// ErrProbeInService means that only the running service can do the sandbox
// internet check: in VM mode, the probe needs a firewall rule that only the
// service can add without a conflict.
var ErrProbeInService = errors.New("in VM mode, only the running runplane service can do this check, because it owns the firewall rules of the sandboxes")

// Settings are the dashboard settings that a driver obeys. The primary sends
// them to each server when they change.
type Settings struct {
	DNS             []string `json:"dns"`
	EgressAllowlist []string `json:"egress_allowlist"`
	BandwidthMbps   int      `json:"bandwidth_mbps"` // VM mode, 0 = no limit
	RegistryMirror  bool     `json:"registry_mirror"`
	ActionsCache    bool     `json:"actions_cache"`
}

// IPv4DNS returns the IPv4 addresses of DNS. Sandboxes have no IPv6.
func (s Settings) IPv4DNS() []netip.Addr {
	var list []netip.Addr
	for _, v := range s.DNS {
		if a, err := netip.ParseAddr(v); err == nil && a.Is4() {
			list = append(list, a)
		}
	}
	return list
}

type TermSize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type Terminal interface {
	io.ReadWriteCloser
	Resize(TermSize) error
}
