//go:build devseed

// Package devseed fills an empty database with demo data that matches the
// Paper designs: the Kestrel team with six servers, five runner labels, and
// about 1,300 jobs from the last 30 days. It is for development and design
// review only. Builds include it only with the build tag devseed.
package devseed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

// Seed fills the empty database of st with the demo data, with times relative
// to now. It seals the fake GitHub and webhook secrets with box and
// writes job logs to logsDir. The same now gives the same data, except the
// job IDs and enriched_at times, which the store sets, and the sealed
// secrets. Seed refuses a database that has users or servers.
func Seed(ctx context.Context, st *store.Store, box *secret.Box, logsDir string, now time.Time) error {
	if err := checkEmpty(ctx, st); err != nil {
		return err
	}
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	src := rand.NewChaCha8([32]byte{'r', 'u', 'n', 'p', 'l', 'a', 'n', 'e'})
	now = now.UTC()
	s := &seeder{
		st: st, box: box, logsDir: logsDir, now: now,
		// Keep the design date, but stay before the 30 days of jobs.
		base:    earlier(setupDate, now.Add(-33*24*time.Hour)),
		src:     src,
		rng:     rand.New(src),
		servers: map[string]*server{},
		pools:   map[string]store.Pool{},
	}
	for _, step := range []func(context.Context) error{
		s.settings, s.users, s.github, s.addServers, s.images, s.bundles, s.addPools,
		s.history, s.cacheEntries, s.notifications, s.audit,
	} {
		if err := step(ctx); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}
	return nil
}

func checkEmpty(ctx context.Context, st *store.Store) error {
	users, err := st.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	servers, err := st.ListServers(ctx)
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	if users > 0 || len(servers) > 0 {
		return errors.New("seed: the database is not empty; seed a new data folder")
	}
	return nil
}

type seeder struct {
	st      *store.Store
	box     *secret.Box
	logsDir string
	now     time.Time
	base    time.Time // when Ada set up runplane
	src     *rand.ChaCha8
	rng     *rand.Rand

	admin, member string
	servers       map[string]*server     // by name
	pools         map[string]store.Pool  // by label
	digests       map[string]string      // default image digest by arch
	scaleSets     map[scaleSetKey]string // scale set ID
}

type server struct {
	serverSpec
	id string
}

type scaleSetKey struct {
	pool string // label
	repo string // "" for the organization scale set
}

// id returns a lowercase ULID with the time t.
func (s *seeder) id(t time.Time) string {
	return strings.ToLower(ulid.MustNew(ulid.Timestamp(t), s.src).String())
}

// hex returns n random hex digits.
func (s *seeder) hex(n int) string {
	b := make([]byte, (n+1)/2)
	s.src.Read(b)
	return hex.EncodeToString(b)[:n]
}

// jitter returns d changed by up to ±f of its value.
func (s *seeder) jitter(d time.Duration, f float64) time.Duration {
	return time.Duration(float64(d) * (1 + f*(2*s.rng.Float64()-1)))
}

func pick[T any](s *seeder, list []T) T {
	return list[s.rng.IntN(len(list))]
}

// settings leaves the mode empty: serve sets it from RUNPLANE_MODE.
func (s *seeder) settings(ctx context.Context) error {
	_, err := s.st.UpdateSettings(ctx, func(st *store.Settings) error {
		st.InstanceName = instanceName
		st.PublicURL = publicURL
		return nil
	})
	return err
}

func (s *seeder) users(ctx context.Context) error {
	admin := store.User{
		ID: s.id(s.base), Email: "ada@kestrel.dev", Name: "Ada Okafor", Role: store.RoleAdmin,
		PasswordHash: auth.HashPassword(password), CreatedAt: s.base, LastLoginAt: ptr(s.now.Add(-25 * time.Minute)),
	}
	joined := s.base.Add(50 * time.Hour)
	member := store.User{
		ID: s.id(joined), Email: "mira@kestrel.dev", Name: "Mira Kowalski", Role: store.RoleMember,
		PasswordHash: auth.HashPassword(password), CreatedAt: joined, LastLoginAt: ptr(s.now.Add(-3 * time.Hour)),
	}
	for _, u := range []store.User{admin, member} {
		if err := s.st.CreateUser(ctx, u); err != nil {
			return err
		}
	}
	s.admin, s.member = admin.ID, member.ID
	return nil
}

