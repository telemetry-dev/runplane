//go:build e2e && linux

package e2e

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/github/fakegh"
)

// TestContainerFlow runs the job flow on this Linux host, where install.sh
// installed runplane in container mode with Docker Engine and Sysbox. It
// also checks what container mode adds: sysbox-runc, the loop scratch disk,
// Docker in the sandbox, the network rules, and the Actions cache. It ends
// with a job that runs while the host reboots, and leaves the state that
// TestContainerAfterReboot checks after the boot. e2e/linux/run.sh
// prepares the host and runs both.
func TestContainerFlow(t *testing.T) {
	e := newInstalledEnv(t)
	f := &flow{env: e}
	f.connect()
	f.makePool()
	first := f.runJob()
	f.rejectPublic()
	f.scaleOut()
	f.restartDuringJob()
	f.debugRerun(first)
	f.mirror()
	c := &containerFlow{flow: f}
	c.sandboxRuntime()
	c.scratchDisk()
	c.dockerInSandbox()
	c.networkRules()
	c.actionsCache("http://" + gateway + ":8486/")
	c.defaultImage()
	f.dashboard()
	c.checkNoScratch()
	c.beforeReboot()
}

// TestContainerAfterReboot checks that runplane removes the sandboxes and
// runners that the reboot left, and operates again. Then it removes the
// pool and disconnects GitHub.
func TestContainerAfterReboot(t *testing.T) {
	e := newInstalledEnv(t)
	st := readRebootState(t)
	f := &flow{env: e, poolID: st.PoolID, scaleSetID: st.ScaleSetID}
	c := &containerFlow{flow: f}
	c.afterReboot(st)
	f.teardown()
	c.checkNoScratch()
}

type containerFlow struct{ *flow }

// sandboxRuntime checks that the warm sandbox operates with sysbox-runc in
// runplane.slice, and measures its memory.
func (c *containerFlow) sandboxRuntime() {
	c.t.Log("sysbox-runc and the idle sandbox")
	warm := c.waitIdle(nil)
	out, err := docker("inspect", "-f", "{{.HostConfig.Runtime}} {{.HostConfig.CgroupParent}} {{.Id}}", "runplane-"+warm.ID)
	fields := strings.Fields(out)
	if err != nil || len(fields) != 3 {
		c.t.Fatalf("docker inspect runplane-%s: %q, %v", warm.ID, out, err)
	}
	if fields[0] != "sysbox-runc" || fields[1] != "runplane.slice" {
		c.t.Errorf("sandbox runtime %s in %s, want sysbox-runc in runplane.slice", fields[0], fields[1])
	}
	if warm.IP == "" || !strings.HasPrefix(warm.IP, "10.213.") {
		c.t.Errorf("sandbox IP %q, want an address of the runplane network 10.213.0.0/16", warm.IP)
	}

	// The runner and dockerd settle after the runner starts to listen.
	time.Sleep(10 * time.Second)
	cg := "/sys/fs/cgroup/runplane.slice/docker-" + fields[2] + ".scope"
	current, err := readUint(filepath.Join(cg, "memory.current"))
	if err != nil {
		c.t.Fatalf("idle sandbox memory: %v", err)
	}
	stat, err := os.ReadFile(filepath.Join(cg, "memory.stat"))
	if err != nil {
		c.t.Fatal(err)
	}
	c.measured("idle sandbox memory", fmt.Sprintf("%d MiB (anon %d MiB, file %d MiB)",
		current>>20, memStat(stat, "anon")>>20, memStat(stat, "file")>>20))
}

