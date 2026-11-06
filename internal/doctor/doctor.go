// Package doctor checks if a server can run runplane. It runs the general
// checks and the checks of the sandbox driver of the mode, which the driver
// package gives as Requirements. runplane doctor, the setup page, and the
// server page show the result.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/sandbox"
)

type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

type Check struct {
	ID      string
	Name    string
	Status  Status
	Detail  string
	Fix     string // one sentence that says how to fix a check that is not ok
	Command string // one shell command that does the fix, if one can
}

func (c Check) OK(detail string) Check {
	c.Status, c.Detail = StatusOK, detail
	return c
}

func (c Check) Warn(detail, fix string) Check {
	c.Status, c.Detail, c.Fix = StatusWarn, detail, fix
	return c
}

func (c Check) Fail(detail, fix string) Check {
	c.Status, c.Detail, c.Fix = StatusFail, detail, fix
	return c
}

func (c Check) WithCommand(command string) Check {
	c.Command = command
	return c
}

// AsRoot is the warning of a check that needs root to read the server.
func (c Check) AsRoot(detail string) Check {
	return c.Warn(detail, "Run the doctor as root.").WithCommand("sudo runplane doctor")
}

// Report is the result of Run: the facts that the doctor read, and the
// checks in the order that the dashboard and the command show them.
type Report struct {
	Info   host.Info
	Checks []Check
}

func (r Report) Failed() bool {
	return slices.ContainsFunc(r.Checks, func(c Check) bool { return c.Status == StatusFail })
}

type Options struct {
	DataDir string
	EnvFile string   // the environment file, which sets the listen addresses
	Listen  []string // TCP addresses of runplane, for example ":8484"
	// Serving tells that this process listens on Listen, as the doctor of a
	// running server does. Then an address in use needs no owner lookup,
	// which works only on Linux.
	Serving            bool
	MinFreeDiskPercent float64 // the min_free_disk_percent setting
	Unit               string  // systemd unit of runplane, for example "runplane"; empty skips its state
	Requirements       Requirements
	// Docker and BaseImage are required. Docker reads the Docker Engine of
	// the server.
	Docker func(ctx context.Context) (Docker, error)
	// BaseImage tries to pull the default builtin image, and returns its
	// registry ref and the error of the pull.
	BaseImage func(ctx context.Context) (ref string, err error)
	// Internet starts a sandbox that connects to the internet. Without it,
	// only the running service can do the internet check, because the
	// check needs the firewall rules that the service owns.
	Internet func(ctx context.Context) error
}

type Docker struct {
	Version  string
	RootDir  string            // the data folder of Docker
	Firewall string            // the firewall backend, for example "iptables"
	Runtimes map[string]string // the path of each OCI runtime
}

// Host is what the doctor read of the server. Requirements.Software adds the
// versions of the sandbox software to Info, and Requirements.Checks read the
// rest.
type Host struct {
	host.Info
	host.Details
	Docker    Docker
	DockerErr error // why Docker does not answer
	Root      bool
	Systemd   bool   // the host runs systemd
	Unit      string // Options.Unit
	UnitState string // systemctl is-active of Unit

	dockerSnap bool // Docker comes from the snap package
}

// Facts reads the facts of this server that the primary keeps. It returns
// an error only when it cannot read the memory or the data disk, and then
// still sets the other facts.
func Facts(ctx context.Context, o Options) (host.Info, error) {
	h := o.read(ctx)
	return h.Info, h.ResourcesErr
}

// Run reads this server and checks it. Some checks need root; without root
// they warn. When the mode check fails, Run returns only that check.
func Run(ctx context.Context, o Options) Report {
	req := o.Requirements
	if req.Mode.Status == StatusFail {
		return Report{Checks: []Check{req.Mode}}
	}
	s := o.scan(ctx)
	checks := s.checks(ctx, o)
	ref, pullErr := o.BaseImage(ctx)
	checks = append(checks, baseImageCheck(s.Host, req.Docker, ref, pullErr), internetCheck(ctx, o.Internet))
	return Report{Info: s.Info, Checks: checks}
}

type scan struct {
	*Host
	clock clock
	ports []portScan
}

func (o Options) scan(ctx context.Context) scan {
	h := o.read(ctx)
	h.Root = os.Geteuid() == 0
	h.Systemd = exists("/run/systemd/system")
	if h.Unit = o.Unit; h.Unit != "" {
		h.UnitState = UnitState(ctx, h.Unit)
	}
	return scan{Host: h, clock: readClock(ctx), ports: scanPorts(slices.Concat(o.Listen, o.Requirements.Listen), o.Serving)}
}

