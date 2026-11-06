//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// prepareRunnerDir makes the writable runner folder: an overlay with the
// read-only bundle as the lower layer and the upper layer on scratch. When
// the overlay mount fails, for example because scratch is not a separate
// disk, it copies the bundle.
func (g *guestInit) prepareRunnerDir(bundle string, acct account) error {
	scratch := filepath.Join(guest.ScratchDir, guest.ScratchRunner)
	upper, work := scratch+"/upper", scratch+"/work"
	err := mkdirs(guest.RunnerHome, upper, work)
	if err == nil {
		err = mount("overlay", guest.RunnerHome, "overlay", 0, "lowerdir="+bundle+",upperdir="+upper+",workdir="+work)
	}
	if err != nil {
		g.log.Warn("cannot put an overlay on the runner bundle, copying it", "err", err)
		cmd := exec.Command("cp", "-a", bundle+"/.", guest.RunnerHome)
		cmd.Stderr = os.Stderr
		code, err := g.reaper.run(cmd)
		if err == nil && code != 0 {
			err = fmt.Errorf("cp exited with code %d", code)
		}
		if err != nil {
			return fmt.Errorf("copy runner bundle: %w", err)
		}
	}
	// The runner writes its config, _diag, and _work into its folder.
	pages := filepath.Join(guest.RunnerHome, "_diag", "pages")
	if err := mkdirs(pages); err != nil {
		return err
	}
	for _, dir := range []string{guest.RunnerHome, filepath.Dir(pages), pages} {
		if err := os.Lchown(dir, int(acct.uid), int(acct.gid)); err != nil {
			return err
		}
	}
	if !g.cfg.ActionsCache {
		if err := guest.Unpatch(guest.RunnerHome); err != nil {
			return fmt.Errorf("give the runner the GitHub results URL: %w", err)
		}
	}
	envFile := filepath.Join(guest.RunnerHome, ".env")
	if err := writeEnvFile(envFile, g.cfg.Env, g.log); err != nil {
		return fmt.Errorf("write runner .env: %w", err)
	}
	return os.Lchown(envFile, int(acct.uid), int(acct.gid))
}

// writablePaths are the image folders where jobs write the most: caches in
// the home folders, tool downloads in /opt/hostedtoolcache, and installs in
// /usr/local. /etc and /var stay as they are: Docker and Sysbox mount files
// and folders there, which an overlay would hide.
var writablePaths = []string{"/home", "/root", "/opt", "/usr/local"}

// overlayWritablePaths puts the writes to writablePaths on scratch in
// container mode. Else they go to the container root, which is on the host
// disk with no limit. Without a scratch disk it does nothing.
// The sysbox driver stops a sandbox whose writes outside scratch pass its
// disk size, so a failed overlay leaves the host disk safe.
func (g *guestInit) overlayWritablePaths() {
	if !isMountPoint(guest.ScratchDir) {
		return
	}
	for _, p := range writablePaths {
		if err := overlayOnScratch(p); err != nil {
			g.log.Warn("cannot put the writes of a folder on scratch", "path", p, "err", err)
		}
	}
}

func overlayOnScratch(path string) error {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); errors.Is(err, unix.ENOENT) {
		return nil
	} else if err != nil {
		return err
	}
	base := filepath.Join(guest.ScratchDir, guest.ScratchRoot, strings.ReplaceAll(strings.Trim(path, "/"), "/", "-"))
	upper, work := base+"/upper", base+"/work"
	if err := mkdirs(upper, work); err != nil {
		return err
	}
	// The top folder of the overlay takes its owner and mode from the upper layer.
	if err := unix.Chown(upper, int(st.Uid), int(st.Gid)); err != nil {
		return err
	}
	if err := unix.Chmod(upper, st.Mode&0o7777); err != nil {
		return err
	}
	return mount("overlay", path, "overlay", 0, "lowerdir="+path+",upperdir="+upper+",workdir="+work)
}

func (g *guestInit) startDockerd() *child {
	path, err := exec.LookPath("dockerd")
	if err != nil {
		g.log.Info("no dockerd in the image, so no Docker in this sandbox")
		return nil
	}
	if err := dockerDataRoot(); err != nil {
		g.log.Warn("put Docker data on scratch", "err", err)
	}
	args := []string{"--storage-driver", "overlay2", "--data-root", guest.DockerDir}
	if m := g.cfg.RegistryMirror; m != "" {
		args = append(args, "--registry-mirror", m)
		if u, err := url.Parse(m); err == nil && u.Scheme == "http" {
			args = append(args, "--insecure-registry", u.Host)
		}
	}
	c, err := g.startLogged(exec.Command(path, args...), guest.SourceDockerd, nil)
	if err != nil {
		g.log.Error("start dockerd", "err", err)
		return nil
	}
	return c
}

func dockerDataRoot() error {
	if isMountPoint(guest.DockerDir) || !isMountPoint(guest.ScratchDir) {
		return nil
	}
	src := filepath.Join(guest.ScratchDir, guest.ScratchDocker)
	if err := mkdirs(src, guest.DockerDir); err != nil {
		return err
	}
	return mount(src, guest.DockerDir, "", unix.MS_BIND, "")
}

