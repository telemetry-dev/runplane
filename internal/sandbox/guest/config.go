package guest

import (
	"net/netip"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

// Config is the job config that runplane-init gets one time at start.
type Config struct {
	SandboxID string `json:"sandbox_id"`
	Hostname  string `json:"hostname"`
	JITConfig string `json:"jit_config"`
	RunnerDir string `json:"runner_dir"` // read-only runner bundle in the guest
	// CacheURL is the cache server of runplane for the cache calls of the
	// job (RunnerEnv). Empty: init removes the runner patch, and the job
	// uses the GitHub cache.
	CacheURL         string       `json:"cache_url"`
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
	var cache, mirror string
	// An unpatched runner overwrites ACTIONS_RESULTS_URL with the GitHub
	// value, so the cache server works only with a patched runner.
	if s.ActionsCache && patched {
		cache = serviceURL
	}
	if s.RegistryMirror {
		mirror = serviceURL
	}
	return Config{
		SandboxID:        spec.ID,
		Hostname:         Hostname(spec.ID),
		JITConfig:        spec.JITConfig,
		RunnerDir:        runnerDir,
		CacheURL:         cache,
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

// RunnerEnv is the runner .env file of a sandbox. It sends the cache calls of
// a patched runner to the cache server at cacheURL, for example
// http://10.0.0.1:8486. The cache server must be at the root path:
// @actions/cache drops the path of ACTIONS_RESULTS_URL. Without a cache
// server, the file is empty.
func RunnerEnv(cacheURL string) []byte {
	if cacheURL == "" {
		return nil
	}
	return []byte("ACTIONS_CACHE_SERVICE_V2=true\n" + resultsURLVar + "=" + strings.TrimRight(cacheURL, "/") + "/\n")
}
