package cache

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// SandboxResolver finds the sandbox behind a request to the service port of
// the primary, and the repository and the refs of its job, in the store.
type SandboxResolver struct {
	st *store.Store
	// local lists the sandboxes of the primary's own driver. It finds a
	// sandbox whose address the sandboxes table does not have yet.
	local func(ctx context.Context) ([]sandbox.Sandbox, error)

	mu      sync.Mutex
	localID string
}

func NewSandboxResolver(st *store.Store, local func(ctx context.Context) ([]sandbox.Sandbox, error)) *SandboxResolver {
	return &SandboxResolver{st: st, local: local}
}

// Sandbox finds a sandbox of the primary by the source address of the
// request. A request through the agent tunnel names its sandbox in
// SandboxHeader instead; the agent can name only a sandbox of its own
// server, which the primary sets in ServerHeader.
func (r *SandboxResolver) Sandbox(ctx context.Context, remoteIP string, header http.Header) (Identity, error) {
	var b store.SandboxJob
	var err error
	if id := header.Get(SandboxHeader); id != "" {
		b, err = r.st.LiveSandboxJob(ctx, id)
		if err == nil && b.ServerID != header.Get(ServerHeader) {
			err = fmt.Errorf("sandbox %s is on another server: %w", id, store.ErrNotFound)
		}
	} else {
		b, err = r.byIP(ctx, remoteIP)
	}
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

// Allow reports if a sandbox of the primary has the address: the Allow
// hook of the registry mirror.
func (r *SandboxResolver) Allow(ip netip.Addr) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.byIP(ctx, ip.String())
	return err == nil
}

func (r *SandboxResolver) byIP(ctx context.Context, remoteIP string) (store.SandboxJob, error) {
	addr, err := netip.ParseAddr(remoteIP)
	if err != nil {
		return store.SandboxJob{}, fmt.Errorf("source address %q: %w", remoteIP, store.ErrNotFound)
	}
	ip := addr.Unmap().String()
	serverID, err := r.localServer(ctx)
	if err != nil {
		return store.SandboxJob{}, err
	}
	b, err := r.st.LiveSandboxJobByIP(ctx, serverID, ip)
	if !errors.Is(err, store.ErrNotFound) {
		return b, err
	}
	list, err := r.local(ctx)
	if err != nil {
		return store.SandboxJob{}, fmt.Errorf("list sandboxes: %w", err)
	}
	s, ok := sandbox.FindByIP(list, addr)
	if !ok {
		return store.SandboxJob{}, fmt.Errorf("no sandbox has address %s: %w", ip, store.ErrNotFound)
	}
	if err := r.st.SetSandboxIP(ctx, serverID, s.ID, ip); err != nil {
		return store.SandboxJob{}, err
	}
	return r.st.LiveSandboxJob(ctx, s.ID)
}

func (r *SandboxResolver) localServer(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.localID != "" {
		return r.localID, nil
	}
	v, err := r.st.GetLocalServer(ctx)
	if err != nil {
		return "", err
	}
	r.localID = v.ID
	return v.ID, nil
}
