package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The columns of docs/architecture.md section 6.
var contractSchema = map[string]string{
	"settings":              "key value",
	"users":                 "id email name password_hash role created_at last_login_at",
	"sessions":              "id_hash user_id created_at expires_at last_seen_at ip user_agent",
	"invites":               "id email role token_hash created_by created_at expires_at accepted_at",
	"setup_codes":           "code_hash created_at expires_at used_at",
	"github_apps":           "id app_id slug name client_id owner_login owner_type html_url private_key_enc client_secret_enc webhook_secret_enc created_at",
	"manifest_states":       "state_hash account_type organization created_at expires_at",
	"installations":         "id app_id account_login account_id account_type avatar_url repository_selection suspended_at created_at updated_at",
	"repositories":          "id installation_id full_name private archived default_branch updated_at",
	"images":                "id name ref is_default source created_at updated_at",
	"image_builds":          "image_id server_id digest status error size_bytes path updated_at",
	"pools":                 "id name label arch vcpu memory_mb disk_gb image_id min_idle max_runners time_limit_minutes server_ids enabled created_at updated_at",
	"scale_sets":            "id pool_id installation_id repository_id github_id runner_group_id name status error last_poll_at stats_json updated_at",
	"servers":               "id name kind status status_detail address provider region os kernel arch mode vcpu memory_mb disk_bytes disk_kind kvm docker_version sysbox_version firecracker_version version secret_hash last_seen_at created_at updated_at",
	"server_doctor":         "server_id checked_at checks_json",
	"server_tokens":         "id token_hash name created_by created_at expires_at used_at server_id",
	"runner_bundles":        "version arch sha256 patched status path error end_date released_at created_at",
	"sandboxes":             "id pool_id server_id scale_set_id mode state runner_id runner_name ip image_digest runner_version job_id debug hold_until vcpu memory_mb disk_gb created_at idle_at busy_at stopped_at exit_reason error",
	"jobs":                  "id runner_request_id github_job_id scale_set_id pool_id sandbox_id server_id repository workflow_ref workflow_file workflow_name job_name run_id run_number run_attempt head_branch head_sha commit_message event actor pr_number base_ref labels_json state result reject_reason queued_at assigned_at started_at finished_at html_url steps_json log_state cpu_peak cpu_avg mem_peak_mb debug enriched_at",
	"job_metrics":           "job_id t_ms cpu_percent memory_mb",
	"cache_entries":         "id repository ref key version size_bytes path finalized created_at last_used_at",
	"cache_stats":           "hour repository hits misses bytes_read bytes_written",
	"server_stats":          "server_id t_ms cpu_percent memory_percent disk_percent",
	"notification_channels": "id name kind url_enc events_json enabled created_at updated_at last_sent_at last_error",
	"backups":               "id created_at path size_bytes s3_key status error",
	"audit_log":             "id at user_id action target detail_json ip",
}

func TestMigrationsMakeContractSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runplane.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for table, cols := range contractSchema {
		rows, err := s.r.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
		got, err := scanAll(rows, err, scanString)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Fields(cols)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("table %s columns:\n got %v\nwant %v", table, got, want)
		}
	}
	for _, idx := range []struct{ table, column string }{
		{"jobs", "queued_at"}, {"jobs", "state"}, {"jobs", "repository"}, {"jobs", "pool_id"},
		{"jobs", "server_id"}, {"sandboxes", "state"}, {"audit_log", "at"},
	} {
		var n int
		err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM pragma_index_list(?) l
			JOIN pragma_index_info(l.name) i WHERE i.seqno = 0 AND i.name = ?`, idx.table, idx.column).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("no index starts with %s(%s)", idx.table, idx.column)
		}
	}
	s.Close()

	// A second open finds the schema current and applies nothing again.
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s.Close()
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runplane.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("Open accepted a database from a newer runplane")
	}
}

func TestVacuumIntoWithDB(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.UpdateSettings(ctx, func(st *Settings) error { st.InstanceName = "copied"; return nil }); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "backup.db")
	if _, err := s.DB().Exec("VACUUM INTO ?", path); err != nil {
		t.Fatalf("VACUUM INTO: %v", err)
	}
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if st, err := b.GetSettings(ctx); err != nil || st.InstanceName != "copied" {
		t.Fatalf("backup settings: %q, %v", st.InstanceName, err)
	}
}

func TestSettingsDefaultsAndUpdate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.w.Exec(`INSERT INTO settings (key, value) VALUES ('future_key', '{"a":1}')`); err != nil {
		t.Fatal(err)
	}

	st, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.InstanceName != "runplane" || st.ReserveVCPU != 1 || st.ReserveMemoryMB != 2048 || st.CPUOvercommit != 1 ||
		st.MinFreeDiskPercent != 15 || st.DefaultTimeLimitMinutes != 360 || st.AllowPublicRepos || st.AllowForkPRs ||
		len(st.EgressAllowlist) != 0 || st.BandwidthMbps != 0 || !st.RegistryMirror || !st.ActionsCache ||
		st.CacheMaxGB != 50 || st.JobRetentionDays != 90 || st.LogRetentionDays != 30 || st.DebugHoldMinutes != 30 ||
		st.AutoUpdate || !st.BackupEnabled || st.BackupKeep != 7 ||
		!slices.Equal(st.DNSServers, []string{"1.1.1.1", "8.8.8.8"}) {
		t.Fatalf("defaults: %+v", st)
	}

	_, err = s.UpdateSettings(ctx, func(st *Settings) error {
		st.InstanceName = "ci"
		st.RegistryMirror = false
		st.DNSServers[0] = "9.9.9.9" // a change in place must also be written
		st.BackupS3.Bucket = "backups"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err = s.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.InstanceName != "ci" || st.RegistryMirror || st.BackupS3.Bucket != "backups" ||
		!slices.Equal(st.DNSServers, []string{"9.9.9.9", "8.8.8.8"}) {
		t.Fatalf("after update: %+v", st)
	}

	rows, err := s.r.Query("SELECT key FROM settings ORDER BY key")
	keys, err := scanAll(rows, err, scanString)
	if err != nil {
		t.Fatal(err)
	}
	// The unknown key stays. The unchanged keys are not written, so a new
	// default in a later version applies to them.
	want := []string{"backup_s3", "dns_servers", "future_key", "instance_name", "registry_mirror"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}

	boom := errors.New("boom")
	_, err = s.UpdateSettings(ctx, func(st *Settings) error {
		st.InstanceName = "lost"
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if st, _ := s.GetSettings(ctx); st.InstanceName != "ci" {
		t.Fatalf("instance name = %q after a failed update", st.InstanceName)
	}
}

func TestServerTokenSingleUse(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	for _, tok := range []ServerToken{
		{ID: "t1", TokenHash: "h-ok", Name: "build-2", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{ID: "t2", TokenHash: "h-old", Name: "old", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
	} {
		if err := s.CreateServerToken(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}
	newServer := func(id string) Server {
		return Server{ID: id, Kind: ServerAgent, Status: ServerOnline, SecretHash: "secret-" + id, CreatedAt: now, UpdatedAt: now}
	}

	srv, err := s.ConsumeServerToken(ctx, "h-ok", now, newServer("s1"))
	if err != nil {
		t.Fatal(err)
	}
	if srv.Name != "build-2" {
		t.Errorf("server name = %q, want the token name", srv.Name)
	}
	for _, tc := range []struct{ hash, server string }{
		{"h-ok", "s2"},      // used
		{"h-old", "s3"},     // expired
		{"h-unknown", "s4"}, // unknown
	} {
		if _, err := s.ConsumeServerToken(ctx, tc.hash, now, newServer(tc.server)); !errors.Is(err, ErrNotFound) {
			t.Errorf("token %s: err = %v, want ErrNotFound", tc.hash, err)
		}
	}

	list, err := s.ListServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "s1" {
		t.Fatalf("servers = %+v, want only s1", list)
	}
	if got, err := s.GetServerBySecretHash(ctx, "secret-s1"); err != nil || got.ID != "s1" {
		t.Fatalf("by secret: %q, %v", got.ID, err)
	}
}

func TestServerTokenForSSHInstall(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	pending := Server{ID: "s-ssh", Name: "edge-1", Kind: ServerAgent, Status: ServerInstalling, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateServer(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateServerToken(ctx, ServerToken{ID: "t1", TokenHash: "h", Name: "ignored", ServerID: "s-ssh",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	v := Server{ID: "new-id", Kind: ServerAgent, Status: ServerOnline, SecretHash: "secret",
		ServerFacts: ServerFacts{Arch: "arm64", VCPU: 8}, CreatedAt: now, UpdatedAt: now}
	got, err := s.ConsumeServerToken(ctx, "h", now, v)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "s-ssh" || got.Name != "edge-1" || got.Status != ServerOnline || got.SecretHash != "secret" || got.VCPU != 8 {
		t.Fatalf("server = %+v, want the SSH server online", got)
	}
	if list, _ := s.ListServers(ctx); len(list) != 1 {
		t.Fatalf("servers = %d, want 1", len(list))
	}
}

func TestSetSandboxStateTimes(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	if err := s.InsertSandbox(ctx, Sandbox{ID: "b1", PoolID: "p", ServerID: "v", Mode: sandbox.ModeContainer,
		State: sandbox.StateStarting, VCPU: 2, MemoryMB: 4096, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	var b Sandbox
	for _, step := range []struct {
		state  sandbox.State
		reason string
		sec    int
	}{
		{sandbox.StateIdle, "", 1},
		{sandbox.StateIdle, "", 5}, // a repeat keeps the first time
		{sandbox.StateBusy, "", 9},
		{sandbox.StateStopping, "job done", 20},
		{sandbox.StateGone, "removed", 21},
		{sandbox.StateIdle, "", 30}, // a done sandbox keeps its state
	} {
		var err error
		b, err = s.SetSandboxState(ctx, "b1", step.state, step.reason, t0.Add(time.Duration(step.sec)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}

	if b.State != sandbox.StateGone {
		t.Errorf("state = %s, want gone", b.State)
	}
	for _, c := range []struct {
		name string
		got  *time.Time
		sec  int
	}{{"idle_at", b.IdleAt, 1}, {"busy_at", b.BusyAt, 9}, {"stopped_at", b.StoppedAt, 21}} {
		if c.got == nil || !c.got.Equal(t0.Add(time.Duration(c.sec)*time.Second)) {
			t.Errorf("%s = %v, want t0+%ds", c.name, c.got, c.sec)
		}
	}
	if b.ExitReason != "job done" || b.Error != "" {
		t.Errorf("exit reason %q, error %q", b.ExitReason, b.Error)
	}

	if _, err := s.SetSandboxState(ctx, "missing", sandbox.StateIdle, "", t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown sandbox: err = %v, want ErrNotFound", err)
	}
}

func TestMatchCacheEntry(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	t0 := time.UnixMilli(1_700_000_000_000)
	add := func(id, ref, key string, sec int, finalized bool) {
		t.Helper()
		err := s.CreateCacheEntry(ctx, CacheEntry{ID: id, Repository: "o/r", Ref: ref, Key: key, Version: "v1",
			Finalized: finalized, CreatedAt: t0.Add(time.Duration(sec) * time.Second), LastUsedAt: t0})
		if err != nil {
			t.Fatal(err)
		}
	}
	add("main-old", "refs/heads/main", "npm-linux-aaa", 1, true)
	add("main-new", "refs/heads/main", "npm-linux-bbb", 2, true)
	add("pr", "refs/pull/1/merge", "npm-linux-ccc", 0, true)
	add("pr-upload", "refs/pull/1/merge", "npm-linux-ddd", 9, false)
	add("upper", "refs/pull/1/merge", "NPM-linux-eee", 8, true)

	refs := []string{"refs/pull/1/merge", "refs/heads/main"}
	tests := []struct {
		name   string
		key    string
		prefix bool
		refs   []string
		want   string
	}{
		{"first ref wins over newer entries in later refs", "npm-linux-", true, refs, "pr"},
		{"newest entry in the ref", "npm-linux-", true, []string{"refs/heads/main"}, "main-new"},
		{"exact key falls back to the next ref", "npm-linux-aaa", false, refs, "main-old"},
		{"exact key does not match a prefix", "npm-linux", false, refs, ""},
		{"prefix is case-sensitive", "NPM-", true, refs, "upper"},
		{"unfinished upload does not match", "npm-linux-ddd", false, refs, ""},
		{"underscore is not a wildcard", "npm_linux", true, refs, ""},
	}
	for _, tt := range tests {
		e, err := s.MatchCacheEntry(ctx, "o/r", "v1", tt.refs, tt.key, tt.prefix)
		if tt.want == "" {
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("%s: got %q, %v; want ErrNotFound", tt.name, e.ID, err)
			}
			continue
		}
		if err != nil || e.ID != tt.want {
			t.Errorf("%s: got %q, %v; want %q", tt.name, e.ID, err, tt.want)
		}
	}

	err := s.CreateCacheEntry(ctx, CacheEntry{ID: "dup", Repository: "o/r", Ref: "refs/heads/main",
		Key: "npm-linux-aaa", Version: "v1", CreatedAt: t0, LastUsedAt: t0})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate key: err = %v, want ErrConflict", err)
	}
}
