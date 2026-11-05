package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestReplaceRepositories(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	now := time.Now()
	must(t, s.SaveGitHubApp(ctx, GitHubApp{ID: "a", AppID: 5, Slug: "rp", Name: "rp", PrivateKeyEnc: []byte{1}, CreatedAt: now}))
	must(t, s.UpsertInstallation(ctx, Installation{ID: 10, AppID: 5, AccountLogin: "acme", AccountID: 1,
		AccountType: "Organization", CreatedAt: now, UpdatedAt: now}))
	names := func() []string {
		t.Helper()
		list, err := s.ListRepositories(ctx, 10)
		must(t, err)
		var out []string
		for _, r := range list {
			out = append(out, r.FullName)
		}
		return out
	}
	repo := func(id int64, name string) Repository {
		return Repository{ID: id, FullName: name, Private: true, UpdatedAt: now}
	}
	must(t, s.ReplaceRepositories(ctx, 10, []Repository{repo(1, "acme/a"), repo(2, "acme/b")}))
	must(t, s.ReplaceRepositories(ctx, 10, []Repository{repo(2, "acme/b"), repo(3, "acme/c")}))
	if got := names(); !slices.Equal(got, []string{"acme/b", "acme/c"}) {
		t.Fatalf("repositories = %v, want [acme/b acme/c]", got)
	}
	if r, err := s.GetRepositoryByName(ctx, "ACME/C"); err != nil || r.ID != 3 || !r.Private {
		t.Fatalf("by name: %+v, %v", r, err)
	}

	// A new app replaces the old one, and the old installations go with it.
	must(t, s.SaveGitHubApp(ctx, GitHubApp{ID: "b", AppID: 6, Slug: "rp2", Name: "rp2", PrivateKeyEnc: []byte{2}, CreatedAt: now}))
	if app, err := s.GetGitHubApp(ctx); err != nil || app.ID != "b" {
		t.Fatalf("app = %q, %v", app.ID, err)
	}
	if got := names(); len(got) != 0 {
		t.Fatalf("repositories of the old app = %v, want none", got)
	}
}

func TestConsumeManifestState(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	now := time.Now()
	for _, m := range []ManifestState{
		{StateHash: "ok", AccountType: "organization", Organization: "acme", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{StateHash: "old", AccountType: "user", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
	} {
		must(t, s.CreateManifestState(ctx, m))
	}
	if m, err := s.ConsumeManifestState(ctx, "ok", now); err != nil || m.Organization != "acme" {
		t.Fatalf("first use: %+v, %v", m, err)
	}
	for _, hash := range []string{"ok", "old", "unknown"} {
		if _, err := s.ConsumeManifestState(ctx, hash, now); !errors.Is(err, ErrNotFound) {
			t.Errorf("state %s: err = %v, want ErrNotFound", hash, err)
		}
	}
}