func (s *seeder) github(ctx context.Context) error {
	err := s.st.SaveGitHubApp(ctx, store.GitHubApp{
		ID: s.id(s.base), AppID: appID, Slug: appSlug, Name: appSlug, ClientID: "Iv23liKq8sXv0kRpl2Tn",
		OwnerLogin: orgLogin, OwnerType: "Organization", HTMLURL: "https://github.com/apps/" + appSlug,
		// Not a key: GitHub calls fail at once instead of reaching api.github.com.
		PrivateKeyEnc:    s.box.Seal([]byte("-----BEGIN RSA PRIVATE KEY-----\nrunplane dev seed\n-----END RSA PRIVATE KEY-----\n")),
		ClientSecretEnc:  s.box.Seal([]byte(s.hex(40))),
		WebhookSecretEnc: s.box.Seal([]byte(s.hex(40))),
		CreatedAt:        s.base,
	})
	if err != nil {
		return err
	}
	installs := []struct {
		inst  store.Installation
		owner string
		names []string
	}{
		{store.Installation{
			ID: orgInstallation, AppID: appID, AccountLogin: orgLogin, AccountID: orgAccountID,
			AccountType: "Organization", RepositorySelection: "all",
		}, orgLogin, orgRepos},
		{store.Installation{
			ID: userInstallation, AppID: appID, AccountLogin: userLogin, AccountID: userAccountID,
			AccountType: "User", RepositorySelection: "selected",
		}, userLogin, userRepos},
	}
	repoID := int64(712348823)
	for i, in := range installs {
		in.inst.AvatarURL = fmt.Sprintf("https://avatars.githubusercontent.com/u/%d?v=4", in.inst.AccountID)
		in.inst.CreatedAt = s.base.Add(time.Duration(i) * 4 * time.Minute)
		in.inst.UpdatedAt = s.now.Add(-4 * time.Minute)
		if err := s.st.UpsertInstallation(ctx, in.inst); err != nil {
			return err
		}
		var repos []store.Repository
		for _, name := range in.names {
			repoID += 1_337_421
			repos = append(repos, store.Repository{
				ID: repoID, FullName: in.owner + "/" + name, Private: name != ".github",
				Archived: name == "status-page", DefaultBranch: "main", UpdatedAt: in.inst.UpdatedAt,
			})
		}
		if err := s.st.ReplaceRepositories(ctx, in.inst.ID, repos); err != nil {
			return err
		}
	}
	return nil
}

func (s *seeder) addServers(ctx context.Context) error {
	for _, spec := range serverSpecs {
		created := s.base.Add(spec.added)
		if spec.status == store.ServerInstalling {
			created = s.now.Add(-3 * time.Minute)
		}
		v := store.Server{
			ID: s.id(created), Name: spec.name, Kind: spec.kind, Status: spec.status, StatusDetail: spec.detail,
			ServerFacts: spec.facts, CreatedAt: created, UpdatedAt: s.now.Add(-spec.seen),
		}
		if spec.seen > 0 {
			v.LastSeenAt = ptr(s.now.Add(-spec.seen))
		}
		if spec.kind == store.ServerAgent && spec.seen > 0 {
			v.SecretHash = s.hex(64)
		}
		if err := s.st.CreateServer(ctx, v); err != nil {
			return err
		}
		if _, err := s.st.SetServerRegion(ctx, v.ID, spec.region); err != nil {
			return err
		}
		s.servers[spec.name] = &server{serverSpec: spec, id: v.ID}
		if err := s.serverStats(ctx, s.servers[spec.name]); err != nil {
			return err
		}
	}
	return nil
}

