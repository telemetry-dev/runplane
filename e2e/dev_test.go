//go:build e2e

// Package e2e runs the job flow of runplane end to end in dev mode: the
// runplane binary as a process, sandboxes in Docker with the devdocker
// driver, and the fake GitHub of internal/github/fakegh with the fake runner
// of testdata/fakerunner. Run it with "make e2e".
package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/runplane/runplane/internal/github/fakegh"
)

// The pool of the test: one warm sandbox, at most three.
const (
	label      = "e2e"
	minIdle    = 1
	maxRunners = 3
)

// The first admin.
const (
	adminEmail    = "admin@example.com"
	adminPassword = "e2e-password-1234"
)

type job struct {
	ID            string     `json:"id"`
	JobName       string     `json:"job_name"`
	WorkflowName  string     `json:"workflow_name"`
	Repository    string     `json:"repository"`
	RunNumber     int64      `json:"run_number"`
	RunAttempt    int        `json:"run_attempt"`
	HeadBranch    string     `json:"head_branch"`
	HeadSHA       string     `json:"head_sha"`
	CommitMessage string     `json:"commit_message"`
	Event         string     `json:"event"`
	Actor         string     `json:"actor"`
	HTMLURL       string     `json:"html_url"`
	State         string     `json:"state"`
	RejectReason  string     `json:"reject_reason"`
	Result        *string    `json:"result"`
	Debug         bool       `json:"debug"`
	QueuedAt      time.Time  `json:"queued_at"`
	StartedAt     *time.Time `json:"started_at"`
	Steps         []struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
	} `json:"steps"`
	SandboxID    string     `json:"sandbox_id"`
	SandboxState string     `json:"sandbox_state"`
	HoldUntil    *time.Time `json:"hold_until"`
	SSHCommand   string     `json:"ssh_command"`
	LogState     string     `json:"log_state"`
	CPUPeak      *float64   `json:"cpu_peak"`
	MemPeakMB    *int       `json:"mem_peak_mb"`
}

func (j job) result() string {
	if j.Result == nil {
		return ""
	}
	return *j.Result
}

type sandboxRow struct {
	ID         string     `json:"id"`
	State      string     `json:"state"`
	IP         string     `json:"ip"`
	RunnerName string     `json:"runner_name"`
	JobID      string     `json:"job_id"`
	Debug      bool       `json:"debug"`
	HoldUntil  *time.Time `json:"hold_until"`
	CreatedAt  time.Time  `json:"created_at"`
	IdleAt     *time.Time `json:"idle_at"`
	Error      string     `json:"error"`
}

type mirrorStats struct {
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	MissBytes int64 `json:"miss_bytes"`
}

func TestDevFlow(t *testing.T) {
	e := newEnv(t)
	f := &flow{env: e}
	f.connect()
	f.makePool()
	first := f.runJob()
	f.rejectPublic()
	f.scaleOut()
	f.redeliver()
	f.restartDuringJob()
	f.debugRerun(first)
	f.mirror()
	f.runnerWithoutGitHub()
	f.dashboard()
	f.teardown()
}

// flow holds what the steps of the flow tests share.
type flow struct {
	*env
	poolID     string
	scaleSetID int
	// defaultImage makes the pool use the default image, which runplane
	// pulls or builds, not baseImage.
	defaultImage bool
}

