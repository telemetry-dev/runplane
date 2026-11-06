// Package policy decides which jobs runplane runs and where, from the
// settings and the sandbox modes of the runners. It also has the rules of the
// sandbox mode: which mode the primary uses, and which modes it can use.
//
// A job of a public repository runs only in VM mode, because anyone can
// change its code. The setting allow_public_repos alone decides if runplane
// takes such jobs. GitHub can give each job of a scale set to each of its
// runners, so while public jobs can come to a scale set, only VM servers get
// its runners, and a server in another mode waits. The mode setting does not
// tell the mode of a runner: each server runs its own mode, and a mode change
// applies only after a restart.
package policy

import (
	"slices"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// Job is what runplane knows about a job before it takes it.
type Job struct {
	Private bool // the repository is private
	Fork    bool // the code comes from a pull request of a fork
}

// Refusal returns why runplane does not run the job, or "" if it runs it.
// modes are the modes of the live runners of the scale set. A runner from
// before allow_public_repos went on can be in another mode until it stops.
func Refusal(st store.Settings, job Job, modes []sandbox.Mode) string {
	switch {
	case !job.Private && !st.AllowPublicRepos:
		return "Public repository: the setting allow_public_repos is off."
	case job.Fork && !st.AllowForkPRs:
		return "Fork pull request: the setting allow_fork_prs is off."
	case !job.Private && slices.ContainsFunc(modes, func(m sandbox.Mode) bool { return m != sandbox.ModeVM }):
		return "Public repository: a runner of this scale set does not run VM mode."
	}
	return ""
}

// TakesJobs tells if runplane takes the jobs of the repository r while its
// installation is active. GitHub runs no workflows of an archived
// repository. The scheduler checks each job again with Refusal, with the
// fork and the modes of its runners.
func TakesJobs(st store.Settings, r store.Repository) bool {
	return !r.Archived && Refusal(st, Job{Private: r.Private}, nil) == ""
}

// RunnerRefusal tells why a server in mode m gets no runner of a scale set,
// or returns "" when it gets one. publicWaits tells that a job of a public
// repository waits in the scale set for a runner: an admin turned
// allow_public_repos off after the scale set acquired the job.
func RunnerRefusal(st store.Settings, m sandbox.Mode, publicWaits bool) string {
	switch {
	case m == sandbox.ModeVM:
		return ""
	case st.AllowPublicRepos:
		return "Only servers in VM mode get runners while allow_public_repos is on."
	case publicWaits:
		return "A job of a public repository waits in the scale set for a runner in VM mode."
	}
	return ""
}
