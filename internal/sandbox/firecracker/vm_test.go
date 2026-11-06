package firecracker

import (
	"context"
	"io"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

// A VM that dies before runplane-init connects fails with the end of its
// console as the reason, and its console is in the sandbox log.
func TestConsoleOfAFailedVM(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs Linux, where Go polls FIFOs")
	}
	root := shortTempDir(t)
	writeVM(t, root, "vm1", 7, sandbox.StateStarting, time.Now().UTC())
	dir := filepath.Join(root, "data", "sandboxes", "vm1")
	// w is the end of Firecracker.
	r, w, err := makeConsole(filepath.Join(dir, consoleFIFO))
	if err != nil {
		t.Fatal(err)
	}
	sys := &fakeHost{}
	d := recovered(t, root, sys)
	r.Close()

	logs, err := d.Logs(context.Background(), "vm1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	w.WriteString("Linux version 6.1.0\r\nKernel panic - not syncing: VFS: Unable to mount root fs\r\n---[ end Kernel panic")
	w.Close() // Firecracker exits
	writeFile(t, scopeProcs(root, "vm1"), "")

	ev := nextEvent(t, d)
	wantReason := "the sandbox stopped before runplane-init connected: Linux version 6.1.0 | Kernel panic - not syncing: VFS: Unable to mount root fs | ---[ end Kernel panic"
	if ev.State != sandbox.StateFailed || ev.Reason != wantReason {
		t.Errorf("got %s %q,\nwant failed %q", ev.State, ev.Reason, wantReason)
	}
	b, _ := io.ReadAll(logs)
	var texts []string
	for line := range strings.Lines(string(b)) {
		// "<time> console <text>"
		f := strings.SplitN(strings.TrimSuffix(line, "\n"), " ", 3)
		if len(f) != 3 || f[1] != "console" {
			t.Fatalf("log line %q has no console source", line)
		}
		texts = append(texts, f[2])
	}
	if want := []string{"Linux version 6.1.0", "Kernel panic - not syncing: VFS: Unable to mount root fs", "---[ end Kernel panic"}; !slices.Equal(texts, want) {
		t.Errorf("log %q, want %q", texts, want)
	}
	if exists(dir) {
		t.Error("the sandbox folder remains")
	}
	if got := sys.calls(&sys.closed); !slices.Equal(got, []string{"rp-7"}) {
		t.Errorf("TAPs removed: %v, want rp-7", got)
	}
}
