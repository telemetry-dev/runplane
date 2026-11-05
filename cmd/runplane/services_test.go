package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/update"
)

// A backup over backup_keep loses its file and its row together, so the
// dashboard lists no backup that cannot download.
func TestBackupsRemoveTheRowsOfPrunedFiles(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	dir := t.TempDir()
	if _, err := st.UpdateSettings(ctx, func(s *store.Settings) error { s.BackupKeep = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "runplane-20200101T000000.000Z.db.gz")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateBackup(ctx, store.Backup{ID: "old", CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		Path: old, SizeBytes: 3, Status: store.BackupDone}); err != nil {
		t.Fatal(err)
	}
	b := &backups{st: st, bus: &events.Bus{}, dir: dir, log: slog.New(slog.DiscardHandler)}

	id, err := b.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	list, err := st.ListBackups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != id || list[0].Status != store.BackupDone || list[0].SizeBytes == 0 {
		t.Fatalf("backups: %+v, want only the new one, done", list)
	}
	if _, err := os.Stat(list[0].Path); err != nil {
		t.Errorf("the new backup file: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the old backup file is still there: %v", err)
	}
}

// The settings page follows a backup through backup.state events, also the
// upload to S3 and a failure. Only a failure sends a notification.
func TestBackupStateEvents(t *testing.T) {
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	defer s3.Close()
	box, err := secret.NewBox(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		bucket   bool
		dir      func(t *testing.T) string
		want     []string
		notified int
	}{
		{"local", false, func(t *testing.T) string { return t.TempDir() }, []string{store.BackupRunning, store.BackupDone}, 0},
		{"upload to S3", true, func(t *testing.T) string { return t.TempDir() },
			[]string{store.BackupRunning, store.BackupDone, store.BackupUploading, store.BackupUploaded}, 0},
		{"backup fails", false, func(t *testing.T) string {
			file := filepath.Join(t.TempDir(), "file")
			os.WriteFile(file, nil, 0o600)
			return filepath.Join(file, "backups") // a folder in a file cannot exist
		}, []string{store.BackupRunning, store.BackupFailed}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			st := openStore(t)
			if tt.bucket {
				_, err := st.UpdateSettings(ctx, func(s *store.Settings) error {
					s.BackupS3 = store.BackupS3{Endpoint: s3.URL, Region: "us-east-1", Bucket: "ci-backups", AccessKey: "AKIDEXAMPLE",
						SecretKeyEnc: box.Seal([]byte("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"))}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			bus := &events.Bus{}
			states, stop := bus.Subscribe(16)
			notified := 0
			b := &backups{st: st, box: box, bus: bus, dir: tt.dir(t), notify: func(notify.Event) { notified++ }, log: slog.New(slog.DiscardHandler)}
			b.Run(ctx)
			stop()
			var seen []string
			for e := range states {
				if row, ok := e.Data.(store.Backup); ok && e.Type == events.BackupState {
					seen = append(seen, row.Status)
				}
			}
			if !slices.Equal(seen, tt.want) {
				t.Errorf("backup.state events %v, want %v", seen, tt.want)
			}
			if notified != tt.notified {
				t.Errorf("%d notifications, want %d", notified, tt.notified)
			}
		})
	}
}

// The dashboard reads the update status on each visit. A repository with no
// release has no update, and GitHub sees one read in checkTTL.
func TestUpdaterCheck(t *testing.T) {
	var reads, status atomic.Int32
	status.Store(http.StatusNotFound)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	u := &updater{u: &update.Updater{Repo: "runplane/runplane", Current: "v1.0.0", APIURL: srv.URL}, log: slog.New(slog.DiscardHandler)}
	ctx := context.Background()

	for range 2 {
		s, err := u.Check(ctx)
		if err != nil || s.Available || s.Current != "v1.0.0" {
			t.Fatalf("Check = %+v, %v; want v1.0.0 without an update", s, err)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("%d release reads for two checks, want 1", n)
	}

	// A GitHub fault is not kept, so the next visit reads again.
	u.checked = time.Time{}
	status.Store(http.StatusBadGateway)
	if _, err := u.Check(ctx); err == nil {
		t.Fatal("Check with a GitHub fault returned no error")
	}
	u.Check(ctx)
	if n := reads.Load(); n != 3 {
		t.Errorf("%d release reads after two failed checks, want 3", n)
	}
}
