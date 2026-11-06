package joblog

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// jobLog is a GitHub job log. "Set up job" has a group of its own in the
// second that the checkout step starts, and the "Test" step has a name
// that differs from its "Run" group. The post step and "Complete job" start
// in the last second of "Test" and have no group.
const jobLog = "\ufeff2026-11-04T10:00:00.1000000Z Requested labels: linux\n" +
	"2026-11-04T10:00:01.0000000Z Current runner version: '2.330.0'\n" +
	"2026-11-04T10:00:02.1000000Z ##[group]Operating System\n" +
	"2026-11-04T10:00:02.2000000Z Ubuntu\n" +
	"2026-11-04T10:00:02.3000000Z ##[endgroup]\n" +
	"2026-11-04T10:00:02.5000000Z ##[group]Run actions/checkout@v4\n" +
	"2026-11-04T10:00:02.6000000Z ##[endgroup]\n" +
	"2026-11-04T10:00:03.1000000Z Syncing repository\n" +
	"2026-11-04T10:00:05.2000000Z checkout done\n" +
	"2026-11-04T10:00:05.4000000Z ##[group]Run make test\n" +
	"2026-11-04T10:00:05.5000000Z ##[endgroup]\n" +
	"2026-11-04T10:00:06.0000000Z ok\n" +
	"2026-11-04T10:00:07.1000000Z tests passed\n" +
	"2026-11-04T10:00:07.2000000Z Post job cleanup.\n" +
	"2026-11-04T10:00:07.3000000Z [command]/usr/bin/git version\n" +
	"2026-11-04T10:00:07.6000000Z Cleaning up orphan processes\n"

func jobSteps() []store.JobStep {
	at := func(sec int) *time.Time {
		t := time.Date(2026, 11, 4, 10, 0, sec, 0, time.UTC)
		return &t
	}
	return []store.JobStep{
		{Number: 1, Name: "Set up job", StartedAt: at(1)},
		{Number: 2, Name: "Run actions/checkout@v4", StartedAt: at(2)},
		{Number: 3, Name: "Test", StartedAt: at(5)},
		{Number: 4, Name: "Deploy", Conclusion: "skipped"},
		{Number: 5, Name: "Post Run actions/checkout@v4", StartedAt: at(7)},
		{Number: 6, Name: "Complete job", StartedAt: at(7)},
	}
}

func TestStepLines(t *testing.T) {
	tests := []struct {
		step int
		want []string // texts after the time
	}{
		{1, []string{"Requested labels: linux", "Current runner version: '2.330.0'", "##[group]Operating System", "Ubuntu", "##[endgroup]"}},
		{2, []string{"##[group]Run actions/checkout@v4", "##[endgroup]", "Syncing repository", "checkout done"}},
		{3, []string{"##[group]Run make test", "##[endgroup]", "ok", "tests passed"}},
		{4, nil},
		{5, []string{"Post job cleanup.", "[command]/usr/bin/git version"}},
		{6, []string{"Cleaning up orphan processes"}},
	}
	for _, tt := range tests {
		got := readAll(t, filterLines(io.NopCloser(strings.NewReader(jobLog)), stepLines(jobSteps(), tt.step)))
		var texts []string
		for line := range strings.Lines(got) {
			line = strings.TrimSuffix(line, "\n")
			stamp, text, _ := strings.Cut(line, " ")
			if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
				t.Errorf("step %d: line %q has no time", tt.step, line)
			}
			texts = append(texts, text)
		}
		if strings.Join(texts, "|") != strings.Join(tt.want, "|") {
			t.Errorf("step %d:\n got %q\nwant %q", tt.step, texts, tt.want)
		}
	}
}

func TestJobLog(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	dir := t.TempDir()
	newJob := func(id int64, state, logState, sandboxID string) store.Job {
		t.Helper()
		j, _, err := st.UpsertJob(ctx, store.JobMessage{RunnerRequestID: id, State: state, Repository: "acme/api"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetJobLogState(ctx, j.ID, logState); err != nil {
			t.Fatal(err)
		}
		if sandboxID != "" {
			if j, _, err = st.LinkJobSandbox(ctx, j.ID, sandboxID); err != nil {
				t.Fatal(err)
			}
		}
		return j
	}
	stored := newJob(1, store.JobCompleted, store.LogStored, "")
	if err := st.SetJobEnrichment(ctx, stored.ID, store.JobEnrichment{Steps: jobSteps()}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stored.ID+".log"), []byte(jobLog), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertSandbox(ctx, store.Sandbox{ID: "box", PoolID: "p", ServerID: "s1", Mode: sandbox.ModeDev,
		State: sandbox.StateBusy, VCPU: 1, MemoryMB: 1024, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	running := newJob(2, store.JobRunning, store.LogLive, "box")
	expired := newJob(3, store.JobCompleted, store.LogExpired, "")

	console := "2026-11-04T10:00:00.000Z init connected\n" +
		"2026-11-04T10:00:01.000Z runner Running job: test\n" +
		"2026-11-04T10:00:01.500Z diag 2026-11-04T10:00:01.4000000Z ##[group]Run make test\n" +
		"2026-11-04T10:00:02.000Z dockerd API listen on /var/run/docker.sock\n" +
		"2026-11-04T10:00:02.500Z diag 2026-11-04T10:00:02.4000000Z ok\n"
	l := New(Config{Dir: dir, Store: st, Driver: func(serverID string) Driver {
		if serverID != "s1" {
			return nil
		}
		return consoleDriver{log: console}
	}})

	rc, live, err := l.Job(ctx, stored.ID, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc); live || !strings.HasPrefix(got, "2026-11-04T10:00:05.4000000Z ##[group]Run make test\n") || strings.Contains(got, "checkout") {
		t.Errorf("step 3 of the stored log (live %v):\n%s", live, got)
	}

	// The live log has no step marks, so a step gives the whole log.
	want := "2026-11-04T10:00:01.4000000Z ##[group]Run make test\n2026-11-04T10:00:02.4000000Z ok\n"
	for _, step := range []int{0, 2} {
		rc, live, err = l.Job(ctx, running.ID, step, true)
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, rc); !live || got != want {
			t.Errorf("log of a running job with step %d (live %v):\n got %q\nwant %q", step, live, got, want)
		}
	}

	if _, _, err := l.Job(ctx, expired.ID, 0, false); !errors.Is(err, ErrExpired) {
		t.Errorf("log of an expired job: %v, want ErrExpired", err)
	}
}

// consoleDriver is a driver whose sandboxes all have one console log.
type consoleDriver struct{ log string }

func (d consoleDriver) Logs(context.Context, string, bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(d.log)), nil
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
