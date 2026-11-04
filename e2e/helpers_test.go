//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/github/fakegh"
)

// baseImage is the sandbox image of the test. Build it with
// docker build --provenance=false -t runplane/runner-ubuntu-24.04:local images/ubuntu-24.04
// runplane builds the builtin image with this tag too. Without provenance,
// both builds give the same image ID. Otherwise the tag moves, and Docker
// with the containerd image store deletes the image of the test pool.
const baseImage = "runplane/runner-ubuntu-24.04:local"

// env is one runplane with a fake GitHub, and the folders, ports, and
// clients of the test. The test starts runplane as a child process, or it
// uses the systemd unit that install.sh made.
type env struct {
	t           *testing.T
	root        string // repository root
	dir         string // binaries, data, config, and logs of this run
	mode        string // sandbox mode of runplane
	unit        string // systemd unit of an installed runplane; empty: a child process
	since       time.Time
	dataDir     string
	configDir   string
	bin         string
	fake        fakeGitHub
	fakeURL     string
	addr        string // dashboard and API
	sshAddr     string
	agentAddr   string
	servicePort int // and the dev control channel on servicePort+1
	api         *client
	events      *eventLog

	cmd    *exec.Cmd
	exited chan struct{}

	timings []string
}

// fakeGitHub is the fake GitHub of the test: a fakegh server in the test
// process, or one that runs elsewhere (remoteFake).
type fakeGitHub interface {
	Queue(fakegh.JobSpec) int64
	Jobs() []fakegh.Job
	Runners() []fakegh.Runner
	ScaleSets() []fakegh.ScaleSet
}

// newEnv checks Docker and the base image, builds runplane and
// runplane-init, and starts the fake GitHub. The test skips without Docker
// or the base image.
func newEnv(t *testing.T) *env {
	arch, err := dockerArch()
	if err != nil {
		t.Skipf("Docker does not answer, so the dev flow cannot run: %v", err)
	}
	if _, err := docker("image", "inspect", baseImage); err != nil {
		t.Skipf("the base image %s is missing; build it with: docker build --provenance=false -t %[1]s images/ubuntu-24.04", baseImage)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// Docker must be able to bind-mount the data folder, and on macOS it
	// shares the temp folder.
	dir, err := os.MkdirTemp("", "runplane-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		t: t, root: root, dir: dir, mode: "dev", since: time.Now(),
		dataDir: filepath.Join(dir, "data"), configDir: filepath.Join(dir, "etc"),
		// The name keeps "pkill -f 'runplane serve'" of other work on this
		// machine away from the test.
		bin: filepath.Join(dir, "bin", "runplane-e2e"),
	}
	t.Cleanup(e.cleanup)

	e.build(arch)
	e.startFake()
	ports := freePorts(t, 3)
	e.addr = "127.0.0.1:" + strconv.Itoa(ports[0])
	e.sshAddr = "127.0.0.1:" + strconv.Itoa(ports[1])
	e.agentAddr = "127.0.0.1:" + strconv.Itoa(ports[2])
	e.servicePort = ports[3]
	e.api = newClient("http://" + e.addr)
	e.events = &eventLog{}
	return e
}

// build makes runplane-init for the Linux of Docker and gzips it into
// internal/initbin/bin, as "make init" does, and then runplane for this OS,
// which embeds it. An older embedded runplane-init would otherwise win over
// a fresh one.
func (e *env) build(arch string) {
	e.t.Helper()
	start := time.Now()
	run := func(env []string, args ...string) {
		cmd := exec.Command("go", args...)
		cmd.Dir = e.root
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			e.t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	plain := e.bin + "-init"
	run([]string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + arch},
		"build", "-trimpath", "-ldflags", "-s -w", "-o", plain, "./init")
	if err := gzipFile(plain, filepath.Join(e.root, "internal", "initbin", "bin", "runplane-init")); err != nil {
		e.t.Fatal(err)
	}
	run(nil, "build", "-o", e.bin, "./cmd/runplane")
	e.timing("build runplane-init and runplane", time.Since(start))
}

func gzipFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.Write(in)
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(dst, buf.Bytes(), 0o755)
}

