package guest

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

func TestNewConfig(t *testing.T) {
	const url = "http://10.214.0.13:8486"
	spec := sandbox.Spec{ID: "01m6jwd94yx8aj3sdamaedysaw", JITConfig: "jit", TimeLimit: 90 * time.Minute, Debug: true}
	dns := []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("9.9.9.9")}
	cases := []struct {
		name       string
		settings   sandbox.Settings
		patched    bool
		wantCache  string
		wantMirror string
		wantDNS    []netip.Addr
	}{
		{
			name:       "cache and mirror",
			settings:   sandbox.Settings{ActionsCache: true, RegistryMirror: true, DNS: dns},
			patched:    true,
			wantCache:  url,
			wantMirror: url,
			wantDNS:    dns,
		},
		{
			name:     "unpatched runner keeps the GitHub cache",
			settings: sandbox.Settings{ActionsCache: true},
		},
		{
			name:     "cache off",
			settings: sandbox.Settings{},
			patched:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NewConfig(spec, c.settings, url, "/.runplane/runner", c.patched)
			if got.CacheURL != c.wantCache {
				t.Errorf("cache URL %q, want %q", got.CacheURL, c.wantCache)
			}
			if got.RegistryMirror != c.wantMirror {
				t.Errorf("registry mirror %q, want %q", got.RegistryMirror, c.wantMirror)
			}
			if !slices.Equal(got.DNS, c.wantDNS) {
				t.Errorf("DNS %v, want %v", got.DNS, c.wantDNS)
			}
			// The first ULID characters hold the time, which sandboxes of
			// the same second share; the host name takes the random end.
			if got.SandboxID != spec.ID || got.Hostname != "runplane-maedysaw" || got.JITConfig != "jit" ||
				got.RunnerDir != "/.runplane/runner" || got.TimeLimitSeconds != 5400 || !got.Debug {
				t.Errorf("config %+v", got)
			}
		})
	}
}

// @actions/cache drops the path of ACTIONS_RESULTS_URL and adds its own after
// a slash.
func TestRunnerEnv(t *testing.T) {
	want := "ACTIONS_CACHE_SERVICE_V2=true\nACTIONS_RESULTS_URL=http://10.214.0.13:8486/\n"
	if got := string(RunnerEnv("http://10.214.0.13:8486")); got != want {
		t.Errorf("RunnerEnv = %q, want %q", got, want)
	}
	if got := RunnerEnv(""); len(got) != 0 {
		t.Errorf("RunnerEnv without a cache = %q, want an empty file", got)
	}
}

func TestHostnameOfAShortID(t *testing.T) {
	if got := Hostname("vm1"); got != "runplane-vm1" {
		t.Errorf("Hostname(vm1) = %q, want runplane-vm1", got)
	}
}
