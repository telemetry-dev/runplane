package fleet

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/runplane/runplane"
	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/sshinstall"
	"github.com/runplane/runplane/internal/sshkey"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

// connectWait is how long an install over SSH waits for the new agent to
// connect after the install script ends.
const connectWait = time.Minute

// StepPrefix starts the line of each step in the output of install.sh.
const StepPrefix = "==> "

// Script returns install.sh with the values of this primary: the release
// repository, the agent address for the agents that reach the primary at
// host (host[:port] of the request), the certificate fingerprint, the
// version of this binary if it is a release, and the release signing key if
// the binary has one.
func (f *Fleet) Script(host string) []byte {
	return f.script(f.agentAddr(hostname(host)))
}

func (f *Fleet) script(primary string) []byte {
	vars := [][2]string{
		{"RUNPLANE_REPO", f.repo},
		{"RUNPLANE_PRIMARY", primary},
		{"RUNPLANE_FINGERPRINT", f.fp},
	}
	// An agent gets the release of its primary, so that the two speak the
	// same protocol.
	if version.IsRelease(f.version) {
		vars = append(vars, [2]string{"RUNPLANE_VERSION", f.version})
	}
	// The key comes from this binary, not from the release: someone who can
	// change the release files cannot change it.
	if f.publicKey != "" {
		vars = append(vars, [2]string{"RUNPLANE_PUBLIC_KEY", f.publicKey})
	}
	first, rest, _ := bytes.Cut(runplane.InstallScript, []byte("\n"))
	var b bytes.Buffer
	b.Write(first)
	b.WriteString("\n# Set by the primary that served this script.\n")
	for _, v := range vars {
		fmt.Fprintf(&b, "%s=%s\n", v[0], shellQuote(v[1]))
	}
	b.Write(rest)
	return b.Bytes()
}

// installCommand is the shell command that installs an agent with token.
// The script runs as root and carries the fingerprint that the agent trusts,
// so it comes from the primary only over HTTPS. Else the command takes the
// release script from GitHub and gives the primary's values as flags.
func (f *Fleet) installCommand(base, token string) string {
	u, err := url.Parse(base)
	if err != nil {
		u = &url.URL{Scheme: "http", Host: base}
	}
	host := u.Hostname()
	flags := agentFlags(token, f.agentAddr(host), f.fp)
	if u.Scheme == "https" {
		script := strings.TrimSuffix(u.String(), "/") + "/install.sh"
		return fmt.Sprintf("curl -fsSL %s | sudo sh -s -- %s", shellQuote(script), flags)
	}
	script := "https://github.com/" + f.repo + "/releases/latest/download/install.sh"
	release := "--repo " + shellQuote(f.repo)
	if version.IsRelease(f.version) {
		script = "https://github.com/" + f.repo + "/releases/download/" + f.version + "/install.sh"
		release += " --version " + shellQuote(f.version)
	}
	return fmt.Sprintf("curl -fsSL %s | sudo sh -s -- %s %s", shellQuote(script), release, flags)
}

func agentFlags(token, primary, fingerprint string) string {
	return fmt.Sprintf("--token %s --primary %s --fingerprint %s", shellQuote(token), shellQuote(primary), shellQuote(fingerprint))
}

// agentAddr is the address of the agent listener for agents that reach the
// primary at host.
func (f *Fleet) agentAddr(host string) string {
	_, port, _ := net.SplitHostPort(f.cfg.AgentListen)
	return net.JoinHostPort(f.publicAddr(host), port)
}

// publicAddr returns host, or the IPv4 address of the primary if host is a
// loopback name, which other servers cannot use.
func (f *Fleet) publicAddr(host string) string {
	ip := net.ParseIP(host)
	loopback := host == "" || host == "localhost" || ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
	if !loopback {
		return host
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return cmp.Or(f.localAddr, host)
}

// hostname returns the host of a host[:port] value.
func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

// shellQuote returns s as one shell word.
func shellQuote(s string) string {
	safe := s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("_-.,:/@%+=", r))
	})
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SSHInstall is a new server that InstallSSH installs over SSH.
type SSHInstall struct {
	Name string
	Host string // host name or IP address
	Port int    // 0 means 22
	User string // "" means root; another user needs sudo without a password
}

