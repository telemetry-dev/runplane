package guest

import (
	"maps"
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
		wantEnv    map[string]string
		wantCache  bool
		wantMirror string
		wantDNS    []netip.Addr
	}{
		{
			name:       "cache and mirror",
			settings:   sandbox.Settings{ActionsCache: true, RegistryMirror: true, DNS: dns},
			patched:    true,
			wantEnv:    map[string]string{"ACTIONS_RESULTS_URL": "http://10.214.0.13:8486/", "ACTIONS_CACHE_SERVICE_V2": "true"},
			wantCache:  true,
			wantMirror: url,
			wantDNS:    dns,
		},
		{
			name:     "unpatched runner keeps the GitHub cache",
			settings: sandbox.Settings{ActionsCache: true},
			wantEnv:  map[string]string{},
		},
		{
			name:     "cache off",
			settings: sandbox.Settings{},
			patched:  true,
			wantEnv:  map[string]string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NewConfig(spec, c.settings, url, "/.runplane/runner", c.patched)
			if !maps.Equal(got.Env, c.wantEnv) || got.ActionsCache != c.wantCache {
				t.Errorf("env %v, actions cache %v; want %v, %v", got.Env, got.ActionsCache, c.wantEnv, c.wantCache)
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

func TestHostnameOfAShortID(t *testing.T) {
	if got := Hostname("vm1"); got != "runplane-vm1" {
		t.Errorf("Hostname(vm1) = %q, want runplane-vm1", got)
	}
}
