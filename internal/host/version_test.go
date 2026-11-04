package host

import (
	"os"
	"testing"
)

func TestAtLeast(t *testing.T) {
	tests := []struct {
		have, want string
		ok         bool
	}{
		{"29.5.0", "29.5", true},
		{"29.10.1", "29.5", true}, // numbers, not text
		{"28.5.2", "29.5", false},
		{"30.0.0", "29.5", true},
		{"0.7.0", "0.7.1", false},
		{"0.7.1", "0.7.1", true},
		{"0.7.1-0.linux", "0.7.1", true},
		{"0.10.0", "0.7.1", true},
		{"0.6.7+dev", "0.7.1", false},
		{"v1.13.1", "1.13", true},
		{"6.8.0-48-generic", "5.12", true},
		{"5.4.0-200-generic", "5.12", false},
		{"", "0.7.1", false},
		{"unknown", "0.7.1", false},
		{"29", "29.5", false}, // a major number alone is not a version
	}
	for _, tt := range tests {
		if got := atLeast(tt.have, tt.want); got != tt.ok {
			t.Errorf("atLeast(%q, %q) = %v, want %v", tt.have, tt.want, got, tt.ok)
		}
	}
}

func TestFirstVersion(t *testing.T) {
	for file, want := range map[string]string{
		"firecracker": "1.13.1",
		"jailer":      "1.13.1",
		"sysbox-runc": "0.7.1",
	} {
		b, err := os.ReadFile("testdata/tools/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if got := firstVersion(string(b)); got != want {
			t.Errorf("%s: got %q, want %q", file, got, want)
		}
	}
}
