package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

func TestSettingsDefaultsAndUpdate(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	_, err := s.w.Exec(`INSERT INTO settings (key, value) VALUES ('future_key', '{"a":1}')`)
	must(t, err)
	if st, err := s.GetSettings(ctx); err != nil || !reflect.DeepEqual(st, DefaultSettings()) {
		t.Fatalf("settings = %+v, %v; want the defaults", st, err)
	}

	_, err = s.UpdateSettings(ctx, func(st *Settings) error {
		st.InstanceName = "ci"
		st.RegistryMirror = false
		st.DNSServers[0] = "9.9.9.9" // a change in place must also be written
		st.BackupS3.Bucket = "backups"
		return nil
	})
	must(t, err)
	st, err := s.GetSettings(ctx)
	must(t, err)
	if st.InstanceName != "ci" || st.RegistryMirror || st.BackupS3.Bucket != "backups" ||
		!slices.Equal(st.DNSServers, []string{"9.9.9.9", "8.8.8.8"}) {
		t.Fatalf("after update: %+v", st)
	}

	keys, err := queryAll(ctx, s.r, "list keys", scanValue[string], "SELECT key FROM settings ORDER BY key")
	must(t, err)
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

func TestDashboardURL(t *testing.T) {
	tests := []struct {
		name string
		s    Settings
		want string
	}{
		{"the public URL wins", Settings{PublicURL: "https://ci.example.com", Domain: "runplane.example.com"}, "https://ci.example.com"},
		{"the domain gets HTTPS", Settings{Domain: "runplane.example.com"}, "https://runplane.example.com"},
		{"no URL", Settings{}, ""},
	}
	for _, tt := range tests {
		if got := tt.s.DashboardURL(); got != tt.want {
			t.Errorf("%s: DashboardURL() = %q, want %q", tt.name, got, tt.want)
		}
	}
}
