package version

import "testing"

func TestIsRelease(t *testing.T) {
	tests := []struct {
		v    string
		want bool
	}{
		{"v1.2.0", true},
		{"v1.3.0-rc.1", true}, // release.yml publishes it as a pre-release
		{"dev", false},
		{"1.2.0", false}, // the release tags start with v
		{"v1.2.0-3-g3f2a9c1", false},
		{"v1.3.0-rc.1-2-g3f2a9c1", false},
		{"v1.2.0-dirty", false},
		{"3f2a9c1", false},
	}
	for _, tt := range tests {
		if got := IsRelease(tt.v); got != tt.want {
			t.Errorf("IsRelease(%q) = %v, want %v", tt.v, got, tt.want)
		}
	}
}

func TestCompare(t *testing.T) {
	// Each version is older than the next one (semver.org, section 11).
	ordered := []string{
		"v0.4.2",
		"v0.10.0",
		"v1.0.0-alpha",
		"v1.0.0-alpha.1",
		"v1.0.0-alpha.beta",
		"v1.0.0-beta",
		"v1.0.0-beta.2",
		"v1.0.0-beta.11",
		"v1.0.0-rc.1",
		"1.0.0",
		"v1.0.1",
		"v1.2.0",
		"v10.0.0",
	}
	for i := 0; i+1 < len(ordered); i++ {
		a, b := ordered[i], ordered[i+1]
		if Compare(a, b) != -1 || Compare(b, a) != 1 {
			t.Errorf("Compare(%s, %s) = %d, want -1", a, b, Compare(a, b))
		}
	}
	equal := [][2]string{{"v1.2.3", "1.2.3"}, {"v1.2.3+build.5", "v1.2.3"}}
	for _, p := range equal {
		if c := Compare(p[0], p[1]); c != 0 {
			t.Errorf("Compare(%s, %s) = %d, want 0", p[0], p[1], c)
		}
	}
	for _, bad := range []string{"dev", "v1.2", "v1.2.x", "v1.2.3-", "v+1.2.3"} {
		if Compare(bad, "v0.0.1") != -1 {
			t.Errorf("invalid version %q does not sort first", bad)
		}
	}
}
