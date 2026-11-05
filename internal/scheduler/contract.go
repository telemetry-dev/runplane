package scheduler

import (
	"context"
	"errors"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// The github package implements ScaleSetClient and GitHub, and the fleet
// package implements Fleet. Change these interfaces only together with them.

// ScaleSetClient is the GitHub side of one runner scale set.
type ScaleSetClient interface {
	// AcquireJobs returns the request IDs that GitHub gave to the scale set.
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
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

// GitHub gives the job data that the scale set messages do not have.
type GitHub interface {
	// CheckJob tells if the repository is private and if the run comes from
	// a fork pull request, and gives the run data for the cache refs. The
	// scheduler calls it before it acquires a job.
	CheckJob(ctx context.Context, repository string, runID int64) (JobCheck, error)
	// Enrich reads the run and job from the REST API and writes the
	// enrichment fields to the store.
	Enrich(ctx context.Context, job store.Job) error
	// Steps returns nil if the job has no GitHub job ID yet.
	Steps(ctx context.Context, job store.Job) ([]store.JobStep, error)
	// DownloadLog writes the complete job log to path.
	DownloadLog(ctx context.Context, job store.Job, path string) error
	Rerun(ctx context.Context, job store.Job) error
}

type JobCheck struct {
	Private bool
	Fork    bool
	Ref     store.RunRef
}

// ErrRunUnreadable is the CheckJob error when runplane can never read the
// run, for example because the GitHub App is not installed on the
// repository. The scheduler then rejects the job instead of trying again.
var ErrRunUnreadable = errors.New("runplane cannot read the run")

// Fleet is the set of servers that can operate sandboxes.
type Fleet interface {
	// Online returns the servers that can take new sandboxes now: status
	// online, connected, and not draining or disabled.
	Online() []sandbox.Host
	// Driver returns the driver of a connected server, or nil.
	Driver(serverID string) sandbox.Driver
	// Image returns the image for new sandboxes on the server, if the image
	// is ready there. An empty imageID means the default image.
	Image(serverID, imageID string) (sandbox.ImageRef, bool)
	// RunnerVersion returns the runner version for new sandboxes on the
	// server, if a runner bundle for the server arch is ready there.
	RunnerVersion(serverID string) (string, bool)
}
