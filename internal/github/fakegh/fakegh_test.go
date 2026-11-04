package fakegh_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/runplane/runplane/internal/github/fakegh"
)

var runnerDir, _ = filepath.Abs("../../../testdata/fakerunner")

// TestJobLifecycle drives the real scaleset client and listener through a
// job: the scale set gets JobAvailable, acquires the job, starts the fake
// runner with a JIT config, and gets JobStarted and JobCompleted.
func TestJobLifecycle(t *testing.T) {
	needTools(t)
	f := start(t, fakegh.Config{})
	set := f.createScaleSet(t, "runplane")
	events := f.listen(t, set.ID, 2)

	req := f.srv.Queue(fakegh.JobSpec{
		Repo:   "acme/api",
		Name:   "test (node 22)",
		Labels: []string{"RunPlane"},
		Steps:  []fakegh.Step{{Name: "Run go test ./...", Lines: []string{"ok  \texample.com/pkg\t0.01s"}, Sleep: 0.2}},
	})
	events.wait(t, scaleset.MessageTypeJobAvailable, req)
	events.wait(t, scaleset.MessageTypeJobAssigned, req)

	jit, err := f.client.GenerateJitRunnerConfig(t.Context(), &scaleset.RunnerScaleSetJitRunnerSetting{Name: "runplane-01"}, set.ID)
	if err != nil {
		t.Fatal(err)
	}
	dir := copyBundle(t)
	out, err := exec.CommandContext(t.Context(), filepath.Join(dir, "run.sh"), "--jitconfig", jit.EncodedJITConfig).CombinedOutput()
	if err != nil {
		t.Fatalf("run.sh: %v\n%s", err, out)
	}

	started := events.wait(t, scaleset.MessageTypeJobStarted, req)
	completed := events.wait(t, scaleset.MessageTypeJobCompleted, req)
	if started.runnerID != jit.Runner.ID || started.runnerName != "runplane-01" {
		t.Errorf("JobStarted runner = %d %q, want %d runplane-01", started.runnerID, started.runnerName, jit.Runner.ID)
	}
	if completed.result != "succeeded" {
		t.Errorf("JobCompleted result = %q, want succeeded", completed.result)
	}
	// runplane sizes the pool from these counts.
	if st := started.stats; st.TotalAssignedJobs != 1 || st.TotalRunningJobs != 1 || st.TotalBusyRunners != 1 || st.TotalIdleRunners != 0 {
		t.Errorf("statistics with the job running = %+v", st)
	}
	if st := completed.stats; st.TotalAssignedJobs != 0 || st.TotalRunningJobs != 0 || st.TotalRegisteredRunners != 0 {
		t.Errorf("statistics after the job = %+v", st)
	}

	// These are the lines that runplane-init reads, in this order.
	want := []string{
		`^√ Connected to GitHub$`,
		`^Current runner version: '2\.330\.0'$`,
		`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\dZ: Listening for Jobs$`,
		`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\dZ: Running job: test \(node 22\)$`,
		`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\dZ: Job test \(node 22\) completed with result: Succeeded$`,
	}
	lines := strings.Split(string(out), "\n")
	for _, re := range want {
		i := indexMatch(lines, re)
		if i < 0 {
			t.Fatalf("run.sh output has no line %s after the lines before it:\n%s", re, out)
		}
		lines = lines[i+1:]
	}
	pages, _ := filepath.Glob(filepath.Join(dir, "_diag", "pages", "*.log"))
	if len(pages) != 1 {
		t.Fatalf("pages = %v, want one page for the one step", pages)
	}
	page, _ := os.ReadFile(pages[0])
	if !regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{7}Z ok  \texample\.com/pkg\t0\.01s$`).Match(page) {
		t.Errorf("page has no timestamped step line:\n%s", page)
	}
	if r := f.srv.Runners(); len(r) != 0 {
		t.Errorf("runners after the job = %v, want none: a JIT runner does one job", r)
	}
}

// TestRunnerEnd covers each way a busy runner ends its job and the result
// that the scale set sees.
func TestRunnerEnd(t *testing.T) {
	needTools(t)
	tests := []struct {
		name string
		end  func(f *fake, cmd *exec.Cmd, req int64)
		want string
	}{
		{"canceled on GitHub", func(f *fake, _ *exec.Cmd, req int64) { f.srv.CancelJob(req) }, "canceled"},
		{"stopped by runplane-init", func(_ *fake, cmd *exec.Cmd, _ int64) { cmd.Process.Signal(syscall.SIGTERM) }, "canceled"},
		{"killed", func(_ *fake, cmd *exec.Cmd, _ int64) { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }, "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := start(t, fakegh.Config{RunnerTimeout: time.Second})
			set := f.createScaleSet(t, "runplane")
			events := f.listen(t, set.ID, 1)
			req := f.srv.Queue(fakegh.JobSpec{Repo: "acme/api", Labels: []string{"runplane"}, Steps: []fakegh.Step{{Name: "Sleep", Sleep: 60}}})
			cmd := f.startRunner(t, set.ID, "runplane-01")
			events.wait(t, scaleset.MessageTypeJobStarted, req)

			tt.end(f, cmd, req)
			if got := events.wait(t, scaleset.MessageTypeJobCompleted, req).result; got != tt.want {
				t.Errorf("result = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRemoveRunner checks the errors that runplane relies on to stop only
// idle runners.
func TestRemoveRunner(t *testing.T) {
	needTools(t)
	f := start(t, fakegh.Config{})
	set := f.createScaleSet(t, "runplane")
	events := f.listen(t, set.ID, 1)
	ctx := t.Context()

	idle, err := f.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: "idle"}, set.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.client.RemoveRunner(ctx, int64(idle.Runner.ID)); err != nil {
		t.Fatalf("remove idle runner: %v", err)
	}
	if _, err := f.client.GetRunner(ctx, idle.Runner.ID); !errors.Is(err, scaleset.RunnerNotFoundError) {
		t.Errorf("get removed runner: err = %v, want RunnerNotFoundError", err)
	}
	if err := f.client.RemoveRunner(ctx, int64(idle.Runner.ID)); !errors.Is(err, scaleset.RunnerNotFoundError) {
		t.Errorf("remove removed runner: err = %v, want RunnerNotFoundError", err)
	}

	req := f.srv.Queue(fakegh.JobSpec{Repo: "acme/api", Labels: []string{"runplane"}, Steps: []fakegh.Step{{Name: "Sleep", Sleep: 60}}})
	f.startRunner(t, set.ID, "busy")
	started := events.wait(t, scaleset.MessageTypeJobStarted, req)
	if err := f.client.RemoveRunner(ctx, int64(started.runnerID)); !errors.Is(err, scaleset.JobStillRunningError) {
		t.Errorf("remove busy runner: err = %v, want JobStillRunningError", err)
	}
	if ref, err := f.client.GetRunnerByName(ctx, "busy"); err != nil || ref == nil || ref.ID != started.runnerID {
		t.Errorf("busy runner by name = %v, %v; want ID %d", ref, err, started.runnerID)
	}
}

// TestJobsGoToMatchingScaleSets checks that a job goes only to the scale
// sets that cover its repository and have all its labels, and that a new
// scale set gets the jobs that wait for it.
func TestJobsGoToMatchingScaleSets(t *testing.T) {
	f := start(t, fakegh.Config{PollTimeout: 200 * time.Millisecond})
	org := f.createScaleSet(t, "runplane")
	repoFake := f.withScope(t, "acme/api")
	repo := repoFake.createScaleSet(t, "runplane")

	web := f.srv.QueueJob("acme/web", "ci.yml", "build", []string{"runplane"})
	api := f.srv.QueueJob("acme/api", "ci.yml", "build", []string{"runplane"})
	large := f.srv.QueueJob("acme/api", "ci.yml", "build", []string{"runplane", "large"})

	if got := f.available(t, org.ID); !slices.Equal(got, []int64{web, api}) {
		t.Errorf("organization scale set got %v, want %v", got, []int64{web, api})
	}
	if got := repoFake.available(t, repo.ID); !slices.Equal(got, []int64{api}) {
		t.Errorf("repository scale set got %v, want %v", got, []int64{api})
	}

	big := f.createScaleSet(t, "runplane-large", "runplane", "large")
	if got := f.available(t, big.ID); !slices.Equal(got, []int64{web, api, large}) {
		t.Errorf("new scale set got %v, want %v", got, []int64{web, api, large})
	}
}

// TestMaxCapacity checks that JobAvailable waits while the scale set has
// maxCapacity assigned jobs, as GitHub does.
func TestMaxCapacity(t *testing.T) {
	f := start(t, fakegh.Config{PollTimeout: 200 * time.Millisecond})
	set := f.createScaleSet(t, "runplane")
	sess := f.session(t, set.ID)
	ctx := t.Context()
	first := f.srv.QueueJob("acme/api", "ci.yml", "a", []string{"runplane"})
	second := f.srv.QueueJob("acme/api", "ci.yml", "b", []string{"runplane"})

	if msg, err := sess.GetMessage(ctx, 0, 0); err != nil || msg != nil {
		t.Fatalf("poll with maxCapacity 0 = %v, %v; want no message", msg, err)
	}
	msg, err := sess.GetMessage(ctx, 0, 1)
	if err != nil || msg == nil || len(msg.JobAvailableMessages) != 1 || msg.JobAvailableMessages[0].RunnerRequestID != first {
		t.Fatalf("poll with maxCapacity 1 = %+v, %v; want JobAvailable %d only", msg, err, first)
	}
	if _, err := sess.AcquireJobs(ctx, []int64{first}); err != nil {
		t.Fatal(err)
	}
	if err := sess.DeleteMessage(ctx, msg.MessageID); err != nil {
		t.Fatal(err)
	}
	// The acquired job fills the one place, so the second job waits.
	msg, err = sess.GetMessage(ctx, msg.MessageID, 1)
	if err != nil || msg == nil || len(msg.JobAvailableMessages) != 0 || len(msg.JobAssignedMessages) != 1 {
		t.Fatalf("poll after acquire = %+v, %v; want only JobAssigned", msg, err)
	}
	if msg.Statistics.TotalAssignedJobs != 1 || msg.Statistics.TotalAvailableJobs != 1 {
		t.Errorf("statistics = %+v, want 1 assigned and 1 available", *msg.Statistics)
	}
	msg, err = sess.GetMessage(ctx, msg.MessageID, 2)
	if err != nil || msg == nil || len(msg.JobAvailableMessages) != 1 || msg.JobAvailableMessages[0].RunnerRequestID != second {
		t.Fatalf("poll with maxCapacity 2 = %+v, %v; want JobAvailable %d", msg, err, second)
	}
}

// TestSessionConflict checks that a scale set has one live message session
// and that the session of a listener that died expires.
func TestSessionConflict(t *testing.T) {
	f := start(t, fakegh.Config{SessionTimeout: 300 * time.Millisecond, PollTimeout: 200 * time.Millisecond})
	set := f.createScaleSet(t, "runplane")
	ctx := t.Context()

	first := f.session(t, set.ID)
	if _, err := f.client.MessageSessionClient(ctx, set.ID, "second"); !errors.Is(err, scaleset.ConflictError) {
		t.Fatalf("second session: err = %v, want ConflictError", err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	f.session(t, set.ID)

	time.Sleep(400 * time.Millisecond) // the listener of the second session died
	third := f.session(t, set.ID)
	if _, err := third.GetMessage(ctx, 0, 1); err != nil {
		t.Errorf("poll of the new session: %v", err)
	}
}

// TestMessageRedelivery checks that a new session gets the messages that
// the listener did not delete, and only those.
func TestMessageRedelivery(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%v", deleted), func(t *testing.T) {
			f := start(t, fakegh.Config{PollTimeout: 200 * time.Millisecond})
			set := f.createScaleSet(t, "runplane")
			ctx := t.Context()
			f.srv.QueueJob("acme/api", "ci.yml", "build", []string{"runplane"})

			sess := f.session(t, set.ID)
			msg, err := sess.GetMessage(ctx, 0, 1)
			if err != nil || msg == nil {
				t.Fatalf("first poll = %v, %v", msg, err)
			}
			if deleted {
				sess.DeleteMessage(ctx, msg.MessageID)
			}
			sess.Close(ctx) // the listener stops

			again, err := f.session(t, set.ID).GetMessage(ctx, 0, 1)
			if err != nil {
				t.Fatal(err)
			}
			if got := again != nil && again.MessageID == msg.MessageID; got == deleted {
				t.Errorf("new session got message %d again: %v, want %v", msg.MessageID, got, !deleted)
			}
		})
	}
}

// TestRedeliver checks that Redeliver sends a deleted message again with a
// new ID, so that a listener with the last message ID still takes it.
func TestRedeliver(t *testing.T) {
	f := start(t, fakegh.Config{PollTimeout: 200 * time.Millisecond})
	set := f.createScaleSet(t, "runplane")
	ctx := t.Context()
	if f.srv.Redeliver(set.ID) {
		t.Fatal("Redeliver with no messages sent returned true")
	}
	req := f.srv.QueueJob("acme/api", "ci.yml", "build", []string{"runplane"})

	sess := f.session(t, set.ID)
	msg, err := sess.GetMessage(ctx, 0, 1)
	if err != nil || msg == nil {
		t.Fatalf("first poll = %v, %v", msg, err)
	}
	sess.DeleteMessage(ctx, msg.MessageID)
	if !f.srv.Redeliver(set.ID) || f.srv.ScaleSets()[0].Unacked != 1 {
		t.Fatalf("after Redeliver: scale sets %+v, want one message that waits", f.srv.ScaleSets())
	}
	again, err := sess.GetMessage(ctx, msg.MessageID, 1)
	if err != nil || again == nil || again.MessageID <= msg.MessageID {
		t.Fatalf("poll after Redeliver = %+v, %v; want a message after %d", again, err, msg.MessageID)
	}
	if len(again.JobAvailableMessages) != 1 || again.JobAvailableMessages[0].RunnerRequestID != req {
		t.Errorf("redelivered jobs = %+v, want the JobAvailable of %d", again.JobAvailableMessages, req)
	}
	sess.DeleteMessage(ctx, again.MessageID)
	if n := f.srv.ScaleSets()[0].Unacked; n != 0 {
		t.Errorf("%d messages wait after the delete, want 0", n)
	}
}

type fake struct {
	srv    *fakegh.Server
	url    string
	client *scaleset.Client
}

func start(t *testing.T, cfg fakegh.Config) *fake {
	t.Helper()
	cfg.RunnerDir = runnerDir
	srv := fakegh.New(cfg)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		srv.Close()
		ts.Close()
	})
	f := &fake{srv: srv, url: ts.URL}
	return f.withScope(t, "acme")
}

// withScope returns a copy with a client for the config URL of scope.
func (f *fake) withScope(t *testing.T, scope string) *fake {
	t.Helper()
	c, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: f.url + "/" + scope,
		GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: fakegh.AppClientID, InstallationID: 1, PrivateKey: string(f.srv.PrivateKey())},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fake{srv: f.srv, url: f.url, client: c}
}

func (f *fake) createScaleSet(t *testing.T, name string, labels ...string) *scaleset.RunnerScaleSet {
	t.Helper()
	group, err := f.client.GetRunnerGroupByName(t.Context(), scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatal(err)
	}
	in := &scaleset.RunnerScaleSet{Name: name, RunnerGroupID: group.ID}
	for _, l := range labels {
		in.Labels = append(in.Labels, scaleset.Label{Name: l})
	}
	set, err := f.client.CreateRunnerScaleSet(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func (f *fake) session(t *testing.T, setID int) *scaleset.MessageSessionClient {
	t.Helper()
	sess, err := f.client.MessageSessionClient(t.Context(), setID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// available returns the runner request IDs of the JobAvailable messages
// that one poll gets.
func (f *fake) available(t *testing.T, setID int) []int64 {
	t.Helper()
	sess := f.session(t, setID)
	defer sess.Close(context.Background())
	msg, err := sess.GetMessage(t.Context(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	if msg != nil {
		for _, j := range msg.JobAvailableMessages {
			ids = append(ids, j.RunnerRequestID)
		}
	}
	return ids
}

// startRunner makes a JIT config and starts run.sh in its own process
// group. The test ends the runner.
func (f *fake) startRunner(t *testing.T, setID int, name string) *exec.Cmd {
	t.Helper()
	jit, err := f.client.GenerateJitRunnerConfig(t.Context(), &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, setID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(copyBundle(t), "run.sh"), "--jitconfig", jit.EncodedJITConfig)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	})
	return cmd
}

// listen runs a listener with a scaler that acquires each available job.
func (f *fake) listen(t *testing.T, setID, maxRunners int) *recorder {
	t.Helper()
	sess := f.session(t, setID)
	l, err := listener.New(sess, listener.Config{ScaleSetID: setID, MaxRunners: maxRunners})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{client: sess, changed: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := l.Run(ctx, rec); err != nil && ctx.Err() == nil {
			t.Errorf("listener: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		sess.Close(context.Background())
	})
	return rec
}

type event struct {
	kind       scaleset.MessageType
	requestID  int64
	runnerID   int
	runnerName string
	result     string
	stats      scaleset.RunnerScaleSetStatistic // of the message
}

type recorder struct {
	client  listener.Client
	mu      sync.Mutex
	events  []event
	changed chan struct{}
}

func (r *recorder) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		return nil
	}
	var ids []int64
	var got []event
	for _, j := range msg.JobAvailableMessages {
		ids = append(ids, j.RunnerRequestID)
		got = append(got, event{kind: j.MessageType, requestID: j.RunnerRequestID})
	}
	for _, j := range msg.JobAssignedMessages {
		got = append(got, event{kind: j.MessageType, requestID: j.RunnerRequestID})
	}
	for _, j := range msg.JobStartedMessages {
		got = append(got, event{kind: j.MessageType, requestID: j.RunnerRequestID, runnerID: j.RunnerID, runnerName: j.RunnerName})
	}
	for _, j := range msg.JobCompletedMessages {
		got = append(got, event{kind: j.MessageType, requestID: j.RunnerRequestID, runnerID: j.RunnerID, runnerName: j.RunnerName, result: j.Result})
	}
	if len(ids) > 0 {
		if _, err := r.client.AcquireJobs(ctx, ids); err != nil {
			return err
		}
	}
	for i := range got {
		got[i].stats = *msg.Statistics
	}
	r.mu.Lock()
	r.events = append(r.events, got...)
	r.mu.Unlock()
	select {
	case r.changed <- struct{}{}:
	default:
	}
	return nil
}

// wait returns the first event of kind for the job, or fails after 15 seconds.
func (r *recorder) wait(t *testing.T, kind scaleset.MessageType, requestID int64) event {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		r.mu.Lock()
		for _, e := range r.events {
			if e.kind == kind && e.requestID == requestID {
				r.mu.Unlock()
				return e
			}
		}
		seen := slices.Clone(r.events)
		r.mu.Unlock()
		select {
		case <-r.changed:
		case <-timeout:
			t.Fatalf("no %s for job %d; events: %+v", kind, requestID, seen)
		}
	}
}

func needTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "jq", "base64"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("the fake runner needs %s", tool)
		}
	}
}

// copyBundle copies the fake runner to a new folder, as runplane-init gives
// each runner a writable copy.
func copyBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(runnerDir)); err != nil {
		t.Fatal(err)
	}
	return dir
}

func indexMatch(lines []string, re string) int {
	rx := regexp.MustCompile(re)
	for i, l := range lines {
		if rx.MatchString(l) {
			return i
		}
	}
	return -1
}
