// Package sandbox defines the contract between the scheduler and the drivers
// that operate sandboxes (container mode, VM mode, remote agents).
package sandbox

import (
	"context"
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

// LabelSandbox is the container or VM label that marks a runplane sandbox.
// Drivers change only objects with this label.
const LabelSandbox = "dev.runplane.sandbox"

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
	Labels        map[string]string `json:"labels,omitempty"`
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

type Capacity struct {
	Mode            Mode    `json:"mode"`
	Arch            string  `json:"arch"`
	VCPU            int     `json:"vcpu"`
	MemoryMB        int     `json:"memory_mb"`
	DiskBytes       int64   `json:"disk_bytes"`
	FreeDiskBytes   int64   `json:"free_disk_bytes"`
	AllocVCPU       int     `json:"alloc_vcpu"`
	AllocMemoryMB   int     `json:"alloc_memory_mb"`
	ReserveVCPU     int     `json:"reserve_vcpu"`
	ReserveMemoryMB int     `json:"reserve_memory_mb"`
	Overcommit      float64 `json:"overcommit"`
	MaxSandboxes    int     `json:"max_sandboxes"` // 0 = no limit
	Sandboxes       int     `json:"sandboxes"`
}

// FreeVCPU is the vCPU count that new sandboxes can still get.
func (c Capacity) FreeVCPU() int {
	overcommit := c.Overcommit
	if overcommit <= 0 {
		overcommit = 1
	}
	return int(float64(c.VCPU-c.ReserveVCPU)*overcommit) - c.AllocVCPU
}

// FreeMemoryMB is the memory that new sandboxes can still get.
// Memory is never overcommitted.
func (c Capacity) FreeMemoryMB() int {
	return c.MemoryMB - c.ReserveMemoryMB - c.AllocMemoryMB
}

// Fits reports if a new sandbox of this size can start now.
func (c Capacity) Fits(vcpu, memoryMB int) bool {
	if c.MaxSandboxes > 0 && c.Sandboxes >= c.MaxSandboxes {
		return false
	}
	return vcpu <= c.FreeVCPU() && memoryMB <= c.FreeMemoryMB()
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

type Metrics struct {
	CPUPercent float64 `json:"cpu_percent"` // of the sandbox vCPU allocation, 0 to 100
	MemoryMB   int     `json:"memory_mb"`
}

// Driver operates sandboxes on one server.
type Driver interface {
	Mode() Mode
	// Start returns when the sandbox process starts. The state is then starting.
	Start(ctx context.Context, spec Spec) error
	// Stop is idempotent.
	Stop(ctx context.Context, id string) error
	List(ctx context.Context) ([]Sandbox, error)
	Capacity(ctx context.Context) (Capacity, error)
	Events() <-chan Event
	// Logs returns the console and runner log of the sandbox.
	Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	Shell(ctx context.Context, id string, size TermSize) (Terminal, error)
	// Hold keeps a finished debug sandbox until the given time.
	Hold(ctx context.Context, id string, until time.Time) error
	Close() error
}

// Host is one server and its driver, as the scheduler sees it.
type Host struct {
	ServerID string
	Name     string
	Arch     string // "x64" or "arm64"
	Driver   Driver
}

// Settings are the dashboard settings that a driver obeys. The primary sends
// them to each server when they change.
type Settings struct {
	ReserveVCPU        int      `json:"reserve_vcpu"`
	ReserveMemoryMB    int      `json:"reserve_memory_mb"`
	Overcommit         float64  `json:"overcommit"`
	MinFreeDiskPercent int      `json:"min_free_disk_percent"`
	DNS                []string `json:"dns"`
	EgressAllowlist    []string `json:"egress_allowlist"`
	BandwidthMbps      int      `json:"bandwidth_mbps"` // VM mode, 0 = no limit
	RegistryMirror     bool     `json:"registry_mirror"`
	ActionsCache       bool     `json:"actions_cache"`
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

// Configurable is a driver that can take new settings while it operates.
type Configurable interface {
	Configure(Settings) error
}

type TermSize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type Terminal interface {
	io.ReadWriteCloser
	Resize(TermSize) error
}
