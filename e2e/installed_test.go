//go:build e2e

package e2e

import (
	"bufio"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/github/fakegh"
)

// newInstalledEnv targets the runplane that install.sh installed on this
// host: the systemd unit runplane with the configuration in
// /etc/runplane/runplane.env. E2E_FAKEGH is the URL of the fakegh server
// that runplane uses as GitHub. The test must run as root.
func newInstalledEnv(t *testing.T) *env {
	fakeURL := os.Getenv("E2E_FAKEGH")
	if fakeURL == "" {
		t.Skip("E2E_FAKEGH is not set; e2e/linux/run.sh runs this test")
	}
	cfg := readEnvFile(t, "/etc/runplane/runplane.env")
	port, _ := strconv.Atoi(cfg["RUNPLANE_SERVICE_PORT"])
	e := &env{
		t: t, mode: cfg["RUNPLANE_MODE"], unit: "runplane", since: time.Now(),
		dataDir: cfg["RUNPLANE_DATA_DIR"], configDir: cfg["RUNPLANE_CONFIG_DIR"],
		fake: remoteFake{t: t, api: &client{base: fakeURL, http: &http.Client{Timeout: 30 * time.Second}}}, fakeURL: fakeURL,
		addr: localAddr(cfg["RUNPLANE_LISTEN"]), sshAddr: localAddr(cfg["RUNPLANE_SSH_LISTEN"]), servicePort: port,
	}
	e.api = newClient("http://" + e.addr)
	e.events = &eventLog{}
	t.Cleanup(e.logTimings)
	return e
}

func (e *env) systemctl(action string) {
	e.t.Helper()
	if out, err := exec.Command("systemctl", action, e.unit).CombinedOutput(); err != nil {
		e.t.Fatalf("systemctl %s %s: %v\n%s", action, e.unit, err, out)
	}
}

// readEnvFile reads the NAME=value lines of a systemd environment file.
func readEnvFile(t *testing.T, path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("read the runplane configuration: %v", err)
	}
	defer f.Close()
	vars := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && !strings.HasPrefix(k, "#") {
			vars[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return vars
}

// localAddr turns a listen address such as ":8484" into one to dial.
func localAddr(listen string) string {
	host, port, _ := net.SplitHostPort(listen)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// remoteFake is a fakegh server in another process, through its control
// API under /_fake/.
type remoteFake struct {
	t   *testing.T
	api *client
}

func (r remoteFake) Queue(spec fakegh.JobSpec) int64 {
	var out struct {
		RequestID int64 `json:"request_id"`
	}
	r.call("POST", "/_fake/jobs", spec, &out)
	return out.RequestID
}

func (r remoteFake) Jobs() (jobs []fakegh.Job) {
	r.call("GET", "/_fake/jobs", nil, &jobs)
	return jobs
}

func (r remoteFake) Runners() (runners []fakegh.Runner) {
	r.call("GET", "/_fake/runners", nil, &runners)
	return runners
}

func (r remoteFake) ScaleSets() (sets []fakegh.ScaleSet) {
	r.call("GET", "/_fake/scale-sets", nil, &sets)
	return sets
}

func (r remoteFake) call(method, path string, in, out any) {
	r.t.Helper()
	status, err := r.api.do(method, path, in, out)
	if err != nil || status/100 != 2 {
		r.t.Fatalf("fake GitHub %s %s: status %d, %v", method, path, status, err)
	}
}
