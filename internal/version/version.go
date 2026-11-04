// Package version holds the build version. The release build sets the
// variables with -ldflags "-X github.com/runplane/runplane/internal/version.Version=...".
package version

var (
	Version = "dev"
	Commit  = ""
	// Repo is the GitHub repository (owner/name) that publishes the releases.
	Repo = "runplane/runplane"
)

// String returns the version with the short commit, for example "v1.2.0 (3f2a9c1)".
func String() string {
	if Commit == "" {
		return Version
	}
	commit := Commit
	if len(commit) > 7 {
		commit = commit[:7]
	}
	return Version + " (" + commit + ")"
}