// scratchDisk checks in a job that /mnt/scratch is the loop disk of the
// sandbox, that /var/lib/docker and /tmp are on it, and that dockerd in the
// sandbox writes its data there.
func (c *containerFlow) scratchDisk() {
	c.t.Log("the scratch disk")
	out := c.script("acme/api", "scratch disk", `
fail=0
df -h / /mnt/scratch /var/lib/docker /tmp
dev() { stat -c %d "$1"; }
for p in /var/lib/docker /tmp; do
  [ "$(dev $p)" = "$(dev /mnt/scratch)" ] || { echo "FAIL $p is not on the scratch disk"; fail=1; }
done
[ "$(dev /)" != "$(dev /mnt/scratch)" ] || { echo "FAIL the root file system is the scratch disk"; fail=1; }
src=$(findmnt -n -o SOURCE --target /mnt/scratch)
echo "scratch disk: $src"
case $src in /dev/loop*) ;; *) echo "FAIL the scratch disk is $src, want a loop device"; fail=1 ;; esac
size=$(df -B1G --output=size /mnt/scratch | tail -n 1 | tr -d ' ')
{ [ "$size" -ge 7 ] && [ "$size" -le 8 ]; } || { echo "FAIL the scratch disk has ${size}G, want 8G"; fail=1; }
for i in $(seq 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
echo "dockerd: $(docker info -f '{{.DockerRootDir}} {{.Driver}}')"
before=$(df -B1 --output=used /mnt/scratch | tail -n 1)
docker pull -q alpine:3.20
after=$(df -B1 --output=used /mnt/scratch | tail -n 1)
echo "scratch used: $before -> $after bytes"
[ "$after" -gt "$before" ] || { echo "FAIL the image pull did not write to the scratch disk"; fail=1; }
exit $fail
`)
	c.t.Logf("scratch disk job:\n%s", out)
}

// dockerInSandbox builds and runs an image in a job, and runs a service
// container with a published port, as "services:" does.
func (c *containerFlow) dockerInSandbox() {
	c.t.Log("docker build, docker run, and a service container")
	out := c.script("acme/api", "docker in the sandbox", `
set -eu
for i in $(seq 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
mkdir -p /tmp/app && cd /tmp/app
printf 'FROM alpine:3.20\nRUN echo built-in-the-sandbox > /msg\nCMD ["cat", "/msg"]\n' > Dockerfile
docker build -q -t e2e-app .
msg=$(docker run --rm e2e-app)
echo "docker run: $msg"
[ "$msg" = built-in-the-sandbox ]
docker network create e2e
docker run -d --name svc --network e2e -p 16379:6379 redis:7-alpine
redis_ping() { exec 3<>/dev/tcp/127.0.0.1/16379 && printf 'PING\r\n' >&3 && read -r -t 5 reply <&3 && exec 3<&- && echo "$reply"; }
for i in $(seq 30); do reply=$(redis_ping 2>/dev/null | tr -d '\r') && [ "$reply" = +PONG ] && break; sleep 1; done
echo "service on localhost:16379: $reply"
[ "$reply" = +PONG ]
by_name=$(docker run --rm --network e2e redis:7-alpine redis-cli -h svc ping)
echo "service by name: $by_name"
[ "$by_name" = PONG ]
`)
	c.t.Logf("docker job:\n%s", out)
}

// Addresses of the test network that e2e/linux/run.sh makes: a public
// address for the fake GitHub, and a web server on blocked addresses.
const (
	publicAddr  = "198.51.100.2"
	hostTestNet = "198.51.100.1" // the host side of the test network
	gateway     = "10.213.0.1"
)

var blockedTargets = []struct{ addr, what string }{
	{"169.254.169.254:80", "cloud metadata"},
	{"168.63.129.16:80", "Azure host endpoint"},
	{"10.99.0.2:80", "10.0.0.0/8"},
	{"172.16.99.2:80", "172.16.0.0/12"},
	{"192.168.99.2:80", "192.168.0.0/16"},
	{"100.64.99.2:80", "100.64.0.0/10"},
}

