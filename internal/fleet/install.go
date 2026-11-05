package fleet

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/runplane/runplane"
	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/sshinstall"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/update"
	"github.com/runplane/runplane/internal/version"
)

// connectWait is how long an install over SSH waits for the new agent to
// connect after the install script ends.
const connectWait = time.Minute

// releaseVersion matches the versions that have a release to install.
var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// Script returns install.sh with the values of this primary: the release
// repository, the agent address for the host of the request, the
// certificate fingerprint, the version of this binary if it is a release,
// and the release signing key if the binary has one.
func (f *Fleet) Script(r *http.Request) []byte {
	return f.script(f.agentAddr(hostname(r.Host)))
}

func (f *Fleet) script(primary string) []byte {
	vars := [][2]string{
		{"RUNPLANE_REPO", version.Repo},
		{"RUNPLANE_PRIMARY", primary},
		{"RUNPLANE_FINGERPRINT", f.fp},
	}
	// An agent gets the release of its primary, so that the two speak the
	// same protocol.
	if releaseVersion.MatchString(version.Version) {
		vars = append(vars, [2]string{"RUNPLANE_VERSION", version.Version})
	}
	// The key comes from this binary, not from the release: someone who can
	// change the release files cannot change it.
	if update.PublicKey != "" {
		vars = append(vars, [2]string{"RUNPLANE_PUBLIC_KEY", update.PublicKey})
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
	script := "https://github.com/" + version.Repo + "/releases/latest/download/install.sh"
	release := "--repo " + shellQuote(version.Repo)
	if releaseVersion.MatchString(version.Version) {
		script = "https://github.com/" + version.Repo + "/releases/download/" + version.Version + "/install.sh"
		release += " --version " + shellQuote(version.Version)
	}
	return fmt.Sprintf("curl -fsSL %s | sudo sh -s -- %s %s", shellQuote(script), release, flags)
}

func agentFlags(token, primary, fingerprint string) string {
	return fmt.Sprintf("--token %s --primary %s --fingerprint %s", shellQuote(token), shellQuote(primary), shellQuote(fingerprint))
}

// agentAddr is the address of the agent listener for agents that reach the
// primary at host.
func (f *Fleet) agentAddr(host string) string {
	_, port, err := net.SplitHostPort(f.cfg.AgentListen)
	if err != nil || port == "" {
		port = "8485"
	}
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

// InstallSSH makes a server row in the status installing and installs the
// agent over SSH in the background. The status detail shows the output of
// the install script.
func (f *Fleet) InstallSSH(ctx context.Context, base string, req api.SSHInstall) (string, error) {
	signer, _, err := f.sshKey()
	if err != nil {
		return "", err
	}
	t := sshinstall.Target{Addr: net.JoinHostPort(req.Host, strconv.Itoa(cmp.Or(req.Port, 22))), User: cmp.Or(req.User, "root")}
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
		f.st.DeleteServer(ctx, id)
		return "", err
	}
	f.publishServer(ctx, id)

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
	addr := job.target.Addr
	known, err := f.knownHostKey(addr)
	if err != nil {
		f.installFailed(ctx, job.id, err, "")
		return
	}
	var last string
	var shown time.Time
	hostKey, err := sshinstall.Install(ctx, job.target, job.signer, known, job.command, bytes.NewReader(job.script), func(line string) {
		line = strings.TrimSpace(strings.TrimPrefix(line, "==> "))
		if line == "" {
			return
		}
		last = line
		if time.Since(shown) >= time.Second {
			shown = time.Now()
			f.setStatusIf(ctx, job.id, store.ServerInstalling, store.ServerInstalling, truncate(line))
		}
	})
	if known == nil && hostKey != nil {
		if err := f.trustHostKey(addr, hostKey); err != nil {
			f.log.Warn("save SSH host key", "target", addr, "err", err)
		}
	}
	if err != nil {
		if strings.Contains(err.Error(), "does not match the known key") {
			err = fmt.Errorf("%w. If the server was reinstalled, remove its line from %s", err, f.knownHostsPath())
		}
		f.installFailed(ctx, job.id, err, last)
		return
	}
	f.log.Info("agent installed over SSH", "server_id", job.id, "target", addr)
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

// truncate cuts s to the 500 characters that a status detail keeps.
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
	f.keyMu.Lock()
	defer f.keyMu.Unlock()
	return sshinstall.LoadOrCreateKey(filepath.Join(f.cfg.ConfigDir, "ssh_key"))
}

func (f *Fleet) knownHostsPath() string { return filepath.Join(f.cfg.DataDir, "known_hosts") }

// knownHostKey returns the host key of addr, or nil if runplane has not
// connected to addr before.
func (f *Fleet) knownHostKey(addr string) (ssh.PublicKey, error) {
	f.keyMu.Lock()
	defer f.keyMu.Unlock()
	data, err := os.ReadFile(f.knownHostsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read known hosts: %w", err)
	}
	want := knownhosts.Normalize(addr)
	for {
		_, hosts, key, _, rest, err := ssh.ParseKnownHosts(data)
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read known hosts %s: %w", f.knownHostsPath(), err)
		}
		if slices.Contains(hosts, want) {
			return key, nil
		}
		data = rest
	}
}

// trustHostKey adds the host key of addr to known_hosts.
func (f *Fleet) trustHostKey(addr string, key ssh.PublicKey) error {
	f.keyMu.Lock()
	defer f.keyMu.Unlock()
	file, err := os.OpenFile(f.knownHostsPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = file.WriteString(knownhosts.Line([]string{addr}, key) + "\n")
	return errors.Join(err, file.Close())
}