// Why a runner failed, for the exited event.
const (
	errNoStart   = "the runner did not start; see the sandbox log"
	errNoConnect = "the runner did not connect to GitHub"
)

// runRunner runs ./run.sh --jitconfig as the runner user until it exits, the
// time limit passes, or the host asks for a shutdown. It returns the exit
// code, and why the runner failed or "". The time limit ends at
// sandbox.Deadline from the start of the runner.
//
// A runner that exits by itself before "Listening for Jobs" failed, also
// with exit code 0: the runner exits with 0 after a bad JIT config.
func (g *guestInit) runRunner(acct account) (int, string) {
	cmd := exec.Command("./run.sh", "--jitconfig", g.cfg.JITConfig)
	cmd.Dir = guest.RunnerHome
	// With this variable, run.sh passes SIGTERM on to the runner as SIGINT
	// and exits with the runner's exit code.
	cmd.Env = acct.env("RUNNER_MANUALLY_TRAP_SIG=1")
	// A new session lets init find and kill all runner processes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: acct.credential()}
	jobStarted := make(chan struct{}, 1)
	var listened atomic.Bool
	start := time.Now()
	r, err := g.startLogged(cmd, guest.SourceRunner, func(line string) {
		if ev, ok := parseRunnerLine(line); ok {
			g.link.event(ev)
			switch ev.Kind {
			case sandbox.RunnerListening:
				listened.Store(true)
			case sandbox.RunnerJobStarted:
				select {
				case jobStarted <- struct{}{}:
				default:
				}
			}
		}
	})
	if err != nil {
		g.log.Error("start runner", "err", err)
		return -1, errNoStart
	}
	sid := r.pid // run.sh leads the session
	g.log.Info("runner started", "pid", sid)

	pagesCtx, stopPages := context.WithCancel(context.Background())
	pagesDone := make(chan struct{})
	go func() {
		defer close(pagesDone)
		err := tailPages(pagesCtx, filepath.Join(guest.RunnerHome, "_diag", "pages"), func(line string) { g.logs.push(guest.SourceDiag, line) })
		if err != nil {
			g.log.Warn("tail runner pages", "err", err)
		}
	}()

	limit := time.Duration(g.cfg.TimeLimitSeconds) * time.Second
	var timer *time.Timer
	var timeout <-chan time.Time
	if limit > 0 {
		timer = time.NewTimer(time.Until(sandbox.Deadline(start, time.Time{}, limit)))
		defer timer.Stop()
		timeout = timer.C
	}
	var code int
	ended := false // the runner exited by itself
wait:
	for {
		select {
		case <-jobStarted:
			if timer != nil {
				timer.Reset(time.Until(sandbox.Deadline(start, time.Now(), limit)))
			}
		case code = <-r.exit:
			ended = true
			break wait
		case <-timeout:
			g.log.Warn("time limit reached, killing the runner", "seconds", g.cfg.TimeLimitSeconds)
			killSession(sid, unix.SIGKILL)
			code = <-r.exit
			break wait
		case <-g.req.stop:
			g.log.Info("stopping the runner")
			unix.Kill(sid, unix.SIGTERM)
			select {
			case code = <-r.exit:
			case <-time.After(guest.ProcessStop):
				killSession(sid, unix.SIGKILL)
				code = <-r.exit
			}
			break wait
		}
	}
	g.log.Info("runner exited", "code", code)
	// Processes that the job left behind can hold the output pipe open.
	killSession(sid, unix.SIGKILL)
	// Send the last lines before the exited event.
	select {
	case <-r.output:
	case <-time.After(guest.OutputWait):
	}
	stopPages()
	<-pagesDone
	if ended && !listened.Load() {
		g.log.Warn("the runner exited before it connected to GitHub", "code", code)
		return code, errNoConnect
	}
	return code, ""
}

type child struct {
	pid    int
	exit   <-chan int
	output chan struct{} // closed when its output ends
}

// startLogged starts cmd with its output in the log stream under source.
// onLine also gets each output line.
func (g *guestInit) startLogged(cmd *exec.Cmd, source string, onLine func(string)) (*child, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	pid, exit, err := g.reaper.start(cmd)
	pw.Close()
	if err != nil {
		pr.Close()
		return nil, err
	}
	c := &child{pid: pid, exit: exit, output: make(chan struct{})}
	go func() {
		defer close(c.output)
		defer pr.Close()
		guest.ReadLines(pr, func(line string) {
			g.logs.push(source, line)
			if onLine != nil {
				onLine(line)
			}
		})
	}()
	return c, nil
}

func (c *child) stop() {
	unix.Kill(c.pid, unix.SIGTERM)
	select {
	case <-c.exit:
	case <-time.After(guest.ProcessStop):
		unix.Kill(c.pid, unix.SIGKILL)
		<-c.exit
	}
}

// killSession signals each process of a session. A job can start processes
// in new process groups, but rarely in a new session.
func killSession(sid int, sig unix.Signal) {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if s, err := unix.Getsid(pid); err == nil && s == sid {
			unix.Kill(pid, sig)
		}
	}
}

func (a account) credential() *syscall.Credential {
	return &syscall.Credential{Uid: a.uid, Gid: a.gid, Groups: a.groups}
}
