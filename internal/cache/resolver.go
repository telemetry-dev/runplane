package cache

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// SandboxResolver finds the sandbox behind a request to the service port of
// the primary, and the repository and the refs of its job, in the store.
type SandboxResolver struct {
	st *store.Store
	// local lists the sandboxes of the primary's own driver.
	local func(ctx context.Context) ([]sandbox.Sandbox, error)
}

func NewSandboxResolver(st *store.Store, local func(ctx context.Context) ([]sandbox.Sandbox, error)) *SandboxResolver {
	return &SandboxResolver{st: st, local: local}
}

// SandboxAt finds the sandbox of the primary at addr in the list of its
// driver. It reads the list each time, because an address can belong to a
// new sandbox a short time after a different sandbox stops.
func (r *SandboxResolver) SandboxAt(ctx context.Context, addr netip.Addr) (Identity, error) {
	list, err := r.local(ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("list sandboxes: %w", err)
	}
	s, ok := sandbox.FindByIP(list, addr)
	if !ok {
		return Identity{}, fmt.Errorf("no sandbox has address %s: %w", addr, ErrNoSandbox)
	}
	return found(r.st.LiveSandboxJob(ctx, s.ID))
}

// SandboxOn finds a sandbox that an agent names. The agent can name only a
// sandbox of its own server.
func (r *SandboxResolver) SandboxOn(ctx context.Context, serverID, id string) (Identity, error) {
	b, err := r.st.LiveSandboxJob(ctx, id)
	if err == nil && b.ServerID != serverID {
		err = fmt.Errorf("sandbox %s is on another server: %w", id, store.ErrNotFound)
	}
	return found(b, err)
}

// found gives the Identity of a live sandbox row, and ErrNoSandbox when no
// live sandbox has the row.
func found(b store.SandboxJob, err error) (Identity, error) {
	if errors.Is(err, store.ErrNotFound) {
		return Identity{}, fmt.Errorf("%w: %w", ErrNoSandbox, err)
	}
	if err != nil {
		return Identity{}, err
	}
	return identity(b), nil
}

// identity gives the refs of the job of a sandbox. Ref is the GITHUB_REF of
// the run, from the run data that runplane read from GitHub when it took
// the job. These runs get no Ref, so they can only read the default branch:
//   - a pull request with no known number, which is the case for a fork;
//   - pull_request_target and workflow_run, which run the workflow of the
//     base branch for code that the base branch does not have. GitHub lets
//     them save to the base branch.
func identity(b store.SandboxJob) Identity {
	id := Identity{SandboxID: b.SandboxID, Repository: b.Repository, DefaultBranch: b.DefaultBranch}
	switch b.Event {
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		// head_branch is the branch of the pull request, which can be a
		// fork branch named like the default branch.
		if b.PRNumber > 0 {
			id.Ref = fmt.Sprintf("refs/pull/%d/merge", b.PRNumber)
			if b.BaseRef != "" {
				id.BaseRef = "refs/heads/" + b.BaseRef
			}
		}
	case "pull_request_target", "workflow_run":
	default:
		if b.HeadBranch == "" {
			break
		}
		// For a tag, head_branch is the tag name.
		if b.Event == "release" || strings.HasSuffix(b.WorkflowRef, "@refs/tags/"+b.HeadBranch) {
			id.Ref = "refs/tags/" + b.HeadBranch
		} else {
			id.Ref = "refs/heads/" + b.HeadBranch
		}
	}
	return id
}