// networkRules checks from a sandbox what it can reach: the internet, a
// public address, and the service port, but no metadata service, private
// ranges, other sandbox, or other host port. Each blocked target answers
// from the host first, so a pass means a rule, not a missing server.
func (c *containerFlow) networkRules() {
	c.t.Log("network rules")
	// The other sandbox serves port 8000 during its job.
	req := c.fake.Queue(fakegh.JobSpec{
		Repo: "acme/api", Name: "listener", Labels: []string{label},
		Steps: []fakegh.Step{{Name: "Serve", Run: "timeout 300 python3 -m http.server 8000 --bind 0.0.0.0 >/dev/null 2>&1; true"}},
	})
	listener := c.waitJob("listener", 1, "running", 2*time.Minute)
	other, err := docker("inspect", "-f", "{{.NetworkSettings.Networks.runplane.IPAddress}}", "runplane-"+listener.SandboxID)
	if err != nil || other == "" {
		c.t.Fatalf("IP of sandbox %s: %q, %v", listener.SandboxID, other, err)
	}
	lan := hostLANAddr(c.t)
	blocked := []struct{ addr, what string }{
		{net.JoinHostPort(other, "8000"), "the other sandbox"},
		{gateway + ":8484", "the dashboard on the gateway"},
		{gateway + ":8485", "the agent listener on the gateway"},
		{gateway + ":2222", "the SSH gateway on the gateway"},
		{gateway + ":22", "sshd on the gateway"},
		{hostTestNet + ":8484", "the dashboard on a public host address"},
		{net.JoinHostPort(lan, "8484"), "the dashboard on the host LAN address"},
	}
	blocked = append(blocked, blockedTargets...)
	for _, b := range blocked {
		c.waitFor(b.addr+" answers the host", 30*time.Second, func() (bool, error) {
			err := dial(b.addr)
			return err == nil, err
		})
	}

	var checks strings.Builder
	for _, b := range blocked {
		fmt.Fprintf(&checks, "no %s %q\n", b.addr, b.what)
	}
	out := c.script("acme/api", "network rules", `
fail=0
reach() { timeout 5 bash -c "exec 3<>/dev/tcp/${1%:*}/${1##*:}" 2>/dev/null; }
ok() { if reach "$1"; then echo "PASS reached $1 ($2)"; else echo "FAIL cannot reach $1 ($2)"; fail=1; fi; }
no() { if reach "$1"; then echo "FAIL reached $1 ($2)"; fail=1; else echo "PASS blocked $1 ($2)"; fi; }
ok `+gateway+`:8486 "the service port"
ok `+publicAddr+`:80 "a public address"
`+checks.String()+`
if curl -fsS -m 20 -o /dev/null https://api.github.com/zen; then echo "PASS internet: https://api.github.com"; else echo "FAIL internet"; fail=1; fi
exit $fail
`)
	c.t.Logf("network job:\n%s", out)
	c.fake.(remoteFake).call("POST", fmt.Sprintf("/_fake/jobs/%d/cancel", req), nil, nil)
	listener = c.waitJob("listener", 1, "completed", 2*time.Minute)
	if listener.result() != "cancelled" {
		c.t.Errorf("the canceled listener job: %s, want cancelled", listener.result())
	}
}

// actionsCache saves a cache with the real @actions/cache client in one
// sandbox and restores it in another. The client sends no repository: the
// cache server finds the sandbox, and so the repository, by the source
// address, so the same key misses in a different repository. resultsURL is
// the cache URL as a shell word.
func (c *containerFlow) actionsCache(resultsURL string) {
	c.t.Log("Actions cache")
	// The fake runner gives the steps no job environment, so they set what
	// the patched runner gives a push job on main: the cache URL from the
	// runner .env, a temp folder, and a runtime token. The cache server
	// ignores the "ac" claim of the token: the ref refs/heads/main comes
	// from the run that the fake GitHub reports.
	client := `
set -eu
b64() { printf '%s' "$1" | base64 -w0 | tr '+/' '-_' | tr -d '='; }
export ACTIONS_RESULTS_URL=` + resultsURL + ` ACTIONS_CACHE_SERVICE_V2=true RUNNER_TEMP=/tmp/runner-temp
export ACTIONS_RUNTIME_TOKEN="$(b64 '{"alg":"none"}').$(b64 '{"ac":"[{\"Scope\":\"refs/heads/main\",\"Permission\":3}]"}').e2e"
mkdir -p /tmp/cache "$RUNNER_TEMP" && cd /tmp/cache
npm init -y >/dev/null
npm install --no-audit --no-fund --loglevel=error @actions/cache@4 >/dev/null
`
	out := c.script("acme/api", "cache save", client+`
seq 1 1000000 > blob.txt
node -e 'require("@actions/cache").saveCache(["blob.txt"], "e2e-key").then(id => console.log("saved cache", id))'
`)
	c.t.Logf("cache save job:\n%s", out)
	var entries []struct {
		Key       string `json:"key"`
		Ref       string `json:"ref"`
		SizeBytes int64  `json:"size_bytes"`
		Finalized bool   `json:"finalized"`
	}
	c.must("GET", "/api/caches/entries?repository=acme/api", nil, &entries)
	if len(entries) != 1 || entries[0].Key != "e2e-key" || !entries[0].Finalized || entries[0].SizeBytes == 0 || entries[0].Ref != "refs/heads/main" {
		c.t.Errorf("cache entries of acme/api = %+v, want e2e-key on refs/heads/main", entries)
	}

	out = c.script("acme/api", "cache restore", client+`
node -e 'require("@actions/cache").restoreCache(["blob.txt"], "e2e-key").then(k => require("fs").writeFileSync("key", k || "miss"))'
key=$(cat key)
echo "restore: $key"
[ "$key" = e2e-key ]
seq 1 1000000 | cmp - blob.txt
echo "the restored file is the same"
`)
	c.t.Logf("cache restore job:\n%s", out)
	out = c.script("acme/web", "cache other repository", client+`
node -e 'require("@actions/cache").restoreCache(["blob.txt"], "e2e-key").then(k => require("fs").writeFileSync("key", k || "miss"))'
key=$(cat key)
echo "restore in acme/web: $key"
[ "$key" = miss ]
`)
	c.t.Logf("cache job of another repository:\n%s", out)
}

