package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "runplane.db"))
	must(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func addJob(t *testing.T, s *Store, m JobMessage) Job {
	t.Helper()
	j, err := s.UpsertJob(t.Context(), m, time.Now())
	must(t, err)
	return j
}

var t0 = time.UnixMilli(1_700_000_000_000).UTC()

func sec(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }

// Open applies each migration one time, and refuses a database from a
// newer runplane.
func TestOpenMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runplane.db")
	s, err := Open(path)
	must(t, err)
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_, err = s.w.Exec("PRAGMA user_version = 999")
	must(t, err)
	s.Close()
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("Open accepted a database from a newer runplane")
	}
}

func TestVacuumIntoWithDB(t *testing.T) {
	s := openTest(t)
	_, err := s.UpdateSettings(t.Context(), func(st *Settings) error {
		st.InstanceName = "copied"
		return nil
	})
	must(t, err)
	path := filepath.Join(t.TempDir(), "backup.db")
	if _, err := s.DB().Exec("VACUUM INTO ?", path); err != nil {
		t.Fatalf("VACUUM INTO: %v", err)
	}
	b, err := Open(path)
	must(t, err)
	defer b.Close()
	if st, err := b.GetSettings(t.Context()); err != nil || st.InstanceName != "copied" {
		t.Fatalf("backup settings: %q, %v", st.InstanceName, err)
	}
}
