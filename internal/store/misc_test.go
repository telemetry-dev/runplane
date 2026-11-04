package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestReplaceRepositories(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.SaveGitHubApp(ctx, GitHubApp{ID: "a", AppID: 5, Slug: "rp", Name: "rp", PrivateKeyEnc: []byte{1}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertInstallation(ctx, Installation{ID: 10, AppID: 5, AccountLogin: "acme", AccountID: 1,
		AccountType: "Organization", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	names := func() []string {
		t.Helper()
		list, err := s.ListRepositories(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range list {
			out = append(out, r.FullName)
		}
		return out
	}
	repo := func(id int64, name string) Repository {
		return Repository{ID: id, FullName: name, Private: true, UpdatedAt: now}
	}
	if err := s.ReplaceRepositories(ctx, 10, []Repository{repo(1, "acme/a"), repo(2, "acme/b")}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceRepositories(ctx, 10, []Repository{repo(2, "acme/b"), repo(3, "acme/c")}); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"acme/b", "acme/c"}) {
		t.Fatalf("repositories = %v, want [acme/b acme/c]", got)
	}
	if r, err := s.GetRepositoryByName(ctx, "ACME/C"); err != nil || r.ID != 3 || !r.Private {
		t.Fatalf("by name: %+v, %v", r, err)
	}

	// A new app replaces the old one, and the old installations go with it.
	if err := s.SaveGitHubApp(ctx, GitHubApp{ID: "b", AppID: 6, Slug: "rp2", Name: "rp2", PrivateKeyEnc: []byte{2}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if app, err := s.GetGitHubApp(ctx); err != nil || app.ID != "b" {
		t.Fatalf("app = %q, %v", app.ID, err)
	}
	if n, _ := s.CountRepositories(ctx); n != 0 {
		t.Fatalf("repositories of the old app = %d, want 0", n)
	}
}

func TestSetDefaultImage(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	for _, img := range []Image{
		{ID: "i1", Name: "Ubuntu 24.04", Ref: "ghcr.io/runplane/ubuntu-24.04", IsDefault: true, Source: "builtin"},
		{ID: "i2", Name: "Ubuntu 26.04", Ref: "ghcr.io/runplane/ubuntu-26.04", Source: "builtin"},
	} {
		img.CreatedAt, img.UpdatedAt = now, now
		if err := s.CreateImage(ctx, img); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetDefaultImage(ctx, "i2"); err != nil {
		t.Fatal(err)
	}
	if img, err := s.GetDefaultImage(ctx); err != nil || img.ID != "i2" {
		t.Fatalf("default = %q, %v", img.ID, err)
	}
	if err := s.SetDefaultImage(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown image: err = %v", err)
	}
	if img, _ := s.GetDefaultImage(ctx); img.ID != "i2" {
		t.Fatalf("a failed change lost the default: %q", img.ID)
	}

	// A pool with a deleted image changes to the default image.
	if err := s.CreatePool(ctx, Pool{ID: "p", Name: "Default", Label: "runplane", Arch: "x64", VCPU: 2, MemoryMB: 4096,
		DiskGB: 64, ImageID: "i1", MaxRunners: 2, Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteImage(ctx, "i1"); err != nil {
		t.Fatal(err)
	}
	if p, err := s.GetPoolByLabel(ctx, "RunPlane"); err != nil || p.ImageID != "" {
		t.Fatalf("pool image = %q, %v; want empty", p.ImageID, err)
	}
}

func TestLatestReadyBundle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, b := range []RunnerBundle{
		{Version: "2.99.0", Arch: "x64", Status: BundleReady},
		{Version: "2.330.0", Arch: "x64", Status: BundleReady},
		{Version: "2.331.0", Arch: "x64", Status: BundleFailed},
		{Version: "2.340.0", Arch: "arm64", Status: BundleReady},
	} {
		b.CreatedAt = time.Now()
		if err := s.UpsertRunnerBundle(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	b, err := s.LatestReadyBundle(ctx, "x64")
	if err != nil {
		t.Fatal(err)
	}
	if b.Version != "2.330.0" {
		t.Errorf("latest ready x64 = %s, want 2.330.0", b.Version)
	}
	if _, err := s.LatestReadyBundle(ctx, "riscv"); !errors.Is(err, ErrNotFound) {
		t.Errorf("no bundle: err = %v", err)
	}
}

func TestConsumeManifestState(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	for _, m := range []ManifestState{
		{StateHash: "ok", AccountType: "organization", Organization: "acme", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{StateHash: "old", AccountType: "user", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
	} {
		if err := s.CreateManifestState(ctx, m); err != nil {
			t.Fatal(err)
		}
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

// A deleted server leaves no ID in the pools, so a later pool change does
// not fail on an unknown server.
func TestDeleteServerLeavesThePools(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"s1", "s2"} {
		if err := s.CreateServer(ctx, Server{ID: id, Name: id, Kind: ServerAgent, Status: ServerOnline, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	pools := map[string][]string{"both": {"s1", "s2"}, "only-s1": {"s1"}, "all": {}, "only-s2": {"s2"}}
	for label, ids := range pools {
		err := s.CreatePool(ctx, Pool{ID: label, Name: label, Label: label, Arch: "x64", VCPU: 2, MemoryMB: 4096, DiskGB: 64,
			MaxRunners: 2, ServerIDs: ids, Enabled: true, CreatedAt: now, UpdatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := s.DeleteServer(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		ids     []string
		enabled bool
	}{
		"both":    {[]string{"s2"}, true},
		"only-s1": {[]string{}, false}, // empty would mean all servers
		"all":     {[]string{}, true},
		"only-s2": {[]string{"s2"}, true},
	}
	for label, w := range want {
		p, err := s.GetPool(ctx, label)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(p.ServerIDs, w.ids) || p.Enabled != w.enabled {
			t.Errorf("pool %s: server_ids %q, enabled %v; want %q, %v", label, p.ServerIDs, p.Enabled, w.ids, w.enabled)
		}
	}
	if err := s.DeleteServer(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}