// startFake serves the fake GitHub. Sandboxes reach it through
// host.docker.internal, which Docker on macOS sends to the loopback
// address of the host, and Docker on Linux to the bridge gateway.
func (e *env) startFake() {
	host := "127.0.0.1"
	if runtime.GOOS == "linux" {
		host = "0.0.0.0"
	}
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		e.t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fake := fakegh.New(fakegh.Config{
		RunnerDir: filepath.Join(e.root, "testdata", "fakerunner"),
		RunnerURL: fmt.Sprintf("http://host.docker.internal:%d", port),
		// Short polls let a stopped listener let go of its session soon.
		PollTimeout: 5 * time.Second,
	})
	e.fake = fake
	srv := &http.Server{Handler: fake, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	e.t.Cleanup(func() {
		fake.Close()
		srv.Close()
	})
	e.fakeURL = fmt.Sprintf("http://127.0.0.1:%d", port)
}

// start starts runplane and waits until the API answers.
func (e *env) start() {
	e.t.Helper()
	if e.unit != "" {
		e.systemctl("start")
	} else {
		e.startProcess()
	}
	e.waitFor("the API answers", 30*time.Second, func() (bool, error) {
		select {
		case <-e.exited:
			e.t.Fatalf("runplane exited: %v\n%s", e.cmd.ProcessState, e.log())
		default:
		}
		status, err := e.api.do("GET", "/api/status", nil, nil)
		return status == http.StatusOK, err
	})
}

// startProcess starts runplane serve as a child process.
func (e *env) startProcess() {
	e.t.Helper()
	logFile, err := os.OpenFile(filepath.Join(e.dir, "runplane.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		e.t.Fatal(err)
	}
	cmd := exec.Command(e.bin, "serve")
	cmd.Dir = e.dir
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = append(cleanEnv(),
		"RUNPLANE_MODE=dev",
		"RUNPLANE_DEV_INSECURE=1",
		"RUNPLANE_DATA_DIR="+e.dataDir,
		"RUNPLANE_CONFIG_DIR="+e.configDir,
		"RUNPLANE_LISTEN="+e.addr,
		"RUNPLANE_AGENT_LISTEN="+e.agentAddr,
		"RUNPLANE_SERVICE_PORT="+strconv.Itoa(e.servicePort),
		"RUNPLANE_SSH_LISTEN="+e.sshAddr,
		"RUNPLANE_GITHUB_API_URL="+e.fakeURL+"/api/v3",
		"RUNPLANE_GITHUB_URL="+e.fakeURL,
		"RUNPLANE_DEV_RUNNER_DIR="+filepath.Join(e.root, "testdata", "fakerunner"),
	)
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	logFile.Close()
	e.cmd, e.exited = cmd, make(chan struct{})
	go func() {
		cmd.Wait()
		close(e.exited)
	}()
}

// stop ends runplane with SIGTERM, as systemd does, and waits for it.
func (e *env) stop() {
	e.t.Helper()
	if e.unit != "" {
		e.systemctl("stop")
		return
	}
	if e.cmd == nil {
		return
	}
	e.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-e.exited:
	case <-time.After(45 * time.Second):
		e.cmd.Process.Kill()
		<-e.exited
		e.t.Errorf("runplane did not stop within 45 s of SIGTERM")
	}
	e.cmd = nil
}

// cleanup stops runplane and removes the sandboxes that it left. It keeps
// the folder of a failed run, or of each run with E2E_KEEP=1, for the logs.
func (e *env) cleanup() {
	e.stop()
	owned := "label=dev.runplane.data=" + e.dataDir
	if ids, _ := docker("ps", "-aq", "--filter", owned); ids != "" {
		e.t.Errorf("containers of this runplane are left: %s", ids)
		for _, id := range strings.Fields(ids) {
			// The output of runplane-init before it reached runplane.
			out, _ := exec.Command("docker", "logs", id).CombinedOutput()
			os.WriteFile(filepath.Join(e.dir, "container-"+id+".log"), out, 0o600)
		}
		docker(append([]string{"rm", "-f"}, strings.Fields(ids)...)...)
	}
	if names, _ := docker("volume", "ls", "-q", "--filter", owned); names != "" {
		e.t.Errorf("volumes of this runplane are left: %s", names)
		docker(append([]string{"volume", "rm", "-f"}, strings.Fields(names)...)...)
	}
	e.logTimings()
	if e.t.Failed() || os.Getenv("E2E_KEEP") == "1" {
		e.t.Logf("the runplane log and data are in %s", e.dir)
		return
	}
	os.RemoveAll(e.dir)
}

func (e *env) logTimings() {
	for _, line := range e.timings {
		e.t.Log("timing: " + line)
	}
}

// log returns the runplane log since the start of the test.
func (e *env) log() string {
	if e.unit != "" {
		out, _ := exec.Command("journalctl", "-u", e.unit, "-o", "cat", "--no-pager",
			"--since", fmt.Sprintf("@%d", e.since.Unix())).Output()
		return string(out)
	}
	b, _ := os.ReadFile(filepath.Join(e.dir, "runplane.log"))
	return string(b)
}

var setupCodeRe = regexp.MustCompile(`code=([0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4})`)

// setupCode reads the setup code from the log, as an admin would. For an
// installed runplane, it asks for a new one, as the installer does.
func (e *env) setupCode() string {
	e.t.Helper()
	if e.unit != "" {
		out, err := exec.Command("runplane", "setup-code").Output()
		if err != nil {
			e.t.Fatalf("runplane setup-code: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	m := setupCodeRe.FindStringSubmatch(e.log())
	if m == nil {
		e.t.Fatalf("no setup code in the log:\n%s", e.log())
	}
	return m[1]
}

func (e *env) timing(what string, d time.Duration) {
	e.timings = append(e.timings, fmt.Sprintf("%-45s %s", what, d.Round(time.Millisecond)))
}

// waitFor calls fn until it reports true. It fails the test after timeout
// with the last error of fn.
func (e *env) waitFor(what string, timeout time.Duration, fn func() (bool, error)) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, err := fn()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("waited %s for %s; last error: %v", timeout, what, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// must calls the API and fails the test unless the status is 2xx.
func (e *env) must(method, path string, in, out any) {
	e.t.Helper()
	status, err := e.api.do(method, path, in, out)
	if err != nil || status/100 != 2 {
		e.t.Fatalf("%s %s: status %d, %v", method, path, status, err)
	}
}

// checkRemoved fails the test if a sandbox left a container, a scratch
// volume, a folder, or a VM chroot or scope.
func (e *env) checkRemoved(id string) {
	e.t.Helper()
	if _, err := docker("container", "inspect", "runplane-"+id); err == nil {
		e.t.Errorf("sandbox %s: the container is still there", id)
	}
	if _, err := docker("volume", "inspect", "runplane-scratch-"+id); err == nil {
		e.t.Errorf("sandbox %s: the scratch volume is still there", id)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "sandboxes", id)); !errors.Is(err, os.ErrNotExist) {
		e.t.Errorf("sandbox %s: the folder is still there (%v)", id, err)
	}
	if jails, _ := filepath.Glob(filepath.Join(e.dataDir, "jail", "*", id)); len(jails) > 0 {
		e.t.Errorf("sandbox %s: the VM chroot is still there: %v", id, jails)
	}
	if _, err := os.Stat("/sys/fs/cgroup/runplane.slice/runplane-vm-" + id + ".scope"); err == nil {
		e.t.Errorf("sandbox %s: the VM scope is still there", id)
	}
}

// client calls the runplane API with a session cookie and the CSRF header.
type client struct {
	base string
	http *http.Client
}

func newClient(base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{base: base, http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

// do sends in as JSON and decodes a JSON answer into out. A text answer
// goes into out if it is a *string.
func (c *client) do(method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Runplane-CSRF", "1")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, err
	}
	if res.StatusCode/100 != 2 {
		return res.StatusCode, fmt.Errorf("%s", bytes.TrimSpace(b))
	}
	switch out := out.(type) {
	case nil:
	case *string:
		*out = string(b)
	default:
		if err := json.Unmarshal(b, out); err != nil {
			return res.StatusCode, fmt.Errorf("decode %s: %w", b, err)
		}
	}
	return res.StatusCode, nil
}

// eventLog keeps the events of GET /api/events. It connects again when
// the stream ends, for example while runplane restarts.
type eventLog struct {
	mu   sync.Mutex
	list []event
}

type event struct {
	Type string
	Data json.RawMessage
}

func (l *eventLog) follow(ctx context.Context, c *client) {
	for ctx.Err() == nil {
		l.read(ctx, c)
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (l *eventLog) read(ctx context.Context, c *client) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/api/events", nil)
	if err != nil {
		return
	}
	// The stream has no end, so it cannot use the client timeout.
	stream := &http.Client{Jar: c.http.Jar}
	res, err := stream.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var typ string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			typ = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			l.mu.Lock()
			l.list = append(l.list, event{typ, json.RawMessage(strings.TrimPrefix(line, "data: "))})
			l.mu.Unlock()
		}
	}
}

// find reports if an event of the type has data that match.
func (l *eventLog) find(typ string, match func(data map[string]any) bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ev := range l.list {
		var data map[string]any
		if ev.Type == typ && json.Unmarshal(ev.Data, &data) == nil && match(data) {
			return true
		}
	}
	return false
}

func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).Output()
	return strings.TrimSpace(string(out)), err
}

// dockerArch returns the GOARCH of the Docker server.
func dockerArch() (string, error) {
	out, err := docker("info", "--format", "{{.Architecture}}")
	if err != nil {
		return "", err
	}
	switch out {
	case "aarch64", "arm64":
		return "arm64", nil
	case "x86_64", "amd64":
		return "amd64", nil
	}
	return "", fmt.Errorf("unknown Docker arch %q", out)
}

// cleanEnv is the environment without RUNPLANE_ variables of the shell.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "RUNPLANE_") {
			out = append(out, kv)
		}
	}
	return out
}

// freePorts returns n free ports and then a port p where p and p+1 are
// free, all different: runplane serves the service port on p and the dev
// control channel on p+1. It holds the ports until it has all, so the
// kernel cannot give one twice.
func freePorts(t *testing.T, n int) []int {
	var held []net.Listener
	defer func() {
		for _, ln := range held {
			ln.Close()
		}
	}()
	listen := func(addr string) (int, bool) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return 0, false
		}
		held = append(held, ln)
		return ln.Addr().(*net.TCPAddr).Port, true
	}
	var ports []int
	for range n {
		p, ok := listen("127.0.0.1:0")
		if !ok {
			t.Fatal("no free port")
		}
		ports = append(ports, p)
	}
	for range 50 {
		p, _ := listen(":0")
		if _, ok := listen(":" + strconv.Itoa(p+1)); ok {
			return append(ports, p)
		}
	}
	t.Fatal("no two free ports in a row")
	return nil
}
