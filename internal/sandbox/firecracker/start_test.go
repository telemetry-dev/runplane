package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// The test binary is also the fake jailer: systemd-run (a script in the
// tests) executes it with the jailer arguments.
func TestMain(m *testing.M) {
	if os.Getenv("RUNPLANE_FAKE_JAILER") == "1" {
		fakeJailer(os.Args[1:])
		return
	}
	scopePoll = 10 * time.Millisecond
	os.Exit(m.Run())
}

// fakeJailer serves the Firecracker API in the chroot. After InstanceStart
// it acts as runplane-init: it gets the config, reports the runner, and
// powers off on a shutdown request.
func fakeJailer(args []string) {
	var id, base, execFile string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--id":
			id = args[i+1]
		case "--chroot-base-dir":
			base = args[i+1]
		case "--exec-file":
			execFile = args[i+1]
		}
	}
	fmt.Println("jailer: starting", id)
	if msg := os.Getenv("RUNPLANE_FAKE_JAILER_FAIL"); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	}
	chroot := filepath.Join(base, filepath.Base(execFile), id, "root")
	ln, err := net.Listen("unix", filepath.Join(chroot, apiSocket))
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	booted := make(chan struct{})
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/actions" && strings.Contains(string(b), "InstanceStart") {
			close(booted)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	<-booted

	conn, err := net.Dial("unix", filepath.Join(chroot, controlSocket))
	if err != nil {
		fmt.Println("dial the control socket:", err)
		os.Exit(1)
	}
	shutdown := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+guest.PathShutdown, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		close(shutdown)
	})
	host, err := guest.NewClient(conn, mux, slog.New(slog.DiscardHandler))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	check := func(what string, err error) {
		if err != nil {
			fmt.Println(what, err)
			os.Exit(1)
		}
	}
	ctx := context.Background()
	cfg, err := host.Config(ctx)
	check("get config", err)
	if cfg.JITConfig == "jit-secret" {
		fmt.Println("guest: got the JIT config")
	}
	line := guest.AppendLogLine(nil, guest.LogLine{Time: time.Now(), Source: guest.SourceRunner, Text: "Listening for Jobs"})
	check("stream logs", host.StreamLogs(ctx, bytes.NewReader(line)))
	check("post event", host.PostEvent(ctx, sandbox.RunnerEvent{Kind: sandbox.RunnerListening}))
	<-shutdown
	check("post event", host.PostEvent(ctx, sandbox.RunnerEvent{Kind: sandbox.RunnerExited}))
	host.Close()
	os.Exit(0)
}

// startDriver is a driver whose jailer is the fake jailer above.
func startDriver(t *testing.T) (*Driver, *fakeHost, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Go polls FIFOs on Linux only; scratch disks need Linux")
	}
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("no mkfs.ext4")
	}
	root := shortTempDir(t)
	bin := filepath.Join(root, "bin")
	writeFile(t, filepath.Join(bin, "systemd-run"), "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n")
	os.Chmod(filepath.Join(bin, "systemd-run"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("RUNPLANE_FAKE_JAILER", "1")

	sys := &fakeHost{}
	d := testDriver(t, root, sys)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d.jailer = self
	files := filepath.Join(root, "files")
	for _, f := range []string{"vmlinux", "root.ext4", "runner.ext4"} {
		writeFile(t, filepath.Join(files, f), f)
	}
	d.opts.Kernel = filepath.Join(files, "vmlinux")
	d.opts.RootDisk = func(string) (string, error) { return filepath.Join(files, "root.ext4"), nil }
	d.opts.RunnerDrive = func(context.Context, string, string) (string, bool, error) {
		return filepath.Join(files, "runner.ext4"), true, nil
	}
	return d, sys, filepath.Join(root, "data")
}

func testSpec() sandbox.Spec {
	return sandbox.Spec{
		ID: "vm1", PoolID: "pool1", Image: sandbox.ImageRef{Ref: "runner:1", Digest: "sha256:5f1c"}, Arch: host.Arch(),
		VCPU: 1, MemoryMB: 512, DiskGB: 1, JITConfig: "jit-secret", TimeLimit: time.Hour,
	}
}

func TestStartAndStop(t *testing.T) {
	d, sys, data := startDriver(t)
	ctx := context.Background()
	if err := d.Start(ctx, testSpec()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"starting", "runner listening", "idle"} {
		ev := nextEvent(t, d)
		got := string(ev.State)
		if ev.Type == sandbox.EventRunner {
			got = "runner " + ev.Runner.Kind
		}
		if got != want {
			t.Fatalf("got event %q, want %q", got, want)
		}
	}
	list, _ := d.List(ctx)
	if len(list) != 1 || list[0].State != sandbox.StateIdle || list[0].IP != "10.214.0.2" || list[0].Spec.JITConfig != "" {
		t.Errorf("listed %+v", list)
	}
	record, _ := os.ReadFile(filepath.Join(data, "sandboxes", "vm1", "sandbox.json"))
	if bytes.Contains(record, []byte("jit-secret")) {
		t.Error("the JIT config is on disk")
	}

	logs, err := d.Logs(ctx, "vm1", false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(logs)
	logs.Close()
	for _, want := range []string{" console jailer: starting vm1\n", " console guest: got the JIT config\n", " runner Listening for Jobs\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the log has no line %q:\n%s", want, b)
		}
	}

	if err := d.Stop(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	var got []string
	for range 3 {
		ev := nextEvent(t, d)
		got = append(got, fmt.Sprintf("%s %s %s", ev.Type, ev.State, ev.Reason))
	}
	if want := []string{"state stopping stopped by runplane", "runner  ", "state gone stopped by runplane"}; !slices.Equal(got, want) {
		t.Errorf("events after Stop: %q, want %q", got, want)
	}
	if exists(filepath.Join(data, "sandboxes", "vm1")) || exists(filepath.Join(data, "jail", "firecracker", "vm1")) {
		t.Error("the folders of the VM remain")
	}
	if !slices.Equal(sys.calls(&sys.opened), []string{"rp-0"}) || !slices.Equal(sys.calls(&sys.closed), []string{"rp-0"}) {
		t.Errorf("TAPs opened %v and closed %v, want rp-0 both", sys.calls(&sys.opened), sys.calls(&sys.closed))
	}
	if err := d.Stop(ctx, "vm1"); err != nil {
		t.Errorf("a second Stop: %v", err)
	}
}

func TestStartFailureRemovesTheVM(t *testing.T) {
	d, sys, data := startDriver(t)
	t.Setenv("RUNPLANE_FAKE_JAILER_FAIL", "Jailer error: Cgroup not found")
	err := d.Start(context.Background(), testSpec())
	if err == nil || !strings.Contains(err.Error(), "Jailer error: Cgroup not found") {
		t.Fatalf("got %v, want the jailer error from the console", err)
	}
	if list, _ := d.List(context.Background()); len(list) != 0 {
		t.Errorf("listed %+v", list)
	}
	if exists(filepath.Join(data, "sandboxes", "vm1")) || exists(filepath.Join(data, "jail", "firecracker", "vm1")) {
		t.Error("the folders of the failed VM remain")
	}
	if !slices.Equal(sys.calls(&sys.closed), []string{"rp-0"}) {
		t.Errorf("TAPs closed: %v, want rp-0", sys.calls(&sys.closed))
	}
}
