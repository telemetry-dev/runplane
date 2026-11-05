package guest

import (
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

func TestNewConfig(t *testing.T) {
	const url = "http://10.214.0.13:8486"
	spec := sandbox.Spec{ID: "01m6jwd94yx8aj3sdamaedysaw", JITConfig: "jit", TimeLimit: 90 * time.Minute, Debug: true}
	all := sandbox.Settings{ActionsCache: true, RegistryMirror: true, DNS: []string{"1.1.1.1", "2606:4700::1111", "9.9.9.9"}}
	cases := []struct {
		name       string
		settings   sandbox.Settings
		patched    bool
		env        map[string]string
		wantEnv    map[string]string
		wantCache  bool
		wantMirror string
		wantDNS    []string
	}{
		{
			name:       "cache and mirror",
			settings:   all,
			patched:    true,
			env:        map[string]string{"FOO": "bar"},
			wantEnv:    map[string]string{"FOO": "bar", "ACTIONS_RESULTS_URL": "http://10.214.0.13:8486/", "ACTIONS_CACHE_SERVICE_V2": "true"},
			wantCache:  true,
			wantMirror: url,
			wantDNS:    []string{"1.1.1.1", "9.9.9.9"},
		},
		{
			name:     "unpatched runner keeps the GitHub cache",
			settings: sandbox.Settings{ActionsCache: true},
			env:      map[string]string{"FOO": "bar"},
			wantEnv:  map[string]string{"FOO": "bar"},
		},
		{
			name:     "cache off",
			settings: sandbox.Settings{},
			patched:  true,
			wantEnv:  map[string]string{},
		},
		{
			name:      "the spec env wins",
			settings:  sandbox.Settings{ActionsCache: true},
			patched:   true,
			env:       map[string]string{"ACTIONS_RESULTS_URL": "http://other/"},
			wantEnv:   map[string]string{"ACTIONS_RESULTS_URL": "http://other/", "ACTIONS_CACHE_SERVICE_V2": "true"},
			wantCache: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := spec
			spec.Env = c.env
			got := NewConfig(spec, c.settings, url, "/.runplane/runner", c.patched)
			if !maps.Equal(got.Env, c.wantEnv) || got.ActionsCache != c.wantCache {
				t.Errorf("env %v, actions cache %v; want %v, %v", got.Env, got.ActionsCache, c.wantEnv, c.wantCache)
			}
			if got.RegistryMirror != c.wantMirror {
				t.Errorf("registry mirror %q, want %q", got.RegistryMirror, c.wantMirror)
			}
			if !reflect.DeepEqual(got.DNS, c.wantDNS) {
				t.Errorf("DNS %v, want the IPv4 servers %v", got.DNS, c.wantDNS)
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