// defaultImage checks that the default image, which install.sh did not
// bring, becomes ready on this server, and that the doctor passes with
// the sandbox internet check.
func (c *containerFlow) defaultImage() {
	c.t.Log("the default image and the doctor")
	c.waitFor("the default image is ready", 45*time.Minute, func() (bool, error) {
		var list []struct {
			IsDefault bool `json:"is_default"`
			Builds    []struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			} `json:"builds"`
		}
		_, err := c.api.do("GET", "/api/images", nil, &list)
		for _, i := range list {
			if i.IsDefault && len(i.Builds) == 1 {
				if i.Builds[0].Status == "failed" {
					c.t.Fatalf("default image: %s", i.Builds[0].Error)
				}
				return i.Builds[0].Status == "ready", err
			}
		}
		return false, err
	})
	internet := false
	for _, ch := range c.doctor() {
		if ch.Status == "fail" {
			c.t.Errorf("doctor check %s failed: %s", ch.ID, ch.Detail)
		}
		internet = internet || (ch.ID == "internet" && ch.Status == "ok")
	}
	if !internet {
		c.t.Error("the doctor has no passed sandbox internet check")
	}
}

type doctorCheck struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// doctor runs the doctor of the service on this server.
func (c *containerFlow) doctor() []doctorCheck {
	var servers []struct {
		ID string `json:"id"`
	}
	c.must("GET", "/api/servers", nil, &servers)
	var out struct {
		Checks []doctorCheck `json:"checks"`
	}
	c.must("POST", "/api/servers/"+servers[0].ID+"/doctor", nil, &out)
	for _, ch := range out.Checks {
		c.t.Logf("doctor: %-5s %-12s %s", ch.Status, ch.ID, ch.Detail)
	}
	return out.Checks
}

// forwarding checks that the host forwards the sandbox traffic, and that
// the FORWARD policy is DROP after a boot: runplane turns on forwarding
// after Docker starts, so Docker turns it on and sets DROP. The doctor must
// find the same.
func (c *containerFlow) forwarding() {
	c.t.Log("IP forwarding and the FORWARD policy")
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err != nil || strings.TrimSpace(string(b)) != "1" {
		c.t.Errorf("net.ipv4.ip_forward = %q (%v), want 1", b, err)
	}
	out, err := exec.Command("iptables", "-w", "5", "-S", "FORWARD").Output()
	if err != nil || !strings.HasPrefix(string(out), "-P FORWARD DROP\n") {
		c.t.Errorf("iptables -S FORWARD (%v):\n%s\nwant the policy DROP", err, out)
	}
	for _, ch := range c.doctor() {
		if (ch.ID == "ip_forward" || ch.ID == "firewall") && ch.Status != "ok" {
			c.t.Errorf("doctor check %s is %s: %s", ch.ID, ch.Status, ch.Detail)
		}
	}
}