// connect makes the first admin with the setup code and connects GitHub
// with the manifest flow, as a browser does.
func (f *flow) connect() {
	f.t.Log("setup and GitHub")
	f.start()
	var status struct {
		NeedsSetup bool   `json:"needs_setup"`
		Mode       string `json:"mode"`
	}
	f.must("GET", "/api/status", nil, &status)
	if !status.NeedsSetup || status.Mode != f.mode {
		f.t.Fatalf("status = %+v, want needs_setup in %s mode", status, f.mode)
	}
	f.must("POST", "/api/setup", map[string]string{
		"code": f.setupCode(), "name": "E2E Admin", "email": adminEmail, "password": adminPassword,
	}, nil)
	// The jobs of the test need little disk, and a debug hold of one
	// minute keeps the test short.
	f.must("PATCH", "/api/settings", map[string]any{"min_free_disk_percent": 1, "debug_hold_minutes": 1}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	f.t.Cleanup(cancel)
	go f.events.follow(ctx, f.api)

	var m struct {
		ActionURL string `json:"action_url"`
		Manifest  string `json:"manifest"`
	}
	f.must("POST", "/api/github/manifest", map[string]string{"account_type": "organization", "organization": "acme"}, &m)
	// The browser posts the manifest to GitHub. GitHub sends it to the
	// callback, the callback to the install page, and the install page to
	// the setup URL.
	res, err := f.api.http.PostForm(m.ActionURL, url.Values{"manifest": {m.Manifest}})
	if err != nil {
		f.t.Fatal(err)
	}
	res.Body.Close()
	if res.Request.URL.Path != "/setup/first-job" {
		f.t.Fatalf("the manifest flow ended at %s, want /setup/first-job", res.Request.URL)
	}
	var gh struct {
		Connected     bool `json:"connected"`
		Installations []struct {
			AccountLogin string `json:"account_login"`
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		} `json:"installations"`
	}
	f.must("GET", "/api/github", nil, &gh)
	if !gh.Connected || len(gh.Installations) != 1 || gh.Installations[0].AccountLogin != "acme" || len(gh.Installations[0].Repositories) != 3 {
		f.t.Fatalf("GitHub after the manifest flow = %+v, want the acme installation with 3 repositories", gh)
	}
}

// makePool makes a pool with the base image, or with the default image
// when defaultImage is set, and waits for its listener and its warm
// sandbox.
func (f *flow) makePool() {
	f.t.Log("pool, scale set, and warm sandbox")
	// A new install has the label "runplane". The test uses a smaller pool of
	// its own, so it removes the default one, as a user can.
	var pools []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	f.must("GET", "/api/pools", nil, &pools)
	if len(pools) != 1 || pools[0].Label != "runplane" {
		f.t.Fatalf("pools of a new install %+v, want the default label runplane", pools)
	}
	f.must("DELETE", "/api/pools/"+pools[0].ID, nil, nil)
	var img struct {
		ID string `json:"id"`
	}
	if !f.defaultImage {
		f.must("POST", "/api/images", map[string]string{"name": "e2e", "ref": baseImage}, &img)
	}
	start := time.Now()
	// A VM root disk of the default image can need a build.
	f.waitFor("the image is ready", 45*time.Minute, func() (bool, error) {
		var list []struct {
			ID        string `json:"id"`
			IsDefault bool   `json:"is_default"`
			Builds    []struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			} `json:"builds"`
		}
		_, err := f.api.do("GET", "/api/images", nil, &list)
		for _, i := range list {
			if (i.ID == img.ID || img.ID == "" && i.IsDefault) && len(i.Builds) == 1 {
				if i.Builds[0].Status == "failed" {
					f.t.Fatalf("image: %s", i.Builds[0].Error)
				}
				return i.Builds[0].Status == "ready", err
			}
		}
		return false, err
	})
	f.timing("image ready", time.Since(start))

	created := time.Now()
	var pool struct {
		ID string `json:"id"`
	}
	f.must("POST", "/api/pools", map[string]any{
		"label": label, "vcpu": 1, "memory_mb": 1024, "disk_gb": 8, "min_idle": minIdle, "max_runners": maxRunners, "image_id": img.ID,
	}, &pool)
	f.poolID = pool.ID

	f.waitFor("the listener polls", time.Minute, func() (bool, error) {
		var gh struct {
			ScaleSets []struct {
				PoolLabel  string     `json:"pool_label"`
				Status     string     `json:"status"`
				Error      string     `json:"error"`
				LastPollAt *time.Time `json:"last_poll_at"`
			} `json:"scale_sets"`
		}
		_, err := f.api.do("GET", "/api/github", nil, &gh)
		for _, s := range gh.ScaleSets {
			if s.PoolLabel == label {
				return s.Status == "listening" && s.LastPollAt != nil, fmt.Errorf("scale set %+v", s)
			}
		}
		return false, err
	})
	for _, s := range f.fake.ScaleSets() {
		if s.Name == label && s.Session {
			f.scaleSetID = s.ID
		}
	}
	if f.scaleSetID == 0 {
		f.t.Fatalf("GitHub has no scale set %q with a session: %+v", label, f.fake.ScaleSets())
	}

	warm := f.waitIdle(nil)
	f.timing("pool created to warm sandbox idle", time.Since(created))
	f.timing("first sandbox start to idle", warm.IdleAt.Sub(warm.CreatedAt))
}

