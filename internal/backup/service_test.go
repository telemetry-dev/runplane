package backup

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

// A backup is the database in a gzip file, as the restore steps of the
// README expect. Only root can read it, because it holds password hashes.
func TestCreateWritesTheDatabase(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	dir := t.TempDir()
	if err := st.CreateUser(ctx, store.User{ID: "u1", Email: "ada@example.com", Name: "Ada", PasswordHash: "hash",
		Role: store.RoleAdmin, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	id, err := New(Config{Store: st, Bus: &events.Bus{}, Dir: dir}).Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.GetBackup(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(row.Path)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.BackupDone || row.SizeBytes != info.Size() {
		t.Errorf("backup row %+v, want done with the size %d of the file", row, info.Size())
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("backup mode %o, want 600", mode)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the backups folder has %d files, want only the backup", len(entries))
	}
	restored, err := store.Open(gunzip(t, row.Path))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if u, err := restored.GetUserByEmail(ctx, "ada@example.com"); err != nil || u.ID != "u1" {
		t.Errorf("user of the restored database: %+v, %v; want u1", u, err)
	}
}

// Create keeps the newest backup_keep backups. An older backup loses its
// file and its row together, so the dashboard lists no backup that cannot
// download. Other files in the folder stay.
func TestCreateKeepsTheNewest(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	dir := t.TempDir()
	if _, err := st.UpdateSettings(ctx, func(s *store.Settings) error { s.BackupKeep = 2; return nil }); err != nil {
		t.Fatal(err)
	}
	others := []string{"notes.txt", "runplane-before-upgrade.db.gz"}
	for _, name := range others {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b := New(Config{Store: st, Bus: &events.Bus{}, Dir: dir})
	var ids []string
	for range 3 {
		id, err := b.Create(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	list, err := st.ListBackups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	files := slices.Clone(others)
	for _, row := range list {
		kept = append(kept, row.ID)
		files = append(files, filepath.Base(row.Path))
	}
	if !sameSet(kept, ids[1:]) {
		t.Errorf("backup rows %v, want the two newest %v", kept, ids[1:])
	}
	var left []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if !sameSet(left, files) {
		t.Errorf("files %v, want %v", left, files)
	}
}

func sameSet(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// The settings page follows a backup through backup.state events, also the
// upload to S3 and a failure. A failed upload fails the backup, and the
// file stays.
func TestBackupStateEvents(t *testing.T) {
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if strings.HasPrefix(r.URL.Path, "/denied/") {
			http.Error(w, "<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>", http.StatusForbidden)
		}
	}))
	defer s3.Close()
	box, err := secret.NewBox(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		bucket string
		dir    func(t *testing.T) string
		want   []string
	}{
		{"local", "", func(t *testing.T) string { return t.TempDir() }, []string{store.BackupRunning, store.BackupDone}},
		{"upload to S3", "ci-backups", func(t *testing.T) string { return t.TempDir() },
			[]string{store.BackupRunning, store.BackupDone, store.BackupUploading, store.BackupUploaded}},
		{"upload fails", "denied", func(t *testing.T) string { return t.TempDir() },
			[]string{store.BackupRunning, store.BackupDone, store.BackupUploading, store.BackupFailed}},
		{"backup fails", "", func(t *testing.T) string {
			file := filepath.Join(t.TempDir(), "file")
			os.WriteFile(file, nil, 0o600)
			return filepath.Join(file, "backups") // a folder in a file cannot exist
		}, []string{store.BackupRunning, store.BackupFailed}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			st := openStore(t)
			if tt.bucket != "" {
				_, err := st.UpdateSettings(ctx, func(s *store.Settings) error {
					s.BackupS3 = store.BackupS3{Endpoint: s3.URL, Region: "us-east-1", Bucket: tt.bucket, AccessKey: "AKIDEXAMPLE",
						SecretKeyEnc: box.Seal([]byte("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"))}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			bus := &events.Bus{}
			states := events.Subscribe[events.BackupState](t.Context(), bus)
			b := New(Config{Store: st, Box: box, Bus: bus, Dir: tt.dir(t)})
			b.Create(ctx)
			var seen []string
			var last events.BackupState
			for len(states) > 0 {
				last = <-states
				seen = append(seen, last.Status)
			}
			if !slices.Equal(seen, tt.want) {
				t.Errorf("backup.state events %v, want %v", seen, tt.want)
			}
			if tt.bucket == "denied" {
				if _, err := os.Stat(last.Path); err != nil || last.Error == "" {
					t.Errorf("failed upload: file %v, error %q; want the file and the error", err, last.Error)
				}
			}
		})
	}
}

// A download opens only a backup file in the backups folder.
func TestOpen(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.key")
	for _, path := range []string{filepath.Join(dir, "runplane-a.db.gz"), outside} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for id, path := range map[string]string{
		"here":    filepath.Join(dir, "runplane-a.db.gz"),
		"gone":    filepath.Join(dir, "runplane-b.db.gz"),
		"outside": outside,
	} {
		if err := st.CreateBackup(ctx, store.Backup{ID: id, CreatedAt: time.Now(), Path: path, Status: store.BackupDone}); err != nil {
			t.Fatal(err)
		}
	}
	b := New(Config{Store: st, Bus: &events.Bus{}, Dir: dir})

	for _, tt := range []struct {
		id        string
		err       error
		available bool
	}{
		{"here", nil, true},
		{"gone", ErrNoFile, false},
		{"outside", ErrNoFile, false},
		{"unknown", store.ErrNotFound, false},
	} {
		row, f, err := b.Open(ctx, tt.id)
		if f != nil {
			f.Close()
		}
		if !errors.Is(err, tt.err) || (err == nil) != (f != nil) {
			t.Errorf("Open(%s): file %v, err %v; want err %v", tt.id, f != nil, err, tt.err)
		}
		if got := b.Available(row); got != tt.available {
			t.Errorf("Available(%s) = %v, want %v", tt.id, got, tt.available)
		}
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// gunzip writes the database of a backup to a new file, and returns its path.
func gunzip(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "runplane.db")
	if err := os.WriteFile(out, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}