// checks returns the checks before the base image check.
func (s scan) checks(ctx context.Context, o Options) []Check {
	req := o.Requirements
	cs := []Check{req.Mode, dockerCheck(s.Host, req.Docker)}
	if req.Checks != nil {
		cs = append(cs, req.Checks(ctx, s.Host)...)
	}
	cs = append(cs, diskCheck(s.Host, o.DataDir, o.MinFreeDiskPercent), s.clock.check())
	return append(cs, portChecks(s.ports, s.Root, o.EnvFile)...)
}

func (o Options) read(ctx context.Context) *Host {
	info, details := host.Read(ctx, o.DataDir)
	h := &Host{Info: info, Details: details}
	h.Docker, h.DockerErr = o.Docker(ctx)
	h.DockerVersion = h.Docker.Version
	// A snap install conflicts with Docker Engine even when it does not answer.
	h.dockerSnap = strings.HasPrefix(h.Docker.RootDir, "/var/snap/") || exists("/snap/docker")
	if o.Requirements.Software != nil {
		o.Requirements.Software(ctx, h)
	}
	return h
}

// Format writes the checks for a terminal: one line for each check with a
// status mark, the fix and its command below a check that is not ok, and a
// summary.
func Format(w io.Writer, checks []Check) error {
	marks := map[Status]string{StatusOK: "✓", StatusWarn: "!", StatusFail: "✗"}
	width := 0
	for _, c := range checks {
		width = max(width, len(c.Name))
	}
	var b strings.Builder
	count := map[Status]int{}
	for _, c := range checks {
		count[c.Status]++
		fmt.Fprintf(&b, "%s %-*s  %s\n", marks[c.Status], width, c.Name, c.Detail)
		if c.Status != StatusOK && c.Fix != "" {
			fmt.Fprintf(&b, "  %-*s  Fix: %s\n", width, "", c.Fix)
		}
		if c.Status != StatusOK && c.Command != "" {
			fmt.Fprintf(&b, "  %-*s  Run: %s\n", width, "", c.Command)
		}
	}
	if count[StatusFail]+count[StatusWarn] == 0 {
		b.WriteString("\nAll checks passed.\n")
	} else {
		fmt.Fprintf(&b, "\n%s, %s.\n", plural(count[StatusFail], "failure"), plural(count[StatusWarn], "warning"))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// minFreeDiskBytes is the free space below which the disk check warns.
const minFreeDiskBytes = 20e9

// dockerCheck fails only when the sandboxes need Docker. Else Docker only
// builds the images that runplane cannot pull.
func dockerCheck(h *Host, need bool) Check {
	c := Check{ID: "docker", Name: "Docker"}
	if !need {
		if h.Docker.Version != "" {
			return c.OK("Docker Engine " + h.Docker.Version)
		}
		return c.OK("Not running; the sandboxes of this mode do not need Docker")
	}
	switch {
	case h.dockerSnap:
		return c.Fail("Docker comes from the snap package, which runplane does not support", "Remove the Docker snap, then install Docker Engine.").
			WithCommand("sudo snap remove docker && curl -fsSL https://get.docker.com | sh")
	case h.Docker.Version == "" && !h.Root:
		return c.AsRoot(fmt.Sprintf("Docker does not answer: %v", h.DockerErr))
	case h.Docker.Version == "":
		return c.Fail(fmt.Sprintf("Docker does not answer: %v", h.DockerErr), "Install Docker Engine.").WithCommand("curl -fsSL https://get.docker.com | sh")
	}
	return c.OK("Docker Engine " + h.Docker.Version)
}

// diskCheck fails where the scheduler starts no sandbox: at or below
// minPercent free.
func diskCheck(h *Host, dataDir string, minPercent float64) Check {
	c := Check{ID: "disk", Name: "Free disk space"}
	if h.ResourcesErr != nil || h.DiskBytes <= 0 {
		return c.Fail(fmt.Sprintf("Cannot read the data disk: %v", h.ResourcesErr), "Make sure that "+dataDir+" is on a mounted disk.")
	}
	gigabytes := func(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/1e9) }
	freePercent := float64(h.FreeDiskBytes) * 100 / float64(h.DiskBytes)
	detail := fmt.Sprintf("%s free of %s (%.0f%%)", gigabytes(h.FreeDiskBytes), gigabytes(h.DiskBytes), freePercent)
	switch {
	case freePercent <= minPercent:
		return c.Fail(fmt.Sprintf("%s; runplane starts no sandbox at %g%% free or less", detail, minPercent), "Free disk space, or lower min_free_disk_percent.")
	case h.FreeDiskBytes < minFreeDiskBytes:
		return c.Warn(detail+"; images and jobs need 20 GB or more", "Free disk space, or use a larger disk.")
	}
	return c.OK(detail)
}

// baseImageCheck tells where the default image comes from: a pull of ref,
// or a build of its Dockerfile with Docker when the pull fails (pullErr).
// needDocker tells that the docker check above fails without Docker.
func baseImageCheck(h *Host, needDocker bool, ref string, pullErr error) Check {
	c := Check{ID: "base_image", Name: "Base image source"}
	if pullErr == nil {
		return c.OK("Pulls " + ref)
	}
	why := pullErr.Error()
	if len(why) > 200 {
		why = why[:200] + "..."
	}
	switch {
	case h.Docker.Version != "":
		return c.OK(fmt.Sprintf("Builds the image from its Dockerfile with Docker, because %s cannot be pulled: %s", ref, why))
	case needDocker:
		return c.Fail(fmt.Sprintf("Cannot pull %s (%s), and Docker does not answer to build it", ref, why), "Fix the Docker check above.")
	case !h.Root:
		return c.AsRoot(fmt.Sprintf("Cannot pull %s (%s), and Docker does not answer to this user", ref, why))
	}
	return c.Fail(fmt.Sprintf("Cannot pull %s (%s), and Docker is not installed to build it", ref, why),
		"Run the install script again: it installs Docker Engine for local image builds. Or make the image public on GHCR.")
}

func internetCheck(ctx context.Context, probe func(context.Context) error) Check {
	c := Check{ID: "internet", Name: "Sandbox internet access"}
	if probe == nil {
		return c.Warn("Not checked here: only the running runplane service can do this check, because it owns the firewall rules of the sandboxes.",
			"Open this server in the dashboard and select Run doctor.")
	}
	switch err := probe(ctx); {
	case errors.Is(err, sandbox.ErrNoProbe):
		return c.Warn(fmt.Sprintf("Not checked: %v.", err), "Run runplane doctor again when the default image is ready.")
	case err != nil:
		return c.Fail(fmt.Sprintf("A sandbox cannot reach the internet: %v", err), "Fix the IP forwarding and firewall checks above.")
	}
	return c.OK("A sandbox reached the internet")
}

// The doctor measures the clock against an NTP server, because GitHub
// rejects app tokens from a server with a wrong clock. The Date header of
// GitHub can come from a cache, so it is seconds old.
const ntpServer = "time.cloudflare.com:123"

type clock struct {
	synced    bool
	syncedErr error // nil when synced is known
	skew      time.Duration
	skewErr   error
}

func readClock(ctx context.Context) clock {
	var c clock
	c.synced, c.syncedErr = clockSynced()
	c.skew, c.skewErr = ntpOffset(ctx, ntpServer)
	return c
}

func (k clock) check() Check {
	c := Check{ID: "clock", Name: "Time synchronization"}
	const fix, command = "Turn on time sync.", "sudo timedatectl set-ntp true"
	off := k.skew.Abs().Round(10 * time.Millisecond)
	side := "ahead"
	if k.skew < 0 {
		side = "behind"
	}
	switch {
	case k.skewErr == nil && off >= time.Minute:
		return c.Fail(fmt.Sprintf("The clock is %s %s, so GitHub rejects the app tokens", off, side), fix).WithCommand(command)
	case k.syncedErr == nil && !k.synced:
		return c.Warn("The kernel clock is not synchronized", fix).WithCommand(command)
	case k.skewErr == nil && off >= 5*time.Second:
		return c.Warn(fmt.Sprintf("The clock is %s %s", off, side), fix).WithCommand(command)
	case k.skewErr == nil:
		return c.OK(fmt.Sprintf("Synchronized (%s %s)", off, side))
	case k.syncedErr == nil:
		return c.OK("Synchronized")
	}
	return c.Warn(fmt.Sprintf("Cannot check: %v", k.skewErr), "Allow NTP (UDP port 123) to the internet.")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