// runJob runs one job on the warm sandbox and checks the job record, the
// log, the metrics, the events, and the removal of the sandbox. It returns
// the job.
func (f *flow) runJob() job {
	f.t.Log("one job on the warm sandbox")
	warm := f.waitIdle(nil)
	queued := time.Now()
	req := f.fake.Queue(fakegh.JobSpec{
		Repo: "acme/api", Name: "unit tests", Labels: []string{label},
		Steps: []fakegh.Step{{Name: "Run tests", Lines: []string{"e2e step output"}, Sleep: 15}},
	})
	j := f.waitJob("unit tests", 1, "running", time.Minute)
	f.timing("job queued to running (warm sandbox)", time.Since(queued))
	if fj := f.fakeJob(req); !fj.StartedAt.IsZero() {
		f.timing("job queued to started on GitHub", fj.StartedAt.Sub(fj.QueuedAt))
	}
	if j.SandboxID != warm.ID {
		f.t.Errorf("the job runs on sandbox %s, want the warm sandbox %s", j.SandboxID, warm.ID)
	}
	f.waitFor("the live log", 30*time.Second, func() (bool, error) {
		var text string
		_, err := f.api.do("GET", "/api/jobs/"+j.ID+"/log", nil, &text)
		return strings.Contains(text, "e2e step output"), err
	})
	var console string
	f.must("GET", "/api/sandboxes/"+warm.ID+"/log", nil, &console)
	if !strings.Contains(console, "Listening for Jobs") || !strings.Contains(console, "Running job: unit tests") {
		f.t.Errorf("sandbox log:\n%s\nwant the runner lines", console)
	}
	// GitHub counts the assigned job, so a second warm sandbox starts.
	next := f.waitIdle([]string{warm.ID})

	j = f.waitJob("unit tests", 1, "completed", time.Minute)
	f.waitFor("the stored log and the metrics", time.Minute, func() (bool, error) {
		f.must("GET", "/api/jobs/"+j.ID, nil, &j)
		return j.LogState == "stored" && j.CPUPeak != nil, nil
	})
	if j.result() != "succeeded" || j.RunNumber < 1 || len(j.HeadSHA) != 40 || j.HeadBranch != "main" || j.CommitMessage == "" ||
		j.Actor != "octocat" || j.Event != "push" || j.WorkflowName != "CI" || j.HTMLURL == "" {
		f.t.Errorf("job = %+v, want succeeded with the run data from GitHub", j)
	}
	var steps []string
	for _, s := range j.Steps {
		steps = append(steps, s.Name+":"+s.Conclusion)
	}
	if want := []string{"Set up job:success", "Run tests:success", "Complete job:success"}; !slices.Equal(steps, want) {
		f.t.Errorf("steps = %v, want %v", steps, want)
	}
	var text string
	f.must("GET", "/api/jobs/"+j.ID+"/log", nil, &text)
	if !strings.Contains(text, "##[group]Run tests") || !strings.Contains(text, "e2e step output") {
		f.t.Errorf("stored log:\n%s\nwant the step and its output", text)
	}
	var metrics struct {
		Samples []struct {
			CPUPercent float64 `json:"cpu_percent"`
			MemoryMB   int     `json:"memory_mb"`
		} `json:"samples"`
	}
	f.must("GET", "/api/jobs/"+j.ID+"/metrics", nil, &metrics)
	if len(metrics.Samples) == 0 || metrics.Samples[0].MemoryMB <= 0 {
		f.t.Errorf("metrics = %+v, want samples with memory", metrics)
	}

	f.waitGone(warm.ID, 2*time.Minute)
	// The sandbox that started during the job stays as the warm sandbox.
	time.Sleep(3 * time.Second)
	if live := f.live(); len(live) != 1 || live[0].ID != next.ID || live[0].State != "idle" {
		f.t.Errorf("sandboxes after the job = %+v, want only the warm sandbox %s", live, next.ID)
	}
	for _, check := range []struct {
		typ   string
		match func(map[string]any) bool
	}{
		{"job.state", func(d map[string]any) bool { return d["id"] == j.ID && d["state"] == "completed" }},
		{"sandbox.state", func(d map[string]any) bool { return d["id"] == warm.ID && d["state"] == "busy" }},
		{"sandbox.state", func(d map[string]any) bool { return d["id"] == warm.ID && d["state"] == "gone" }},
		{"pool.stats", func(d map[string]any) bool { return d["pool_id"] == f.poolID }},
	} {
		if !f.events.find(check.typ, check.match) {
			f.t.Errorf("no %s event in the stream for the job", check.typ)
		}
	}
	return j
}