// InstallSSH makes a server row in the status installing and installs the
// agent over SSH in the background. It returns the ID of the new server.
// The status detail shows the output of the install script. A name or a
// host that is not valid gives field.Problems.
func (f *Fleet) InstallSSH(ctx context.Context, by audit.Actor, base string, req SSHInstall) (string, error) {
	name, host := strings.TrimSpace(req.Name), strings.TrimSpace(req.Host)
	var p field.Problems
	if name == "" {
		p.Add("name", "Enter a name.", req.Name)
	}
	if host == "" || strings.ContainsAny(host, " /@") {
		p.Add("host", "Enter a host name or an IP address.", req.Host)
	}
	if err := p.Err(); err != nil {
		return "", err
	}
	req.Name, req.Host, req.Port, req.User = name, host, cmp.Or(req.Port, 22), cmp.Or(req.User, "root")
	signer, _, err := f.sshKey()
	if err != nil {
		return "", err
	}
	t := sshinstall.Target{Addr: net.JoinHostPort(req.Host, strconv.Itoa(req.Port)), User: req.User}
	id := ids.New()
	// Register the install before its row exists: Remove cancels it, and
	// the start of Run does not mark it as stopped.
	ictx, cancel := context.WithCancel(f.bg)
	f.mu.Lock()
	f.installs[id] = cancel
	f.mu.Unlock()
	done := func() {
		cancel()
		f.mu.Lock()
		delete(f.installs, id)
		f.mu.Unlock()
	}
	now := f.now()
	v := store.Server{
		ID: id, Name: req.Name, Kind: store.ServerAgent, Status: store.ServerInstalling,
		StatusDetail: "Connecting to " + t.User + "@" + t.Addr, ServerFacts: store.ServerFacts{Address: req.Host},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := f.st.CreateServer(ctx, v); err != nil {
		done()
		return "", err
	}
	token, _, err := f.newToken(ctx, req.Name, "", id)
	if err != nil {
		done()
		if derr := f.st.DeleteServer(ctx, id, f.now()); derr != nil {
			f.log.Warn("delete the server of a failed install", "server_id", id, "err", derr)
		}
		return "", err
	}
	f.publishServer(ctx, id)
	f.audit.Record(ctx, by, "server.install", id, map[string]any{"name": req.Name, "host": req.Host, "port": req.Port, "user": req.User})

	job := installJob{id: id, target: t, signer: signer, primary: f.agentAddr(hostOf(base))}
	job.command = "sh -s -- " + agentFlags(token, job.primary, f.fp)
	if t.User != "root" {
		job.command = "sudo -n " + job.command
	}
	job.script = f.script(job.primary)
	f.goBg(func(context.Context) {
		defer done()
		f.install(ictx, job)
	})
	return id, nil
}

type installJob struct {
	id      string // the server
	target  sshinstall.Target
	signer  ssh.Signer
	primary string // the agent address of the primary
	command string
	script  []byte
}

// hostOf returns the host name of a URL such as "https://ci.example.com:8443".
func hostOf(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// install runs the install script on the target and then waits for the
// agent to connect. The status detail shows the latest line of the script,
// at most once a second.
func (f *Fleet) install(ctx context.Context, job installJob) {
	var last string
	var shown time.Time
	err := sshinstall.Install(ctx, job.target, job.signer, f.knownHosts, job.command, bytes.NewReader(job.script), func(line string) {
		line = strings.TrimSpace(strings.TrimPrefix(line, StepPrefix))
		if line == "" {
			return
		}
		last = line
		if time.Since(shown) >= time.Second {
			shown = time.Now()
			f.setStatusIf(ctx, job.id, store.ServerInstalling, store.ServerInstalling, truncate(line))
		}
	})
	if err != nil {
		f.installFailed(ctx, job.id, err, last)
		return
	}
	f.log.Info("agent installed over SSH", "server_id", job.id, "target", job.target.Addr)
	deadline := time.After(connectWait)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for f.node(job.id) == nil {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			f.setStatusIf(ctx, job.id, store.ServerInstalling, store.ServerUnreachable,
				"The agent is installed but did not connect to "+job.primary+". Make sure that the server can reach this address.")
			return
		case <-tick.C:
		}
	}
}

func (f *Fleet) installFailed(ctx context.Context, id string, err error, last string) {
	if ctx.Err() != nil {
		return // Remove or the end of Run stopped the install
	}
	f.log.Warn("install over SSH", "server_id", id, "err", err, "last_output", last)
	detail := "Install failed: " + err.Error()
	if last != "" {
		detail += ". Last output: " + last
	}
	f.setStatusIf(ctx, id, store.ServerInstalling, store.ServerUnreachable, truncate(detail))
}

// truncate cuts the text of a status detail or an update error, which can
// hold the output of a server, to 500 characters.
func truncate(s string) string {
	if r := []rune(s); len(r) > 500 {
		return string(r[:499]) + "…"
	}
	return s
}

// SSHPublicKey returns the runplane SSH key in authorized_keys format.
func (f *Fleet) SSHPublicKey(context.Context) (string, error) {
	_, line, err := f.sshKey()
	return line, err
}

func (f *Fleet) sshKey() (ssh.Signer, string, error) {
	return sshkey.LoadOrCreate(f.cfg.SSHKeyPath())
}
