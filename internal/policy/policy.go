// Package policy decides which jobs runplane runs and where, from the
// settings and the sandbox modes of the runners.
//
// A job of a public repository runs only in VM mode, because anyone can
// change its code. GitHub can give each job of a scale set to each of its
// runners, so while public repositories are allowed, each runner must be in
// VM mode. The mode setting does not tell the mode of a runner: each server
// runs its own mode, and a mode change applies only after a restart.
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
// modes are the modes of the runners that can get the job.
func Refusal(st store.Settings, job Job, modes []sandbox.Mode) string {
	switch {
	case !job.Private && !st.AllowPublicRepos:
		return "Public repository: the setting allow_public_repos is off."
	case job.Fork && !st.AllowForkPRs:
		return "Fork pull request: the setting allow_fork_prs is off."
	case !job.Private && slices.ContainsFunc(modes, func(m sandbox.Mode) bool { return m != sandbox.ModeVM }):
		return "Public repository: a server for this pool does not run VM mode."
	}
	return ""
}

// RunnerAllowed tells if a runner in mode m can wait for jobs.
func RunnerAllowed(st store.Settings, m sandbox.Mode) bool {
	return !st.AllowPublicRepos || m == sandbox.ModeVM
}