// rejectPublic queues a job of a public repository, which dev mode, like
// container mode, does not run.
func (f *flow) rejectPublic() {
	f.t.Log("a job of a public repository")
	f.fake.Queue(fakegh.JobSpec{Repo: "acme/site", Name: "public", Labels: []string{label}})
	var rejected job
	f.waitFor("the rejected job", 30*time.Second, func() (bool, error) {
		var page struct {
			Items []job `json:"items"`
		}
		_, err := f.api.do("GET", "/api/jobs?repository=acme/site", nil, &page)
		if len(page.Items) == 1 {
			rejected = page.Items[0]
		}
		return rejected.RejectReason != "", err
	})
	if rejected.State != "queued" || !strings.Contains(rejected.RejectReason, "Public repository") {
		f.t.Errorf("job of a public repository = %s, %q; want queued with the reason", rejected.State, rejected.RejectReason)
	}
	time.Sleep(2 * time.Second)
	if live := f.live(); len(live) != minIdle {
		f.t.Errorf("sandboxes after the rejected job = %+v, want only the warm sandbox", live)
	}
}

// scaleOut runs more jobs than max_runners at once.
func (f *flow) scaleOut() {
	f.t.Log("four jobs at once")
	queued := time.Now()
	for i := range 4 {
		// Each job outlasts the start of two new runners, so all three run
		// at once. Two VMs that boot together in a nested VM take 8 seconds.
		f.fake.Queue(fakegh.JobSpec{
			Repo: "acme/web", Name: fmt.Sprintf("shard %d", i+1), Labels: []string{label},
			Steps: []fakegh.Step{{Name: "Run shard", Lines: []string{"shard"}, Sleep: 20}},
		})
	}
	most, busiest := 0, 0
	f.waitFor("the four jobs complete", 3*time.Minute, func() (bool, error) {
		live := f.live()
		runners, busy := 0, 0
		for _, b := range live {
			if b.State != "stopping" {
				runners++
			}
			if b.State == "busy" {
				busy++
			}
		}
		most, busiest = max(most, runners), max(busiest, busy)
		var page struct {
			Items []job `json:"items"`
		}
		_, err := f.api.do("GET", "/api/jobs?repository=acme/web", nil, &page)
		done := 0
		for _, j := range page.Items {
			if j.State == "completed" {
				if j.result() != "succeeded" {
					f.t.Fatalf("job %s: %s, want succeeded", j.JobName, j.result())
				}
				done++
			}
		}
		return done == 4, err
	})
	f.timing("4 jobs (max 3 runners) queued to all done", time.Since(queued))
	for _, j := range f.fake.Jobs() {
		if strings.HasPrefix(j.Name, "shard") {
			f.timing(j.Name+" queued to started", j.StartedAt.Sub(j.QueuedAt))
		}
	}
	if most > maxRunners || busiest != maxRunners {
		f.t.Errorf("at most %d runners and %d busy at once, want %d busy and no more runners", most, busiest, maxRunners)
	}
	f.waitFor("the pool scales back to min_idle", time.Minute, func() (bool, error) {
		live := f.live()
		return len(live) == minIdle && live[0].State == "idle", fmt.Errorf("live sandboxes %+v", live)
	})
}

