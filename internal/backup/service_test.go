package backup

import (
	"context"
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
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
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
	b := New(Config{Store: st, Bus: &events.Bus{}, Dir: dir})

	id, err := b.Create(ctx)
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
// upload to S3 and a failure. A failed upload fails the backup, and the
// file stays. Only a failure sends a notification.
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
		name     string
		bucket   string
		dir      func(t *testing.T) string
		want     []string
		notified int
	}{
		{"local", "", func(t *testing.T) string { return t.TempDir() }, []string{store.BackupRunning, store.BackupDone}, 0},
		{"upload to S3", "ci-backups", func(t *testing.T) string { return t.TempDir() },
			[]string{store.BackupRunning, store.BackupDone, store.BackupUploading, store.BackupUploaded}, 0},
		{"upload fails", "denied", func(t *testing.T) string { return t.TempDir() },
			[]string{store.BackupRunning, store.BackupDone, store.BackupUploading, store.BackupFailed}, 1},
		{"backup fails", "", func(t *testing.T) string {
			file := filepath.Join(t.TempDir(), "file")
			os.WriteFile(file, nil, 0o600)
			return filepath.Join(file, "backups") // a folder in a file cannot exist
		}, []string{store.BackupRunning, store.BackupFailed}, 1},
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
			notified := 0
			b := New(Config{Store: st, Box: box, Bus: bus, Dir: tt.dir(t), Notify: func(notify.Event) { notified++ }})
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
			if notified != tt.notified {
				t.Errorf("%d notifications, want %d", notified, tt.notified)
			}
			if tt.bucket == "denied" {
				if _, err := os.Stat(last.Path); err != nil || last.Error == "" {
					t.Errorf("failed upload: file %v, error %q; want the file and the error", err, last.Error)
				}
			}
		})
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
