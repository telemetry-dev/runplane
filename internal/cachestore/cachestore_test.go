package cachestore

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/cache"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

var t0 = time.Date(2026, 11, 4, 10, 15, 0, 0, time.UTC)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestIndexLookup(t *testing.T) {
	ctx := context.Background()
	x := NewIndex(openStore(t))
	add := func(id, ref, key string, created time.Time, finalized bool) {
		t.Helper()
		e := cache.Entry{ID: id, Repository: "acme/api", Ref: ref, Key: key, Version: "v1", Path: "blobs/" + id,
			CreatedAt: created, LastUsedAt: created}
		if err := x.Create(ctx, e); err != nil {
			t.Fatal(err)
		}
		if finalized {
			if err := x.Finalize(ctx, id, 100, created); err != nil {
				t.Fatal(err)
			}
		}
	}
	main := "refs/heads/main"
	add("e1", main, "npm-linux-aaa", t0, true)
	add("e2", main, "npm-linux-bbb", t0.Add(time.Minute), true)
	add("e3", main, "npm-linux-ccc", t0.Add(2*time.Minute), false) // upload in progress
	add("e4", main, "NPM-LINUX-ddd", t0.Add(3*time.Minute), true)
	add("e5", main, "go-%-x", t0, true)
	add("e6", main, "go-a-x", t0.Add(time.Minute), true)
	add("e7", "refs/heads/dev", "npm-linux-zzz", t0.Add(time.Hour), true)

	tests := []struct {
		name string
		q    cache.Query
		want string // entry ID; "" for ErrNotFound
	}{
		{"newest finalized prefix match", cache.Query{Ref: main, Key: "npm-linux-", Prefix: true}, "e2"},
		{"exact key", cache.Query{Ref: main, Key: "npm-linux-aaa"}, "e1"},
		{"a prefix is not an exact key", cache.Query{Ref: main, Key: "npm-linux-"}, ""},
		{"a wildcard is a plain character", cache.Query{Ref: main, Key: "go-%", Prefix: true}, "e5"},
		{"keys keep their case", cache.Query{Ref: main, Key: "NPM-", Prefix: true}, "e4"},
		{"other version", cache.Query{Ref: main, Key: "npm-linux-aaa", Version: "v2"}, ""},
		{"other ref", cache.Query{Ref: "refs/heads/dev", Key: "npm-linux-", Prefix: true}, "e7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.q.Repository = "acme/api"
			if tt.q.Version == "" {
				tt.q.Version = "v1"
			}
			got, err := x.Lookup(ctx, tt.q)
			if tt.want == "" {
				if !errors.Is(err, cache.ErrNotFound) {
					t.Fatalf("got %q, %v; want ErrNotFound", got.ID, err)
				}
				return
			}
			if err != nil || got.ID != tt.want {
				t.Fatalf("got %q, %v; want %q", got.ID, err, tt.want)
			}
		})
	}
}