// rebootState is what TestContainerFlow leaves for TestContainerAfterReboot.
type rebootState struct {
	PoolID     string   `json:"pool_id"`
	ScaleSetID int      `json:"scale_set_id"`
	Sandboxes  []string `json:"sandboxes"`
	Runners    []string `json:"runners"`
	JobID      string   `json:"job_id"` // runs during the reboot
}

func stateFile() string {
	if p := os.Getenv("E2E_STATE"); p != "" {
		return p
	}
	return "/var/lib/rp-e2e/state.json"
}

// beforeReboot starts a long job next to the warm sandbox and saves the
// state for the check after the reboot.
func (c *containerFlow) beforeReboot() {
	c.t.Log("a job that runs during the reboot")
	c.fake.Queue(fakegh.JobSpec{
		Repo: "acme/api", Name: "reboot", Labels: []string{label},
		Steps: []fakegh.Step{{Name: "Run long", Lines: []string{"before the reboot"}, Sleep: 3600}},
	})
	j := c.waitJob("reboot", 1, "running", 2*time.Minute)
	c.waitIdle([]string{j.SandboxID})
	st := rebootState{PoolID: c.poolID, ScaleSetID: c.scaleSetID, JobID: j.ID}
	for _, b := range c.live() {
		st.Sandboxes = append(st.Sandboxes, b.ID)
		st.Runners = append(st.Runners, b.RunnerName)
	}
	b, _ := json.Marshal(st)
	if err := writeSynced(stateFile(), b); err != nil {
		c.t.Fatal(err)
	}
	c.t.Logf("reboot the host now; sandboxes %v, runners %v", st.Sandboxes, st.Runners)
}

