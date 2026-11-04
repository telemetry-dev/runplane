package cache

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
)

// SandboxHeader names the sandbox of a request that a trusted proxy (the
// agent cache endpoint) forwards. The server ignores it on other requests.
const SandboxHeader = "X-Runplane-Sandbox"

// ErrNoSandbox means that no sandbox with a job matches a request.
var ErrNoSandbox = errors.New("no sandbox for this request")

// Identity is the sandbox and job behind a request, from runplane records.
// The cache scopes in the runtime token of a job do not count: runplane
// cannot check the signature of the token, so the job controls them.
type Identity struct {
	SandboxID     string
	Repository    string // owner/name of the job; empty if the sandbox has no job
	DefaultBranch string // for example "main"; empty if runplane does not know the repository
	// Ref is the ref of the run: "refs/heads/<branch>", "refs/tags/<tag>",
	// or "refs/pull/<n>/merge". Empty if runplane does not know it.
	Ref string
	// BaseRef is the base branch of a pull request, for example "refs/heads/main".
	BaseRef string
}

// Resolver finds the sandbox behind a request. header contains SandboxHeader
// only when the request came through a trusted proxy; then the sandbox ID
// identifies the sandbox, and remoteIP is the address of the proxy.
// Sandbox returns ErrNoSandbox if no sandbox matches.
type Resolver interface {
	Sandbox(ctx context.Context, remoteIP string, header http.Header) (Identity, error)
}

// readRefs returns the refs that the job may restore from, in search order:
// its ref, its base ref, and the default branch, like GitHub. The job saves
// only to Ref.
func (id Identity) readRefs() []string {
	var defaultRef string
	if id.DefaultBranch != "" {
		defaultRef = "refs/heads/" + strings.TrimPrefix(id.DefaultBranch, "refs/heads/")
	}
	var refs []string
	for _, r := range []string{id.Ref, id.BaseRef, defaultRef} {
		if r != "" && !slices.Contains(refs, r) {
			refs = append(refs, r)
		}
	}
	return refs
}