// redeliver sends all messages again and checks that runplane records
// nothing twice. It needs the fake GitHub in the test process.
func (f *flow) redeliver() {
	f.t.Log("a message comes again")
	before := f.jobCount()
	sandboxes := f.sandboxCount()
	if !f.fake.(*fakegh.Server).Redeliver(f.scaleSetID) {
		f.t.Fatal("the fake has no messages to send again")
	}
	f.waitFor("the listener takes the message", 30*time.Second, func() (bool, error) {
		for _, s := range f.fake.ScaleSets() {
			if s.ID == f.scaleSetID {
				return s.Unacked == 0, nil
			}
		}
		return false, nil
	})
	time.Sleep(2 * time.Second)
	if n := f.jobCount(); n != before {
		f.t.Errorf("%d jobs after the message came again, want %d", n, before)
	}
	if n := f.sandboxCount(); n != sandboxes {
		f.t.Errorf("%d sandboxes after the message came again, want %d", n, sandboxes)
	}
}

// restartDuringJob restarts runplane while a job runs. The job goes on, and
// the new process takes over the sandbox and its control channel.
func (f *flow) restartDuringJob() {
	f.t.Log("restart during a job")
	f.fake.Queue(fakegh.JobSpec{
		Repo: "acme/api", Name: "restart", Labels: []string{label},
		Steps: []fakegh.Step{
			{Name: "Run before", Lines: []string{"before the restart"}, Sleep: 15},
			{Name: "Run after", Lines: []string{"after the restart"}, Sleep: 10},
		},
	})
	j := f.waitJob("restart", 1, "running", time.Minute)
	stopped := time.Now()
	f.stop()
	f.start()
	f.timing("runplane restart", time.Since(stopped))
	// The guest sends the line of the second step only on the new control
	// channel, and the job runs for 10 more seconds after it.
	f.waitFor("the live log after the restart", time.Minute, func() (bool, error) {
		var text string
		_, err := f.api.do("GET", "/api/jobs/"+j.ID+"/log", nil, &text)
		if err == nil && !strings.Contains(text, "after the restart") {
			f.must("GET", "/api/jobs/"+j.ID, nil, &j)
			if j.State == "completed" {
				f.t.Fatalf("the job completed, and the live log after the restart never showed its second step")
			}
		}
		return strings.Contains(text, "after the restart"), err
	})
	j = f.waitJob("restart", 1, "completed", time.Minute)
	if j.result() != "succeeded" {
		f.t.Errorf("the job across the restart: %s, want succeeded", j.result())
	}
	if !strings.Contains(f.log(), "took over sandbox") {
		f.t.Error("the log has no sandbox take-over after the restart")
	}
	f.waitGone(j.SandboxID, 2*time.Minute)
}