// writeSynced writes a file that a power loss right after does not lose.
func writeSynced(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func readRebootState(t *testing.T) rebootState {
	b, err := os.ReadFile(stateFile())
	if err != nil {
		t.Fatalf("no state of TestContainerFlow: %v", err)
	}
	var st rebootState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// afterReboot checks the host after the boot: IP forwarding with the
// FORWARD policy DROP, and the cleanup: the sandboxes are gone with their
// containers, folders, and scratch disks, GitHub has none of their runners,
// the job of the reboot ends, and a new job runs.
func (c *containerFlow) afterReboot(st rebootState) {
	c.t.Log("after the reboot")
	c.waitFor("the API answers after the boot", 3*time.Minute, func() (bool, error) {
		status, err := c.api.do("GET", "/api/status", nil, nil)
		return status == 200, err
	})
	c.must("POST", "/api/session", map[string]string{"email": adminEmail, "password": adminPassword}, nil)
	c.forwarding()
	for _, id := range st.Sandboxes {
		c.waitFor("sandbox "+id+" ends", 3*time.Minute, func() (bool, error) {
			var b sandboxRow
			_, err := c.api.do("GET", "/api/sandboxes/"+id, nil, &b)
			return b.State == "gone" || b.State == "failed", err
		})
		c.checkRemoved(id)
	}
	c.checkNoScratch()

	var interrupted job
	c.waitFor("the job of the reboot ends", 5*time.Minute, func() (bool, error) {
		_, err := c.api.do("GET", "/api/jobs/"+st.JobID, nil, &interrupted)
		return interrupted.State == "completed", err
	})
	c.t.Logf("the job of the reboot ended: %s", interrupted.result())
	// When systemd stops the host, the runner stops the job and reports it.
	// After a power loss, nothing can report it.
	if os.Getenv("E2E_REBOOT") != "crash" && interrupted.result() != "cancelled" {
		c.t.Errorf("the job of the reboot: %s, want cancelled", interrupted.result())
	}
	c.waitFor("GitHub has no runner of the old sandboxes", 5*time.Minute, func() (bool, error) {
		var left []string
		for _, r := range c.fake.Runners() {
			if slices.Contains(st.Runners, r.Name) {
				left = append(left, r.Name)
			}
		}
		return len(left) == 0, fmt.Errorf("runners %v", left)
	})

	c.waitIdle(st.Sandboxes)
	c.fake.Queue(fakegh.JobSpec{
		Repo: "acme/api", Name: "after reboot", Labels: []string{label},
		Steps: []fakegh.Step{{Name: "Run tests", Lines: []string{"after the reboot"}}},
	})
	if j := c.waitJob("after reboot", 1, "completed", 3*time.Minute); j.result() != "succeeded" {
		c.t.Errorf("the job after the reboot: %s, want succeeded", j.result())
	}
}

// checkNoScratch fails the test if a loop device or a mount of a sandbox
// scratch disk is left.
func (c *containerFlow) checkNoScratch() {
	c.t.Helper()
	dir := filepath.Join(c.dataDir, "sandboxes") + "/"
	live := map[string]bool{}
	for _, b := range c.live() {
		live[b.ID] = true
	}
	owner := func(path string) string {
		id, _, _ := strings.Cut(strings.TrimPrefix(path, dir), "/")
		return id
	}
	loops, _ := exec.Command("losetup", "-l", "-n", "-O", "BACK-FILE").Output()
	mounts, _ := exec.Command("findmnt", "-r", "-n", "-o", "TARGET").Output()
	for _, path := range strings.Fields(string(loops) + " " + string(mounts)) {
		if strings.HasPrefix(path, dir) && !live[owner(path)] {
			c.t.Errorf("a scratch disk of a removed sandbox is left: %s", path)
		}
	}
}

// script runs a bash script as a job step in a sandbox and returns its
// output. The output comes from the live job log, so a second step
// keeps the job open until the test has read it. A script that exits with
// an error fails the test.
func (c *containerFlow) script(repo, name, script string) string {
	c.t.Helper()
	c.fake.Queue(fakegh.JobSpec{
		Repo: repo, Name: name, Labels: []string{label},
		Steps: []fakegh.Step{
			{Name: "Run script", Run: "(" + script + ") 2>&1; echo \"E2E-EXIT $?\""},
			{Name: "Hold the log", Sleep: 5},
		},
	})
	j := c.waitJob(name, 1, "running", 3*time.Minute)
	var text string
	c.waitFor("the output of "+name, 15*time.Minute, func() (bool, error) {
		_, err := c.api.do("GET", "/api/jobs/"+j.ID+"/log", nil, &text)
		if strings.Contains(text, "E2E-EXIT") {
			return true, nil
		}
		c.must("GET", "/api/jobs/"+j.ID, nil, &j)
		if j.State == "completed" {
			c.t.Fatalf("job %q completed before the test read its output", name)
		}
		return false, err
	})
	out, code := scriptOutput(text)
	if code != 0 {
		c.t.Errorf("job %q: the script exited with %d:\n%s", name, code, out)
	}
	j = c.waitJob(name, 1, "completed", 2*time.Minute)
	if j.result() != "succeeded" {
		c.t.Errorf("job %q: %s, want succeeded", name, j.result())
	}
	return out
}

var (
	logTime  = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\S+ `)
	exitLine = regexp.MustCompile(`E2E-EXIT (\d+)`)
)

// scriptOutput takes the lines of the script step from a job log, without
// the times, and the exit code of the script.
func scriptOutput(log string) (string, int) {
	_, rest, _ := strings.Cut(log, "##[group]Run script")
	rest, _, _ = strings.Cut(rest, "##[group]Hold the log")
	var lines []string
	code := -1
	for _, line := range strings.Split(rest, "\n") {
		line = logTime.ReplaceAllString(line, "")
		if m := exitLine.FindStringSubmatch(line); m != nil {
			code, _ = strconv.Atoi(m[1])
			break
		}
		if line != "" && !strings.HasPrefix(line, "##[") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n"), code
}

func (c *containerFlow) measured(what, value string) {
	c.timings = append(c.timings, fmt.Sprintf("%-45s %s", what, value))
}

func dial(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err == nil {
		conn.Close()
	}
	return err
}

// hostLANAddr is the address of the interface with the default route.
func hostLANAddr(t *testing.T) string {
	conn, err := net.Dial("udp", "1.1.1.1:53")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// memStat reads one value of a cgroup memory.stat file.
func memStat(stat []byte, key string) uint64 {
	for _, line := range strings.Split(string(stat), "\n") {
		if k, v, ok := strings.Cut(line, " "); ok && k == key {
			n, _ := strconv.ParseUint(v, 10, 64)
			return n
		}
	}
	return 0
}
