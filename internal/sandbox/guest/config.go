package guest

import (
	"net/netip"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

// Config is the job config that runplane-init gets one time at start.
type Config struct {
	SandboxID string            `json:"sandbox_id"`
	Hostname  string            `json:"hostname"`
	JITConfig string            `json:"jit_config"`
	RunnerDir string            `json:"runner_dir"` // read-only runner bundle in the guest
	Env       map[string]string `json:"env"`        // written to the runner .env
	// ActionsCache tells that Env sends the cache calls of the job to the
	// cache server of runplane. Without it, init removes the runner patch.
	ActionsCache     bool         `json:"actions_cache"`
	DNS              []netip.Addr `json:"dns"`
	RegistryMirror   string       `json:"registry_mirror"` // http://<service-ip>:<port>, empty = none
	TimeLimitSeconds int          `json:"time_limit_seconds"`
	Debug            bool         `json:"debug"`
}

// NewConfig is the config of a new sandbox. serviceURL is the cache server
// and the registry mirror as the guest reaches them. runnerDir is the runner
// bundle in the guest, and patched tells if the bundle has the runner patch
// (Patch).
func NewConfig(spec sandbox.Spec, s sandbox.Settings, serviceURL, runnerDir string, patched bool) Config {
	// An unpatched runner overwrites ACTIONS_RESULTS_URL with the GitHub
	// value, so the cache server works only with a patched runner.
	cache := s.ActionsCache && patched
	var env map[string]string
	if cache {
		env = cacheEnv(serviceURL)
	}
	var mirror string
	if s.RegistryMirror {
		mirror = serviceURL
	}
	return Config{
		SandboxID:        spec.ID,
		Hostname:         Hostname(spec.ID),
		JITConfig:        spec.JITConfig,
		RunnerDir:        runnerDir,
		Env:              env,
		ActionsCache:     cache,
		DNS:              s.DNS,
		RegistryMirror:   mirror,
		TimeLimitSeconds: int(spec.TimeLimit / time.Second),
		Debug:            spec.Debug,
	}
}

// Hostname is the host name of a sandbox. It uses the random end of the
// ULID, as the runner name does. The start is the time, which sandboxes of
// the same second share.
func Hostname(id string) string { return "runplane-" + id[max(len(id)-8, 0):] }

// cacheEnv is the runner .env that sends the cache calls of a patched runner
// to the cache server at serviceURL, for example http://10.0.0.1:8486. The
// cache server must be at the root path: @actions/cache drops the path of
// ACTIONS_RESULTS_URL.
func cacheEnv(serviceURL string) map[string]string {
	return map[string]string{
		resultsURLVar:              strings.TrimRight(serviceURL, "/") + "/",
		"ACTIONS_CACHE_SERVICE_V2": "true",
	}
}