// debugRerun runs the job again with debug, opens the browser shell and
// the SSH shell during the hold, and waits for the end of the hold.
func (f *flow) debugRerun(first job) {
	f.t.Log("debug re-run")
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		f.t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.configDir, "authorized_keys"), ssh.MarshalAuthorizedKey(sshPub), 0o600); err != nil {
		f.t.Fatal(err)
	}

	f.must("POST", "/api/jobs/"+first.ID+"/rerun", map[string]bool{"debug": true}, nil)
	j := f.waitJob("unit tests", 2, "completed", 2*time.Minute)
	if !j.Debug || j.result() != "succeeded" {
		f.t.Errorf("re-run = debug %v, result %s; want a debug job that succeeded", j.Debug, j.result())
	}
	f.waitFor("the debug hold", 30*time.Second, func() (bool, error) {
		f.must("GET", "/api/jobs/"+j.ID, nil, &j)
		return j.HoldUntil != nil && j.SSHCommand != "", nil
	})
	if j.SandboxState == "gone" || j.SandboxState == "failed" || !j.HoldUntil.After(time.Now()) {
		f.t.Fatalf("debug sandbox %s is %s with hold %v, want it on hold", j.SandboxID, j.SandboxState, j.HoldUntil)
	}

	// The browser shell.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://"+f.addr+"/api/sandboxes/"+j.SandboxID+"/shell?rows=24&cols=80",
		&websocket.DialOptions{HTTPClient: f.api.http})
	if err != nil {
		f.t.Fatalf("open the browser shell: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageBinary, []byte("echo browser-$((40+2)) $(whoami)\n")); err != nil {
		f.t.Fatal(err)
	}
	var out bytes.Buffer
	for !strings.Contains(out.String(), "browser-42 runner") {
		_, b, err := ws.Read(ctx)
		if err != nil {
			f.t.Fatalf("browser shell: %v; output %q", err, out.String())
		}
		out.Write(b)
	}
	ws.Close(websocket.StatusNormalClosure, "")

	// The SSH gateway with an admin key.
	conn, err := ssh.Dial("tcp", f.sshAddr, &ssh.ClientConfig{
		User: j.SandboxID, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second,
	})
	if err != nil {
		f.t.Fatalf("SSH: %v", err)
	}
	defer conn.Close()
	sess, err := conn.NewSession()
	if err != nil {
		f.t.Fatal(err)
	}
	var sshOut syncBuffer
	sess.Stdout = &sshOut
	stdin, _ := sess.StdinPipe()
	if err := sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		f.t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		f.t.Fatal(err)
	}
	io.WriteString(stdin, "echo ssh-$((40+3)) $(hostname)\nexit\n")
	waited := make(chan error, 1)
	go func() { waited <- sess.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			f.t.Errorf("SSH shell: %v", err)
		}
	case <-time.After(20 * time.Second):
		f.t.Errorf("the SSH shell did not end")
	}
	if !strings.Contains(sshOut.String(), "ssh-43 runplane-") {
		f.t.Errorf("SSH output %q, want the command output from the sandbox", sshOut.String())
	}

	f.t.Logf("debug sandbox %s waits for the end of its hold at %s", j.SandboxID, j.HoldUntil.Format(time.TimeOnly))
	f.waitGone(j.SandboxID, time.Until(*j.HoldUntil)+time.Minute)
}

// runnerWithoutGitHub checks a runner that exits before it connects to
// GitHub, as the real runner does after a bad JIT config, also with exit
// code 0: runplane-init reports it, and the sandbox ends as failed with the
// reason.
func (f *flow) runnerWithoutGitHub() {
	f.t.Log("a runner that does not connect to GitHub")
	const broken = "e2e-broken"
	f.fake.(*fakegh.Server).BreakJIT(broken)
	var pool struct {
		ID      string `json:"id"`
		ImageID string `json:"image_id"`
	}
	f.must("GET", "/api/pools/"+f.poolID, nil, &pool)
	f.must("POST", "/api/pools", map[string]any{
		"label": broken, "vcpu": 1, "memory_mb": 1024, "disk_gb": 8, "min_idle": 1, "max_runners": 1, "image_id": pool.ImageID,
	}, &pool)
	var failed []sandboxRow
	f.waitFor("a failed sandbox", 2*time.Minute, func() (bool, error) {
		_, err := f.api.do("GET", "/api/sandboxes?state=failed&pool_id="+pool.ID, nil, &failed)
		return len(failed) > 0, err
	})
	if b := failed[0]; b.Error != "the runner did not connect to GitHub" || b.IdleAt != nil {
		f.t.Errorf("failed sandbox %+v, want the error \"the runner did not connect to GitHub\" and never idle", b)
	}
	f.checkRemoved(failed[0].ID)
	f.must("DELETE", "/api/pools/"+pool.ID, nil, nil)
	f.waitFor("the sandboxes of the broken pool stop", 2*time.Minute, func() (bool, error) {
		var live []sandboxRow
		_, err := f.api.do("GET", "/api/sandboxes?pool_id="+pool.ID, nil, &live)
		return len(live) == 0 && err == nil, fmt.Errorf("live sandboxes %+v: %v", live, err)
	})
}

