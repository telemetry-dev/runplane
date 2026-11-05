// Package sysboxtest has the helpers of the sandbox tests that start real
// containers: a fake runner bundle, an event watcher, and runplane-init.
package sysboxtest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

// fakeRunner stands in for the actions runner. It prints what the tests
// check and the lines that runplane-init watches for. The test starts the
// job with the file start-<jit config> and ends it with end-<jit config>.
// The job keeps one CPU busy. The sandbox mounts the bundle at
// /.runplane/runner.
const fakeRunner = `#!/bin/bash
dir=/.runplane/runner
echo "jit=$2"
cat .env
awk '$2=="/mnt/scratch" || $2=="/var/lib/docker" || $2=="/tmp" || $2=="/home" || $2=="/opt" {print "mount " $2 " " $3}' /proc/mounts
echo "√ Connected to GitHub"
echo "Listening for Jobs"
while [ ! -e "$dir/start-$2" ]; do sleep 0.2; done
echo "Running job: test"
if command -v docker >/dev/null; then
  for i in $(seq 1 120); do
    if docker info >/dev/null 2>&1; then
      echo "docker storage=$(docker info --format '{{.Driver}}')"
      break
    fi
    sleep 0.5
  done
fi
while [ ! -e "$dir/end-$2" ]; do :; done
echo "Job test completed with result: Succeeded"
`

// Bundle is a fake runner bundle on the host.
type Bundle struct{ Dir string }

func NewBundle(t *testing.T, dir string) Bundle {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(fakeRunner), 0o755); err != nil {
		t.Fatal(err)
	}
	return Bundle{Dir: dir}
}

// Runner stands in for the Runner option of the driver: a bundle with the
// runner patch.
func (b Bundle) Runner(context.Context, string, string) (string, bool, error) {
	return b.Dir, true, nil
}

// Signal makes the file <name>-<jit config> that the fake runner waits for.
// The tests use the JIT config "jit-<sandbox id>".
func (b Bundle) Signal(t *testing.T, name, id string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(b.Dir, name+"-jit-"+id), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Init returns runplane-init for linux/goarch: the file in
// RUNPLANE_TEST_INIT, or a new build.
func Init(t *testing.T, dir, goarch string) string {
	t.Helper()
	if p := os.Getenv("RUNPLANE_TEST_INIT"); p != "" {
		return p
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	path := filepath.Join(dir, "runplane-init")
	cmd := exec.Command("go", "build", "-o", path, "github.com/runplane/runplane/init")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build runplane-init: %v\n%s", err, out)
	}
	return path
}

// Watcher keeps the events of a driver.
type Watcher struct {
	mu     sync.Mutex
	events []sandbox.Event
	more   chan struct{} // closed and replaced on each event
}

func Watch(events <-chan sandbox.Event) *Watcher {
	w := &Watcher{more: make(chan struct{})}
	go func() {
		for ev := range events {
			w.mu.Lock()
			w.events = append(w.events, ev)
			close(w.more)
			w.more = make(chan struct{})
			w.mu.Unlock()
		}
	}()
	return w
}

// States lists the state events of a sandbox.
func (w *Watcher) States(id string) []sandbox.State {
	w.mu.Lock()
	defer w.mu.Unlock()
	var states []sandbox.State
	for _, ev := range w.events {
		if ev.SandboxID == id && ev.Type == sandbox.EventState {
			states = append(states, ev.State)
		}
	}
	return states
}

// WaitFor returns the first event of the sandbox that match accepts. It
// fails the test at the timeout, or when the sandbox ends first.
func (w *Watcher) WaitFor(t *testing.T, id, what string, timeout time.Duration, match func(sandbox.Event) bool) sandbox.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		w.mu.Lock()
		more, events := w.more, slices.Clone(w.events)
		w.mu.Unlock()
		for _, ev := range events {
			if ev.SandboxID != id {
				continue
			}
			if match(ev) {
				return ev
			}
			if ev.Type == sandbox.EventState && ev.State.Done() {
				t.Fatalf("the sandbox ended %s (%s) before %s", ev.State, ev.Reason, what)
			}
		}
		select {
		case <-more:
		case <-deadline:
			t.Fatalf("no %s after %s; events: %+v", what, timeout, events)
		}
	}
}

// Wait returns the time of the state event.
func (w *Watcher) Wait(t *testing.T, id string, st sandbox.State, timeout time.Duration) time.Time {
	t.Helper()
	return w.WaitFor(t, id, "state "+string(st), timeout, func(ev sandbox.Event) bool {
		return ev.Type == sandbox.EventState && ev.State == st
	}).Time
}

func (w *Watcher) WaitRunner(t *testing.T, id, kind string, timeout time.Duration) {
	t.Helper()
	w.WaitFor(t, id, "runner event "+kind, timeout, func(ev sandbox.Event) bool {
		return ev.Type == sandbox.EventRunner && ev.Runner.Kind == kind
	})
}

// ReadAll reads r in the background; the result waits until r ends.
func ReadAll(r io.Reader) func() string {
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, r)
		close(done)
	}()
	return func() string {
		<-done
		return buf.String()
	}
}

// ReadUntil reads r until the output has want or the time is up.
func ReadUntil(r io.Reader, want string, timeout time.Duration) string {
	out := &syncBuffer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			out.Write(buf[:n])
			if strings.Contains(out.String(), want) || err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
	return out.String()
}

// Logger returns a debug logger for a driver. The test prints the log when
// it fails.
func Logger(t *testing.T) *slog.Logger {
	out := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("driver log:\n%s", out)
		}
	})
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// syncBuffer is a bytes.Buffer that goroutines can share, also after a test
// ends.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// CheckLines reports each line of want that text does not have.
func CheckLines(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("the log has no line %q", w)
		}
	}
}