// images adds the two builtin images and their builds on each server. The
// new server gra-b2-30 still pulls the default image.
func (s *seeder) images(ctx context.Context) error {
	for i, name := range []string{"ubuntu-24.04", "ubuntu-26.04"} {
		added := s.base.Add(time.Duration(i) * 20 * 24 * time.Hour)
		img := store.Image{
			ID: s.id(added), Name: name, Ref: "builtin:" + name, IsDefault: i == 0, Source: "builtin",
			CreatedAt: added, UpdatedAt: added,
		}
		if err := s.st.CreateImage(ctx, img); err != nil {
			return err
		}
		digests := map[string]string{"x64": "sha256:" + s.hex(64), "arm64": "sha256:" + s.hex(64)}
		if img.IsDefault {
			s.digests = digests
		}
		for _, spec := range serverSpecs {
			v := s.servers[spec.name]
			b := store.ImageBuild{
				ImageID: img.ID, ServerID: v.id, Digest: digests[spec.facts.Arch], Status: store.BuildReady,
				SizeBytes: int64(2_412+300*i)*1024*1024 + s.rng.Int64N(1<<20), UpdatedAt: s.now.Add(-time.Duration(1+i) * 26 * time.Hour),
			}
			if spec.facts.Mode == sandbox.ModeVM {
				b.Path = "/var/lib/runplane/images/" + strings.TrimPrefix(b.Digest, "sha256:") + "-3f2a9c1e.ext4"
			}
			if spec.status == store.ServerInstalling {
				b = store.ImageBuild{ImageID: img.ID, ServerID: v.id, Status: store.BuildPending, UpdatedAt: s.now.Add(-time.Minute)}
				if img.IsDefault {
					b.Status = store.BuildPulling
				}
			}
			if err := s.st.UpsertImageBuild(ctx, b); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *seeder) bundles(ctx context.Context) error {
	for _, r := range []struct {
		version string
		age     time.Duration
	}{{runnerVersion, 8 * 24 * time.Hour}, {oldRunnerVersion, 40 * 24 * time.Hour}} {
		released := s.now.Add(-r.age)
		for _, arch := range []string{"x64", "arm64"} {
			err := s.st.UpsertRunnerBundle(ctx, store.RunnerBundle{
				Version: r.version, Arch: arch, SHA256: s.hex(64), Patched: true, Status: store.BundleReady,
				Path: "/var/lib/runplane/runners/" + r.version + "-" + arch, ReleasedAt: &released,
				CreatedAt: later(released.Add(40*time.Minute), s.base),
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *seeder) addPools(ctx context.Context) error {
	for i, spec := range poolSpecs {
		created := s.base.Add(time.Duration(5+i) * time.Minute)
		p := store.Pool{
			ID: s.id(created), Name: spec.name, Label: spec.label, Arch: spec.arch, VCPU: spec.vcpu,
			MemoryMB: spec.memMB, DiskGB: spec.diskGB, MinIdle: spec.minIdle, MaxRunners: spec.max,
			ServerIDs: []string{}, Enabled: true, CreatedAt: created, UpdatedAt: created,
		}
		if err := s.st.CreatePool(ctx, p); err != nil {
			return err
		}
		s.pools[spec.label] = p
	}
	return nil
}

// addScaleSets makes one scale set for each pool in the organization, and
// one for each pool in each repository of ada-okafor. stats has the live
// counts of the organization scale sets.
func (s *seeder) addScaleSets(ctx context.Context, stats map[string]store.ScaleSetStats) error {
	s.scaleSets = map[scaleSetKey]string{}
	githubID := int64(40)
	add := func(key scaleSetKey, installation int64, repoID *int64, poll time.Duration) error {
		githubID++
		x := store.ScaleSet{
			ID: s.id(s.pools[key.pool].CreatedAt), PoolID: s.pools[key.pool].ID, InstallationID: installation,
			RepositoryID: repoID, GitHubID: githubID, RunnerGroupID: 1, Name: key.pool,
			Status: store.ScaleSetListening, LastPollAt: ptr(s.now.Add(-poll)), UpdatedAt: s.now.Add(-poll),
		}
		if key.repo == "" {
			x.Stats = stats[key.pool]
		}
		s.scaleSets[key] = x.ID
		return s.st.UpsertScaleSet(ctx, x)
	}
	for _, p := range poolSpecs {
		// The design shows "last poll 12s ago".
		if err := add(scaleSetKey{pool: p.label}, orgInstallation, nil, 12*time.Second); err != nil {
			return err
		}
	}
	for _, name := range userRepos {
		repo, err := s.st.GetRepositoryByName(ctx, userLogin+"/"+name)
		if err != nil {
			return err
		}
		for _, p := range poolSpecs {
			poll := time.Duration(5+s.rng.IntN(40)) * time.Second
			if err := add(scaleSetKey{pool: p.label, repo: repo.FullName}, userInstallation, &repo.ID, poll); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *seeder) notifications(ctx context.Context) error {
	for i, c := range []struct {
		name, kind, url string
		events          []string
		enabled         bool
	}{
		{"#ci-alerts", "slack", "https://hooks.slack.invalid/services/T05KESTREL/B07RUNPLANE/devseedNotARealToken",
			[]string{"job.failed", "server.unreachable", "disk.low"}, true},
		{"Ops webhook", "webhook", "https://ops.kestrel.invalid/hooks/runplane",
			[]string{"server.unreachable", "backup.failed", "update.available", "runner.deprecation"}, true},
		{"#mobile-builds", "discord", "https://discord.invalid/api/webhooks/1290000000000000000/devseed-not-a-real-token",
			[]string{"job.failed"}, false},
	} {
		created := s.base.Add(time.Duration(3+i) * 24 * time.Hour)
		err := s.st.CreateNotificationChannel(ctx, store.NotificationChannel{
			ID: s.id(created), Name: c.name, Kind: c.kind, URLEnc: s.box.Seal([]byte(c.url)),
			Events: c.events, Enabled: c.enabled, CreatedAt: created, UpdatedAt: created,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// audit writes the changes of the story with the actions and details that
// the API writes (internal/api).
func (s *seeder) audit(ctx context.Context) error {
	type entry struct {
		at             time.Time
		user           string
		action, target string
		detail         map[string]any
	}
	entries := []entry{
		{s.base, s.admin, "setup.done", s.admin, map[string]any{"email": "ada@kestrel.dev"}},
		{s.base.Add(time.Minute), s.admin, "github.manifest", "", map[string]any{"account_type": "organization", "organization": orgLogin}},
		{s.base.Add(2 * time.Minute), s.admin, "github.app.create", "", nil},
		{s.base.Add(3 * time.Minute), s.admin, "github.installation.install", strconv.FormatInt(orgInstallation, 10), nil},
		{s.base.Add(48 * time.Hour), s.admin, "invite.create", s.id(s.base.Add(48 * time.Hour)), map[string]any{"email": "mira@kestrel.dev", "role": store.RoleMember}},
		{s.base.Add(21 * 24 * time.Hour), s.admin, "settings.update", "", map[string]any{
			"keys": []string{"debug_hold_minutes"}, "changes": map[string]any{"debug_hold_minutes": []int{15, 30}},
		}},
		{s.now.Add(-6 * time.Hour), s.admin, "server.update", s.servers["fsn1-ax52-b"].id, map[string]any{
			"name": "fsn1-ax52-b", "status": store.ServerDraining, "changes": map[string]any{"status": []string{store.ServerOnline, store.ServerDraining}},
		}},
		{s.now.Add(-5 * time.Hour), s.admin, "pool.update", s.pools["runplane-8cpu"].ID, map[string]any{
			"label": "runplane-8cpu", "changes": map[string]any{"max_runners": []int{8, 12}},
		}},
		{s.now.Add(-4 * time.Hour), s.admin, "server.update", s.servers["fsn1-ax52-b"].id, map[string]any{
			"name": "fsn1-ax52-b", "status": store.ServerOnline, "changes": map[string]any{"status": []string{store.ServerDraining, store.ServerOnline}},
		}},
		{s.now.Add(-3 * time.Minute), s.admin, "server.install", s.servers["gra-b2-30"].id, map[string]any{
			"name": "gra-b2-30", "host": "51.68.42.110", "port": 22, "user": "root",
		}},
	}
	for i, p := range poolSpecs {
		at := s.base.Add(time.Duration(5+i) * time.Minute)
		entries = append(entries, entry{at, s.admin, "pool.create", s.pools[p.label].ID, map[string]any{"label": p.label}})
	}
	for _, spec := range serverSpecs {
		if spec.kind != store.ServerAgent || spec.status == store.ServerInstalling {
			continue
		}
		at := s.base.Add(spec.added - 5*time.Minute)
		entries = append(entries, entry{at, s.admin, "server.token", s.id(at), map[string]any{"name": spec.name, "expires_at": at.Add(time.Hour)}})
	}
	images, err := s.st.ListImages(ctx)
	if err != nil {
		return err
	}
	for _, img := range images {
		if !img.IsDefault {
			entries = append(entries, entry{img.CreatedAt, s.admin, "image.create", img.ID, map[string]any{"name": img.Name, "ref": img.Ref}})
		}
	}
	rerun := s.now.Add(-26 * time.Hour)
	failed, _, err := s.st.ListJobs(ctx, store.JobFilter{Repository: orgLogin + "/web", Status: store.ResultFailed, Until: rerun, Limit: 1})
	if err != nil {
		return err
	}
	for _, j := range failed {
		entries = append(entries, entry{rerun, s.member, "job.rerun", j.ID, map[string]any{
			"debug": true, "repository": j.Repository, "workflow_file": j.WorkflowFile, "job_name": j.JobName, "run_number": j.RunNumber,
		}})
	}
	for _, e := range entries {
		var detail []byte
		if e.detail != nil {
			if detail, err = json.Marshal(e.detail); err != nil {
				return err
			}
		}
		ip := "203.0.113.24"
		if e.user == s.member {
			ip = "198.51.100.7"
		}
		err = s.st.InsertAudit(ctx, store.AuditEntry{
			ID: s.id(e.at), At: e.at, UserID: e.user, Action: e.action, Target: e.target, Detail: detail, IP: ip,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// lockHash is the stable hash that cache keys and logs of a repository use.
func lockHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func ptr[T any](v T) *T { return &v }

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