// mirror runs docker pull in a job and checks that the registry mirror
// served it.
func (f *flow) mirror() {
	f.t.Log("registry mirror")
	var before struct {
		Mirror mirrorStats `json:"mirror"`
	}
	f.must("GET", "/api/caches", nil, &before)
	// The runner starts before dockerd, so the step waits for it.
	f.fake.Queue(fakegh.JobSpec{
		Repo: "acme/api", Name: "pull", Labels: []string{label},
		Steps: []fakegh.Step{{Name: "Run docker pull", Run: "for i in $(seq 60); do docker info >/dev/null 2>&1 && break; sleep 1; done; docker pull alpine:3.20"}},
	})
	j := f.waitJob("pull", 1, "completed", 3*time.Minute)
	if j.result() != "succeeded" {
		f.t.Errorf("the docker pull job: %s, want succeeded", j.result())
	}
	var after struct {
		Mirror mirrorStats `json:"mirror"`
	}
	f.must("GET", "/api/caches", nil, &after)
	if after.Mirror.Misses+after.Mirror.Hits <= before.Mirror.Misses+before.Mirror.Hits || after.Mirror.MissBytes == 0 {
		f.t.Errorf("mirror stats %+v before and %+v after the pull, want more requests and bytes", before.Mirror, after.Mirror)
	}
	f.waitGone(j.SandboxID, 2*time.Minute)
}

// dashboard reads each page of the dashboard after the jobs, and makes a
// backup as an admin does.
func (f *flow) dashboard() {
	f.t.Log("dashboard reads and a backup")
	var servers []struct {
		ID string `json:"id"`
	}
	f.must("GET", "/api/servers", nil, &servers)
	if len(servers) != 1 {
		f.t.Fatalf("servers = %+v, want the local server", servers)
	}
	for _, path := range []string{
		"/api/session", "/api/overview?range=24h", "/api/jobs/facets", "/api/pools", "/api/pools/" + f.poolID,
		"/api/pools/placement?arch=arm64&vcpu=1&memory_mb=1024", "/api/servers/" + servers[0].ID,
		"/api/servers/" + servers[0].ID + "/doctor", "/api/servers/ssh-key", "/api/images", "/api/caches",
		"/api/caches/entries", "/api/runner-versions", "/api/notifications", "/api/users", "/api/audit", "/install.sh",
	} {
		f.must("GET", path, nil, nil)
	}
	var status struct {
		HasJobs         bool `json:"has_jobs"`
		GitHubConnected bool `json:"github_connected"`
	}
	f.must("GET", "/api/status", nil, &status)
	if !status.HasJobs || !status.GitHubConnected {
		f.t.Errorf("status = %+v, want jobs and GitHub", status)
	}
	var settings struct {
		HTTPS *struct {
			State string `json:"state"`
		} `json:"https"`
	}
	f.must("GET", "/api/settings", nil, &settings)
	if settings.HTTPS == nil || settings.HTTPS.State != "none" {
		f.t.Errorf("settings.https = %+v, want state none without a domain", settings.HTTPS)
	}

	var backup struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	f.must("POST", "/api/backups", nil, &backup)
	f.waitFor("the backup events", 10*time.Second, func() (bool, error) {
		return f.events.find("backup.state", func(d map[string]any) bool { return d["id"] == backup.ID && d["status"] == "running" }) &&
			f.events.find("backup.state", func(d map[string]any) bool { return d["id"] == backup.ID && d["status"] == "done" }), nil
	})
	f.must("GET", "/api/backups/"+backup.ID+"/download", nil, nil)
}

