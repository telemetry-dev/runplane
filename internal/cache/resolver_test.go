package cache

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

var t0 = time.Date(2026, 11, 4, 10, 15, 0, 0, time.UTC)

func TestResolver(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	if err := st.CreateServer(ctx, store.Server{ID: "local", Name: "primary", Kind: store.ServerLocal,
		Status: store.ServerOnline, CreatedAt: t0, UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGitHubApp(ctx, store.GitHubApp{ID: "app", AppID: 9, Slug: "rp", PrivateKeyEnc: []byte("k"), CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertInstallation(ctx, store.Installation{ID: 1, AppID: 9, AccountLogin: "acme",
		AccountType: "Organization", CreatedAt: t0, UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceRepositories(ctx, 1, []store.Repository{
		{ID: 11, InstallationID: 1, FullName: "acme/api", Private: true, DefaultBranch: "trunk", UpdatedAt: t0},
	}); err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for i, repo := range []string{"acme/api", "acme/web"} {
		j, _, err := st.UpsertJob(ctx, store.JobMessage{RunnerRequestID: int64(i + 1), State: store.JobRunning, Repository: repo}, t0)
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j.ID)
	}
	if err := st.SetJobRunRef(ctx, jobs[0], store.RunRef{Event: "push", HeadBranch: "trunk"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobRunRef(ctx, jobs[1], store.RunRef{Event: "pull_request", HeadBranch: "main", PRNumber: 5, BaseRef: "trunk"}); err != nil {
		t.Fatal(err)
	}
	api := Identity{SandboxID: "busy", Repository: "acme/api", DefaultBranch: "trunk", Ref: "refs/heads/trunk"}
	web := func(id string) Identity {
		return Identity{SandboxID: id, Repository: "acme/web", Ref: "refs/pull/5/merge", BaseRef: "refs/heads/trunk"}
	}
	sandboxes := []store.Sandbox{
		{ID: "busy", ServerID: "local", State: sandbox.StateBusy, JobID: jobs[0]},
		{ID: "idle", ServerID: "local", State: sandbox.StateIdle},
		{ID: "old", ServerID: "local", State: sandbox.StateGone, JobID: jobs[0]},
		{ID: "agent", ServerID: "remote", State: sandbox.StateBusy, IP: "10.213.0.8", JobID: jobs[1]},
	}
	for _, b := range sandboxes {
		b.PoolID, b.Mode, b.VCPU, b.MemoryMB, b.CreatedAt = "pool", sandbox.ModeContainer, 2, 4096, t0
		if err := st.InsertSandbox(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	// The driver of the primary knows the addresses. It still lists old,
	// which the scheduler has recorded as gone.
	r := NewSandboxResolver(st, func(context.Context) ([]sandbox.Sandbox, error) {
		return []sandbox.Sandbox{
			{ID: "busy", State: sandbox.StateBusy, IP: "10.213.0.5"},
			{ID: "idle", State: sandbox.StateIdle, IP: "10.213.0.6"},
			{ID: "old", State: sandbox.StateBusy, IP: "10.213.0.7"},
		}, nil
	})

	at := func(ip string) func() (Identity, error) {
		return func() (Identity, error) { return r.SandboxAt(ctx, netip.MustParseAddr(ip)) }
	}
	on := func(serverID, id string) func() (Identity, error) {
		return func() (Identity, error) { return r.SandboxOn(ctx, serverID, id) }
	}
	tests := []struct {
		name string
		find func() (Identity, error)
		want Identity // zero: ErrNoSandbox
	}{
		{"sandbox address", at("10.213.0.5"), api},
		{"IPv4 in IPv6 form", at("::ffff:10.213.0.5"), api},
		{"sandbox without a job", at("10.213.0.6"), Identity{SandboxID: "idle"}},
		{"stopped sandbox", at("10.213.0.7"), Identity{}},
		{"address of an agent sandbox", at("10.213.0.8"), Identity{}},
		{"unknown address", at("192.0.2.1"), Identity{}},
		{"agent names its sandbox", on("remote", "agent"), web("agent")},
		{"agent names a sandbox of the primary", on("remote", "busy"), Identity{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.find()
			if tt.want == (Identity{}) {
				if !errors.Is(err, ErrNoSandbox) {
					t.Fatalf("got %+v, %v; want ErrNoSandbox", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

// The cache server lets a job save only to Ref, so Ref must never name a
// branch that the code of the run is not on.
func TestIdentityRefs(t *testing.T) {
	tests := []struct {
		name         string
		run          store.RunRef
		workflowRef  string
		ref, baseRef string
	}{
		{"push to a branch", store.RunRef{Event: "push", HeadBranch: "feature/x"}, "acme/api/.github/workflows/ci.yml@refs/heads/feature/x",
			"refs/heads/feature/x", ""},
		{"push of a tag", store.RunRef{Event: "push", HeadBranch: "v1.2.0"}, "acme/api/.github/workflows/ci.yml@refs/tags/v1.2.0",
			"refs/tags/v1.2.0", ""},
		{"release", store.RunRef{Event: "release", HeadBranch: "v1.2.0"}, "", "refs/tags/v1.2.0", ""},
		{"schedule on the default branch", store.RunRef{Event: "schedule", HeadBranch: "main"}, "", "refs/heads/main", ""},
		{"pull request", store.RunRef{Event: "pull_request", HeadBranch: "fix", PRNumber: 12, BaseRef: "develop"}, "",
			"refs/pull/12/merge", "refs/heads/develop"},
		{"pull request from a fork branch named main", store.RunRef{Event: "pull_request", HeadBranch: "main"}, "", "", ""},
		{"pull request review", store.RunRef{Event: "pull_request_review", HeadBranch: "fix", PRNumber: 3, BaseRef: "main"}, "",
			"refs/pull/3/merge", "refs/heads/main"},
		{"pull_request_target", store.RunRef{Event: "pull_request_target", HeadBranch: "main", PRNumber: 4, BaseRef: "main"}, "", "", ""},
		{"workflow_run", store.RunRef{Event: "workflow_run", HeadBranch: "main"}, "", "", ""},
		{"no run data", store.RunRef{Event: "push"}, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := identity(store.SandboxJob{SandboxID: "sb", Repository: "acme/api", DefaultBranch: "main",
				WorkflowRef: tt.workflowRef, RunRef: tt.run})
			if got.Ref != tt.ref || got.BaseRef != tt.baseRef {
				t.Errorf("ref %q base %q, want %q %q", got.Ref, got.BaseRef, tt.ref, tt.baseRef)
			}
		})
	}
}