func TestIndexEntries(t *testing.T) {
	ctx := context.Background()
	x := NewIndex(openStore(t))
	e := cache.Entry{ID: "a1", Repository: "acme/api", Ref: "refs/heads/main", Key: "k", Version: "v1",
		Path: "blobs/a1", CreatedAt: t0, LastUsedAt: t0}
	if err := x.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	dup := e
	dup.ID = "a2"
	if err := x.Create(ctx, dup); !errors.Is(err, cache.ErrExists) {
		t.Fatalf("second entry with the same key: %v, want ErrExists", err)
	}
	other := cache.Entry{ID: "b1", Repository: "acme/web", Ref: "refs/heads/main", Key: "k", Version: "v1",
		Path: "blobs/b1", CreatedAt: t0.Add(time.Minute), LastUsedAt: t0.Add(time.Minute)}
	newer := cache.Entry{ID: "a3", Repository: "acme/api", Ref: "refs/heads/main", Key: "k2", Version: "v1",
		Path: "blobs/a3", CreatedAt: t0.Add(2 * time.Minute), LastUsedAt: t0.Add(2 * time.Minute)}
	for _, e := range []cache.Entry{other, newer} {
		if err := x.Create(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	used := t0.Add(time.Hour)
	if err := x.Finalize(ctx, "a1", 4096, used); err != nil {
		t.Fatal(err)
	}
	if err := x.Finalize(ctx, "b1", 10, t0); err != nil {
		t.Fatal(err)
	}
	got, err := x.Get(ctx, "a1")
	if err != nil || !got.Finalized || got.SizeBytes != 4096 || !got.LastUsedAt.Equal(used) || got.Path != "blobs/a1" {
		t.Errorf("finalized entry: %+v, %v", got, err)
	}

	list, err := x.List(ctx, "acme/api")
	if ids := entryIDs(list); err != nil || !slices.Equal(ids, []string{"a3", "a1"}) {
		t.Errorf("list acme/api: %v, %v; want newest first", ids, err)
	}
	// The cache server deletes what List returns.
	if list, err := x.List(ctx, ""); err != nil || len(list) != 0 {
		t.Errorf("list of no repository: %v, %v", entryIDs(list), err)
	}
	usage, err := x.Usage(ctx)
	want := []cache.Usage{{Repository: "acme/api", Entries: 1, Bytes: 4096}, {Repository: "acme/web", Entries: 1, Bytes: 10}}
	if err != nil || !slices.Equal(usage, want) {
		t.Errorf("usage: %+v, %v; want %+v", usage, err, want)
	}

	if err := x.Delete(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.Get(ctx, "a1"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("get a deleted entry: %v", err)
	}
	if err := x.Delete(ctx, "a1"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("delete a deleted entry: %v", err)
	}
}

func entryIDs(list []cache.Entry) []string {
	var ids []string
	for _, e := range list {
		ids = append(ids, e.ID)
	}
	return ids
}

func TestResolver(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	if err := st.CreateServer(ctx, store.Server{ID: "local", Name: "primary", Kind: store.ServerLocal,
		Status: store.ServerOnline, CreatedAt: t0, UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGitHubApp(ctx, store.GitHubApp{ID: "app", AppID: 9, Slug: "rp", PrivateKeyEnc: []byte("k"), ClientSecretEnc: []byte("s"), WebhookSecretEnc: []byte("w"), CreatedAt: t0}); err != nil {
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
		j, err := st.UpsertJob(ctx, store.JobMessage{RunnerRequestID: int64(i + 1), State: store.JobRunning, Repository: repo})
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
	api := cache.Identity{SandboxID: "busy", Repository: "acme/api", DefaultBranch: "trunk", Ref: "refs/heads/trunk"}
	web := func(id string) cache.Identity {
		return cache.Identity{SandboxID: id, Repository: "acme/web", Ref: "refs/pull/5/merge", BaseRef: "refs/heads/trunk"}
	}
	sandboxes := []store.Sandbox{
		{ID: "busy", ServerID: "local", State: sandbox.StateBusy, IP: "10.213.0.5", JobID: jobs[0]},
		{ID: "idle", ServerID: "local", State: sandbox.StateIdle, IP: "10.213.0.6"},
		{ID: "old", ServerID: "local", State: sandbox.StateGone, IP: "10.213.0.7", JobID: jobs[0]},
		{ID: "agent", ServerID: "remote", State: sandbox.StateBusy, IP: "10.213.0.8", JobID: jobs[1]},
		{ID: "new", ServerID: "local", State: sandbox.StateBusy, JobID: jobs[1]},
	}
	for _, b := range sandboxes {
		b.PoolID, b.Mode, b.VCPU, b.MemoryMB, b.CreatedAt = "pool", sandbox.ModeContainer, 2, 4096, t0
		if err := st.InsertSandbox(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	// The driver knows the address of the newest sandbox before the table does.
	r := NewResolver(st, func(context.Context) ([]sandbox.Sandbox, error) {
		return []sandbox.Sandbox{{ID: "busy", IP: "10.213.0.5"}, {ID: "new", IP: "10.213.0.9"}}, nil
	})

	tunnel := func(sandboxID, serverID string) http.Header {
		h := http.Header{}
		h.Set(cache.SandboxHeader, sandboxID)
		if serverID != "" {
			h.Set(agent.ServerHeader, serverID)
		}
		return h
	}
	tests := []struct {
		name   string
		ip     string
		header http.Header
		want   cache.Identity // zero: ErrNoSandbox
	}{
		{"sandbox address", "10.213.0.5", nil, api},
		{"IPv4 in IPv6 form", "::ffff:10.213.0.5", nil, api},
		{"sandbox without a job", "10.213.0.6", nil, cache.Identity{SandboxID: "idle"}},
		{"address from the driver", "10.213.0.9", nil, web("new")},
		{"stopped sandbox", "10.213.0.7", nil, cache.Identity{}},
		{"address of an agent sandbox", "10.213.0.8", nil, cache.Identity{}},
		{"unknown address", "192.0.2.1", nil, cache.Identity{}},
		{"agent names its sandbox", "203.0.113.4", tunnel("agent", "remote"), web("agent")},
		{"agent names a sandbox of the primary", "203.0.113.4", tunnel("busy", "remote"), cache.Identity{}},
		{"sandbox header without a server", "10.213.0.5", tunnel("agent", ""), cache.Identity{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.Sandbox(ctx, tt.ip, tt.header)
			if tt.want == (cache.Identity{}) {
				if !errors.Is(err, cache.ErrNoSandbox) {
					t.Fatalf("got %+v, %v; want ErrNoSandbox", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}

	if b, err := st.GetSandbox(ctx, "new"); err != nil || b.IP != "10.213.0.9" {
		t.Errorf("the table did not get the address from the driver: %+v, %v", b, err)
	}
	if !r.Allow(netip.MustParseAddr("10.213.0.6")) || r.Allow(netip.MustParseAddr("10.213.0.8")) {
		t.Error("the mirror must allow the sandboxes of the primary and nothing else")
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

func TestStatsAddPerHour(t *testing.T) {
	st := openStore(t)
	s := NewStats(st, slog.New(slog.DiscardHandler))
	now := t0
	s.now = func() time.Time { return now }
	s.Add("acme/api", cache.Counts{Hits: 1, BytesRead: 100})
	now = t0.Add(30 * time.Minute)
	s.Add("acme/api", cache.Counts{Misses: 1})
	s.Add("acme/web", cache.Counts{BytesWritten: 7})
	now = t0.Add(time.Hour)
	s.Add("acme/api", cache.Counts{Hits: 2})

	// Run writes the counts when it ends.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx)
	// A later flush adds to the rows that exist.
	s.Add("acme/api", cache.Counts{Hits: 1})
	s.flush(context.Background())

	rows, err := st.DB().Query("SELECT hour, repository, hits, misses, bytes_read, bytes_written FROM cache_stats ORDER BY hour, repository")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		hour                                  int64
		repository                            string
		hits, misses, bytesRead, bytesWritten int64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.hour, &r.repository, &r.hits, &r.misses, &r.bytesRead, &r.bytesWritten); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	h := t0.Unix() / 3600
	want := []row{
		{h, "acme/api", 1, 1, 100, 0},
		{h, "acme/web", 0, 0, 0, 7},
		{h + 1, "acme/api", 3, 0, 0, 0},
	}
	if !slices.Equal(got, want) {
		t.Errorf("cache_stats rows:\n got %+v\nwant %+v", got, want)
	}
}