// teardown stops a sandbox, deletes the pool, and disconnects GitHub.
func (f *flow) teardown() {
	f.t.Log("stop, delete, disconnect")
	warm := f.waitIdle(nil)
	f.must("POST", "/api/sandboxes/"+warm.ID+"/stop", nil, nil)
	f.waitGone(warm.ID, 2*time.Minute)
	f.waitIdle([]string{warm.ID})

	f.must("DELETE", "/api/pools/"+f.poolID, nil, nil)
	f.waitFor("the scale set is deleted on GitHub", time.Minute, func() (bool, error) {
		return len(f.fake.ScaleSets()) == 0, fmt.Errorf("scale sets %+v", f.fake.ScaleSets())
	})
	f.waitFor("the sandboxes of the pool stop", 2*time.Minute, func() (bool, error) {
		live := f.live()
		return len(live) == 0, fmt.Errorf("live sandboxes %+v", live)
	})

	f.must("DELETE", "/api/github", nil, nil)
	var gh struct {
		Connected bool `json:"connected"`
	}
	f.must("GET", "/api/github", nil, &gh)
	if gh.Connected {
		f.t.Error("GitHub is still connected after the disconnect")
	}

	var gone []sandboxRow
	f.must("GET", "/api/sandboxes?state=gone&pool_id="+f.poolID, nil, &gone)
	var total time.Duration
	n := 0
	for _, b := range gone {
		f.checkRemoved(b.ID)
		if b.IdleAt != nil {
			total += b.IdleAt.Sub(b.CreatedAt)
			n++
		}
	}
	if n > 0 {
		f.timing(fmt.Sprintf("sandbox start to idle, mean of %d", n), total/time.Duration(n))
	}
	var failed []sandboxRow
	f.must("GET", "/api/sandboxes?state=failed&pool_id="+f.poolID, nil, &failed)
	if len(failed) > 0 {
		f.t.Errorf("failed sandboxes: %+v", failed)
	}
}

// live returns the sandboxes of the pool that still operate.
func (f *flow) live() []sandboxRow {
	var list []sandboxRow
	f.must("GET", "/api/sandboxes?pool_id="+f.poolID, nil, &list)
	return list
}

// waitIdle waits for an idle sandbox that is not in skip.
func (f *flow) waitIdle(skip []string) sandboxRow {
	f.t.Helper()
	var found sandboxRow
	f.waitFor("an idle sandbox", 2*time.Minute, func() (bool, error) {
		for _, b := range f.live() {
			if b.State == "idle" && !slices.Contains(skip, b.ID) {
				found = b
				return true, nil
			}
		}
		return false, nil
	})
	return found
}

// waitGone waits until runplane removed the sandbox and checks that
// nothing of it is left.
func (f *flow) waitGone(id string, timeout time.Duration) {
	f.t.Helper()
	f.waitFor("sandbox "+id+" is gone", timeout, func() (bool, error) {
		var b sandboxRow
		_, err := f.api.do("GET", "/api/sandboxes/"+id, nil, &b)
		if b.State == "failed" {
			f.t.Fatalf("sandbox %s failed: %s", id, b.Error)
		}
		return b.State == "gone", err
	})
	f.checkRemoved(id)
}

// waitJob waits until the attempt of the job with the name is in the state,
// and returns the job with its details.
func (f *flow) waitJob(name string, attempt int, state string, timeout time.Duration) job {
	f.t.Helper()
	var found job
	f.waitFor(fmt.Sprintf("job %q attempt %d %s", name, attempt, state), timeout, func() (bool, error) {
		var page struct {
			Items []job `json:"items"`
		}
		if _, err := f.api.do("GET", "/api/jobs?q="+url.QueryEscape(name), nil, &page); err != nil {
			return false, err
		}
		for _, j := range page.Items {
			if j.JobName != name {
				continue
			}
			f.must("GET", "/api/jobs/"+j.ID, nil, &j)
			if j.RunAttempt == attempt && j.State == state {
				found = j
				return true, nil
			}
		}
		return false, nil
	})
	return found
}

func (f *flow) fakeJob(requestID int64) fakegh.Job {
	for _, j := range f.fake.Jobs() {
		if j.RequestID == requestID {
			return j
		}
	}
	f.t.Fatalf("the fake has no job %d", requestID)
	return fakegh.Job{}
}

func (f *flow) jobCount() int {
	var page struct {
		Total int `json:"total"`
	}
	f.must("GET", "/api/jobs?limit=1", nil, &page)
	return page.Total
}

// sandboxCount counts the sandboxes of the pool in all states.
func (f *flow) sandboxCount() int {
	n := len(f.live())
	for _, state := range []string{"gone", "failed"} {
		var list []sandboxRow
		f.must("GET", "/api/sandboxes?state="+state+"&pool_id="+f.poolID, nil, &list)
		n += len(list)
	}
	return n
}

// syncBuffer is a bytes.Buffer that the SSH session writes from its own
// goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
