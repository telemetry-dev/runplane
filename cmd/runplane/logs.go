package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
	"github.com/runplane/runplane/internal/sandbox/guest"
	"github.com/runplane/runplane/internal/store"
)

// logs serves the sandbox and job logs: the api Logs service.
type logs struct {
	st     *store.Store
	cfg    config.Config
	driver func(serverID string) sandbox.Driver
}

func (l logs) SandboxLog(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	b, err := l.st.GetSandbox(ctx, id)
	if err != nil {
		return nil, err
	}
	return l.sandboxLog(ctx, b, follow)
}

// JobLog returns the stored GitHub log of a finished job. While the job
// operates, it returns the step output that the sandbox streams: the "diag"
// lines of the sandbox log, which have the GitHub log format. The live log
// has no step marks, so it has all steps whatever step is.
func (l logs) JobLog(ctx context.Context, jobID string, step int, follow bool) (_ io.ReadCloser, live bool, _ error) {
	job, err := l.st.GetJob(ctx, jobID)
	if err != nil {
		return nil, false, err
	}
	if job.LogState == store.LogStored {
		f, err := os.Open(l.cfg.JobLogPath(job.ID))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, huma.Error404NotFound("The log file of this job is missing.")
		}
		if err != nil {
			return nil, false, err
		}
		if step > 0 {
			return filterLines(f, stepLines(job.Steps, step)), false, nil
		}
		return f, false, nil
	}
	if job.SandboxID != "" {
		// sandboxLog fails for a sandbox that stopped.
		if b, err := l.st.GetSandbox(ctx, job.SandboxID); err == nil {
			if rc, err := l.sandboxLog(ctx, b, follow); err == nil {
				return filterLines(rc, diagLines), true, nil
			}
		}
	}
	switch job.LogState {
	case store.LogExpired:
		return nil, false, huma.Error404NotFound("The log of this job expired.")
	case store.LogNone:
		if job.State == store.JobCompleted {
			return nil, false, huma.Error404NotFound("GitHub has no log for this job.")
		}
	}
	return nil, false, huma.Error404NotFound("This job has no log yet.")
}

func (l logs) sandboxLog(ctx context.Context, b store.Sandbox, follow bool) (io.ReadCloser, error) {
	if b.State.Done() {
		return nil, huma.Error404NotFound("The sandbox stopped, and its log went with it.")
	}
	drv := l.driver(b.ServerID)
	if drv == nil {
		return nil, huma.Error503ServiceUnavailable("The server of this sandbox is not connected.")
	}
	rc, err := drv.Logs(ctx, b.ID, follow)
	if errors.Is(err, sandbox.ErrNotFound) {
		return nil, huma.Error404NotFound("The sandbox stopped, and its log went with it.")
	}
	return rc, err
}

// diagLines keeps the step output of a sandbox log line.
func diagLines(line string) (string, bool) {
	l, ok := control.ParseLogFileLine(line)
	return l.Text, ok && l.Source == guest.SourceDiag
}

// stepLines keeps the lines of step n of a GitHub job log. Each line starts
// with a time, and the steps have their start times (to the second). A line
// belongs to the last step that started in an earlier second. In the second
// that a step starts, its first line opens it (see opensStep).
func stepLines(steps []store.JobStep, n int) func(string) (string, bool) {
	var started []store.JobStep
	for _, s := range steps {
		if s.StartedAt != nil {
			started = append(started, s)
		}
	}
	cur := 0
	return func(line string) (string, bool) {
		line = strings.TrimPrefix(line, "\ufeff")
		if len(started) == 0 {
			return line, false
		}
		stamp, text, _ := strings.Cut(line, " ")
		if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			sec := t.Truncate(time.Second)
			for cur+1 < len(started) && started[cur+1].StartedAt.Before(sec) {
				cur++
			}
			if cur+1 < len(started) && started[cur+1].StartedAt.Equal(sec) && opensStep(text, started[cur+1].Name) {
				cur++
			}
		}
		return line, started[cur].Number == n
	}
}

// opensStep reports if a log line is the first line of the step: a group
// with the step name, a "Run ..." group of an unnamed step, or the first
// line that the runner writes for a post step or for "Complete job".
func opensStep(text, name string) bool {
	if title, ok := strings.CutPrefix(text, "##[group]"); ok {
		return title == name || strings.HasPrefix(title, "Run ")
	}
	switch {
	case name == "Complete job":
		return text == "Cleaning up orphan processes"
	case strings.HasPrefix(name, "Post "):
		return text == "Post job cleanup."
	}
	return false
}

// filterLines passes the lines of src that keep returns true for, as keep
// changes them.
func filterLines(src io.ReadCloser, keep func(string) (string, bool)) io.ReadCloser {
	return &lineFilter{src: src, r: bufio.NewReader(src), keep: keep}
}

type lineFilter struct {
	src  io.ReadCloser
	r    *bufio.Reader
	keep func(string) (string, bool)
	buf  []byte
	err  error
}

func (f *lineFilter) Read(p []byte) (int, error) {
	for len(f.buf) == 0 {
		if f.err != nil {
			return 0, f.err
		}
		line, err := f.r.ReadString('\n')
		if line != "" {
			if out, ok := f.keep(strings.TrimSuffix(line, "\n")); ok {
				f.buf = append(f.buf, out...)
				f.buf = append(f.buf, '\n')
			}
		}
		f.err = err
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

func (f *lineFilter) Close() error { return f.src.Close() }
