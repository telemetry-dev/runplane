package scheduler

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// The github package implements ScaleSetClient and GitHub, and the fleet
// package implements Fleet. Change these interfaces only together with them.

// Scaler takes the messages of the listener of one scale set.
type Scaler interface {
	// Scale handles one message. The same message can come again after an
	// error, and the zero Message comes when a poll brings nothing.
	Scale(ctx context.Context, m Message) error
}

// Message is one message of a scale set listener.
type Message struct {
	Stats     *store.ScaleSetStats // nil if the message has none
	Available []JobMessage
	Assigned  []JobMessage
	Started   []JobMessage
	Completed []JobMessage
}

// JobMessage is one job of a listener message. RunnerRequestID is the key of
// the job row, and the job has no ScaleSetID or PoolID.
type JobMessage struct {
	store.JobMessage
	RunnerName string // the runner of a started or completed job
}

// ScaleSetClient is the GitHub side of one runner scale set.
type ScaleSetClient interface {
	// AcquireJobs takes the offered jobs with these keys for the scale set,
	// and returns the keys of the jobs that GitHub gave to it.
	AcquireJobs(ctx context.Context, keys []int64) ([]int64, error)
	GenerateJITConfig(ctx context.Context, runnerName string) (JITConfig, error)
	// RemoveRunner returns ErrRunnerBusy if the runner has a job, and nil if
	// the runner does not exist.
	RemoveRunner(ctx context.Context, runnerID int64) error
	ListRunners(ctx context.Context) ([]Runner, error)
	// SetMaxRunners sets the maxCapacity that the listener sends with each poll.
	SetMaxRunners(n int)
}

type JITConfig struct {
	Encoded    string
	RunnerID   int64
	RunnerName string
}

type Runner struct {
	ID   int64
	Name string
	Busy bool
}

var ErrRunnerBusy = errors.New("runner is busy")

// GitHub gives the job data that the scale set messages do not have. It
// does not change the jobs in the store.
type GitHub interface {
	// CheckJob tells if the repository is private and if the run comes from
	// a fork pull request, and gives the run data for the cache refs. The
	// scheduler calls it before it acquires a job.
	CheckJob(ctx context.Context, repository string, runID int64) (JobCheck, error)
	// Enrich reads the run and the job from the REST API.
	Enrich(ctx context.Context, job store.Job) (Enrichment, error)
	// Steps returns nil if the job has no GitHub job ID yet.
	Steps(ctx context.Context, job store.Job) ([]store.JobStep, error)
	// DownloadLog returns the complete job log.
	DownloadLog(ctx context.Context, job store.Job) (io.ReadCloser, error)
	Rerun(ctx context.Context, job store.Job) error
}

type JobCheck struct {
	Private bool
	Fork    bool
	Ref     store.RunRef
}

// Enrichment is the run data of a job, and its result if GitHub completed it.
type Enrichment struct {
	store.JobEnrichment
	Result     string // "" while the job is not completed on GitHub
	FinishedAt time.Time
}

// ErrRunUnreadable is the CheckJob error when runplane can never read the
// run, for example because the GitHub App is not installed on the
// repository. The scheduler then rejects the job instead of trying again.
var ErrRunUnreadable = errors.New("runplane cannot read the run")

// Fleet is the set of servers that can operate sandboxes.
type Fleet interface {
	// Online returns the servers that can take new sandboxes now: connected,
	// with the status online.
	Online() []Host
	// Driver returns the driver of a connected server, also of a draining or
	// disabled one, or nil.
	Driver(serverID string) Driver
	// Image returns the image for new sandboxes on the server, if the image
	// is ready there. An empty imageID means the default image.
	Image(serverID, imageID string) (sandbox.ImageRef, bool)
	// RunnerVersion returns the runner version for new sandboxes on the
	// server, if a runner bundle for the server arch is ready there.
	RunnerVersion(serverID string) (string, bool)
}

// Host is one online server and its driver.
type Host struct {
	ServerID string
	Name     string
	Arch     string // "x64" or "arm64"
	Driver   Driver
}

// Driver is the part of sandbox.Driver that the scheduler uses.
type Driver interface {
	Mode() sandbox.Mode
	// Start returns when the sandbox process starts. The state is then starting.
	Start(ctx context.Context, spec sandbox.Spec) error
	// Stop is idempotent.
	Stop(ctx context.Context, id string) error
	List(ctx context.Context) ([]sandbox.Sandbox, error)
	Capacity(ctx context.Context) (sandbox.Capacity, error)
	// Hold keeps a finished debug sandbox until the given time.
	Hold(ctx context.Context, id string, until time.Time) error
}
