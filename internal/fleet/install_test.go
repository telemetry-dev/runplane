package fleet

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/runplane/runplane"
	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/update"
	"github.com/runplane/runplane/internal/version"
)

// flagScript is install.sh that parses its flags and prints the values
// that an agent install uses, in the place of the install.
func flagScript(script []byte) ([]byte, error) {
	return replaceMain(script, `parse_flags "$@"
printf '%s\n' "REPO=$REPO" "VERSION=$VERSION" "PRIMARY=$PRIMARY" "FINGERPRINT=$FINGERPRINT" "TOKEN=$TOKEN" "ROLE=$ROLE"
`)
}

// replaceMain returns install.sh with body in the place of the call of main.
func replaceMain(script []byte, body string) ([]byte, error) {
	src, ok := strings.CutSuffix(strings.TrimRight(string(script), "\n"), `main "$@"`)
	if !ok {
		return nil, errors.New(`install.sh does not end with main "$@"`)
	}
	return []byte(src + body), nil
}

func values(out []byte) (map[string]string, error) {
	m := map[string]string{}
	for line := range strings.Lines(string(out)) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			return nil, fmt.Errorf("unexpected output line %q in:\n%s", line, out)
		}
		m[k] = v
	}
	return m, nil
}

func TestInstallScriptValues(t *testing.T) {
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	_, port, _ := net.SplitHostPort(h.cfg.AgentListen)
	repo, ver := version.Repo, version.Version
	t.Cleanup(func() { version.Repo, version.Version = repo, ver })
	version.Repo = "acme/runplane-fork"

	tests := []struct {
		name        string
		version     string
		host        string // of the request
		wantVersion string
		wantPrimary string
	}{
		{"release", "v1.4.2", "ci.example.com:8484", "v1.4.2", "ci.example.com:" + port},
		{"development build", "dev", "ci.example.com", "latest", "ci.example.com:" + port},
		{"IPv6 address", "v1.4.2", "[2001:db8::5]:8484", "v1.4.2", "[2001:db8::5]:" + port},
		{"SSH tunnel", "v1.4.2", "localhost:9000", "v1.4.2", "203.0.113.7:" + port},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version.Version = tt.version
			r := httptest.NewRequest("GET", "http://"+tt.host+"/install.sh", nil)
			script, err := flagScript(h.f.Script(r))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-s", "--", "--token", "rp_srv_TEST")
			cmd.Stdin = bytes.NewReader(script)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("install.sh: %v\n%s", err, out)
			}
			want := map[string]string{
				"REPO": "acme/runplane-fork", "VERSION": tt.wantVersion, "PRIMARY": tt.wantPrimary,
				"FINGERPRINT": h.fp, "TOKEN": "rp_srv_TEST", "ROLE": "agent",
			}
			got, err := values(out)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(got, want) {
				t.Errorf("install.sh values:\n%v\nwant\n%v", got, want)
			}
		})
	}
}

// TestInstallKeepsRegisteredAgent runs write_config of install.sh again with
// the token that the agent used. The agent.json of the registered agent must
// stay, also when the primary has an IPv6 address.
func TestInstallKeepsRegisteredAgent(t *testing.T) {
	script, err := replaceMain(runplane.InstallScript, `CONFIG_DIR=$1 DATA_DIR=$1/data ENV_FILE=$1/runplane.env AGENT_FILE=$1/agent.json
DRY_RUN=0 ROLE=agent MODE=container TOKEN=rp_srv_TEST PRIMARY=$2 FINGERPRINT=sha256:aa
write_config
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, primary := range []string{"203.0.113.7:8485", "[2001:db8::5]:8485"} {
		t.Run(primary, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent.json")
			registered := "{\n  \"primary\": \"" + primary + "\",\n  \"fingerprint\": \"sha256:aa\",\n" +
				"  \"server_id\": \"01jbd5x7k2m4n6p8q9r0s1t2v3\",\n  \"secret\": \"s3cret\"\n}\n"
			if err := os.WriteFile(path, []byte(registered), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-s", "--", dir, primary)
			cmd.Stdin = bytes.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("install.sh: %v\n%s", err, out)
			}
			if raw, _ := os.ReadFile(path); string(raw) != registered {
				t.Errorf("agent.json after a second install with the same token:\n%s\nwant it unchanged", raw)
			}
		})
	}
}

func TestInstallCommand(t *testing.T) {
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	_, port, _ := net.SplitHostPort(h.cfg.AgentListen)
	repo, ver := version.Repo, version.Version
	t.Cleanup(func() { version.Repo, version.Version = repo, ver })
	version.Repo = "acme/runplane-fork"
	tests := []struct {
		name, version, base  string
		wantURL, wantPrimary string
		wantVersion          string
		fromPrimary          bool // the primary serves install.sh, with its values in it
	}{
		{"public URL", "v1.4.2", "https://ci.example.com", "https://ci.example.com/install.sh", "ci.example.com:" + port, "v1.4.2", true},
		// Plain HTTP carries no proof of the primary, so the script comes from GitHub.
		{"SSH tunnel", "v1.4.2", "http://localhost:9000",
			"https://github.com/acme/runplane-fork/releases/download/v1.4.2/install.sh", "203.0.113.7:" + port, "v1.4.2", false},
		{"no domain", "v1.4.2", "http://203.0.113.7:8484",
			"https://github.com/acme/runplane-fork/releases/download/v1.4.2/install.sh", "203.0.113.7:" + port, "v1.4.2", false},
		{"development build", "dev", "http://203.0.113.7:8484",
			"https://github.com/acme/runplane-fork/releases/latest/download/install.sh", "203.0.113.7:" + port, "latest", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version.Version = tt.version
			tok, err := h.f.NewToken(t.Context(), tt.base, "build-1", "")
			if err != nil {
				t.Fatal(err)
			}
			// Run the command with a curl that serves install.sh and a sudo
			// that only runs its command. GitHub serves the release script,
			// which has no values of this primary.
			served := runplane.InstallScript
			if tt.fromPrimary {
				served = h.f.Script(httptest.NewRequest("GET", tt.wantURL, nil))
			}
			script, err := flagScript(served)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "install.sh"), script, 0o600); err != nil {
				t.Fatal(err)
			}
			shell := `curl() { printf '%s\n' "$*" >"$DIR/curl-args"; cat "$DIR/install.sh"; }
sudo() { "$@"; }
` + tok.Command
			cmd := exec.Command("sh", "-c", shell)
			cmd.Env = append(os.Environ(), "DIR="+dir)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("command %q: %v\n%s", tok.Command, err, out)
			}
			args, _ := os.ReadFile(filepath.Join(dir, "curl-args"))
			if got := strings.TrimSpace(string(args)); got != "-fsSL "+tt.wantURL {
				t.Errorf("curl %s, want -fsSL %s", got, tt.wantURL)
			}
			got, err := values(out)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				"REPO": "acme/runplane-fork", "VERSION": tt.wantVersion, "PRIMARY": tt.wantPrimary,
				"FINGERPRINT": h.fp, "TOKEN": tok.Token, "ROLE": "agent",
			}
			if !maps.Equal(got, want) {
				t.Errorf("command %q gives\n%v\nwant\n%v", tok.Command, got, want)
			}
		})
	}
}

// TestInstallScriptSignature runs the release download of the served
// install.sh against release files on disk. The script has the release key
// of the primary binary, so it installs only a release whose SHA256SUMS has
// a signature of that key.
func TestInstallScriptSignature(t *testing.T) {
	if out, err := exec.Command("openssl", "version").Output(); err != nil || !bytes.HasPrefix(out, []byte("OpenSSL ")) {
		t.Skip("install.sh checks signatures with OpenSSL 3 or newer")
	}
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, repo, ver := update.PublicKey, version.Repo, version.Version
	t.Cleanup(func() { update.PublicKey, version.Repo, version.Version = publicKey, repo, ver })
	update.PublicKey = base64.StdEncoding.EncodeToString(pub)
	version.Repo, version.Version = "acme/runplane", "v1.4.2"
	// curl takes the files from $RELEASES: install.sh calls it with
	// "-o <file> <url>" at the end.
	script, err := replaceMain(h.f.Script(httptest.NewRequest("GET", "http://ci.example.com/install.sh", nil)), `
curl() {
	while [ $# -gt 2 ]; do shift; done
	cp "$RELEASES/${2#https://github.com/}" "$1" 2>/dev/null || return 22
}
parse_flags "$@"
TMP=$WORK
ARCH=amd64
fetch_runplane
cat "$TMP/runplane"
`)
	if err != nil {
		t.Fatal(err)
	}

	release := releaseArchive(t, "release binary")
	changed := releaseArchive(t, "changed binary")
	sums := func(archive []byte) []byte {
		return fmt.Appendf(nil, "%x  runplane_1.4.2_linux_amd64.tar.gz\n", sha256.Sum256(archive))
	}
	tests := []struct {
		name    string
		archive []byte
		sums    []byte
		sig     []byte // nil: the release has no SHA256SUMS.sig
		wantErr string // "": the script installs the archive
	}{
		{"signed release", release, sums(release), ed25519.Sign(key, sums(release)), ""},
		{"changed archive and SHA256SUMS", changed, sums(changed), ed25519.Sign(key, sums(release)), "has no valid signature"},
		{"signature of a different key", changed, sums(changed), ed25519.Sign(otherKey, sums(changed)), "has no valid signature"},
		{"no signature", release, sums(release), nil, "download failed: https://github.com/acme/runplane/releases/download/v1.4.2/SHA256SUMS.sig"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			releases := t.TempDir()
			dir := filepath.Join(releases, "acme/runplane/releases/download/v1.4.2")
			files := map[string][]byte{"runplane_1.4.2_linux_amd64.tar.gz": tt.archive, "SHA256SUMS": tt.sums}
			if tt.sig != nil {
				files["SHA256SUMS.sig"] = tt.sig
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, b := range files {
				if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", "-s")
			cmd.Stdin = bytes.NewReader(script)
			cmd.Env = append(os.Environ(), "RELEASES="+releases, "WORK="+t.TempDir())
			out, err := cmd.CombinedOutput()
			if tt.wantErr == "" {
				if err != nil || !strings.HasSuffix(string(out), "\nrelease binary") {
					t.Fatalf("install.sh: %v; want the release binary:\n%s", err, out)
				}
				return
			}
			if err == nil || !strings.Contains(string(out), tt.wantErr) || strings.Contains(string(out), "changed binary") {
				t.Fatalf("install.sh: %v; want the error %q and no binary:\n%s", err, tt.wantErr, out)
			}
		})
	}
}

// releaseArchive returns a release archive whose runplane file holds text.
func releaseArchive(t *testing.T, text string) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	err := tw.WriteHeader(&tar.Header{Name: "runplane", Mode: 0o755, Size: int64(len(text))})
	if err == nil {
		_, err = tw.Write([]byte(text))
	}
	if err == nil {
		err = errors.Join(tw.Close(), gz.Close())
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestInstallSSH(t *testing.T) {
	ctx := t.Context()
	local := newFakeNode("arm64")
	local.info.IPv4 = "127.0.0.1" // the address that the agents dial
	h := newHarness(t, local)
	h.run()
	pub, err := h.f.SSHPublicKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil {
		t.Fatal(err)
	}

	// The SSH server does what install.sh does for an agent: it reads its
	// flags, writes agent.json, and starts the agent.
	agentCtx, stopAgent := context.WithCancel(context.Background())
	var agentDone sync.WaitGroup
	t.Cleanup(func() {
		stopAgent()
		agentDone.Wait()
	})
	agentPath := filepath.Join(t.TempDir(), "agent.json")
	var sawOutput atomic.Bool
	srv := newSSHServer(t, clientKey, func(command string, stdin []byte, out io.Writer) uint32 {
		fmt.Fprintln(out, "==> Check the host")
		// A real install takes minutes before the agent starts. Wait until
		// the dashboard shows the line.
		for range 500 {
			list, _ := h.st.ListServers(ctx)
			if slices.ContainsFunc(list, func(v store.Server) bool {
				return v.Status == store.ServerInstalling && v.StatusDetail == "Check the host"
			}) {
				sawOutput.Store(true)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		args, ok := strings.CutPrefix(command, "sudo -n sh -s -- ")
		if !ok {
			fmt.Fprintf(out, "Error: the command %q does not run the script with sudo\n", command)
			return 1
		}
		script, err := flagScript(stdin)
		if err != nil {
			fmt.Fprintf(out, "Error: %v\n", err)
			return 1
		}
		cmd := exec.Command("sh", append([]string{"-s", "--"}, strings.Fields(args)...)...)
		cmd.Stdin = bytes.NewReader(script)
		res, err := cmd.Output()
		if err != nil {
			fmt.Fprintf(out, "Error: %v\n", err)
			return 1
		}
		v, err := values(res)
		if err != nil {
			fmt.Fprintf(out, "Error: %v\n", err)
			return 1
		}
		b, _ := json.Marshal(agentFile{Primary: v["PRIMARY"], Fingerprint: v["FINGERPRINT"], Token: v["TOKEN"]})
		if err := os.WriteFile(agentPath, b, 0o600); err != nil {
			fmt.Fprintf(out, "Error: %v\n", err)
			return 1
		}
		agentDone.Go(func() {
			agent.Run(agentCtx, agent.Config{Path: agentPath, Node: newFakeNode("x64"), Logger: testLogger(t, "agent")})
		})
		fmt.Fprintln(out, "==> Done")
		return 0
	})

	id, err := h.f.InstallSSH(ctx, "http://localhost:8484", api.SSHInstall{Name: "build-2", Host: "127.0.0.1", Port: srv.port(), User: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the new server online", func() bool { return h.server(id).Status == store.ServerOnline && h.f.Driver(id) != nil })
	if v := h.server(id); v.Name != "build-2" || v.Kind != store.ServerAgent || v.Arch != "x64" || h.f.Driver(id) == nil {
		t.Errorf("installed server %+v, connected %v; want build-2, an x64 agent, connected", v, h.f.Driver(id) != nil)
	}
	if !sawOutput.Load() {
		t.Error("the status detail did not show the install output during the install")
	}
	trusted, err := knownhosts.New(filepath.Join(h.cfg.DataDir, "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if err := trusted(srv.addr, srv.tcpAddr(), srv.hostKey.PublicKey()); err != nil {
		t.Errorf("known_hosts does not trust the host key of the server: %v", err)
	}

	// A server at a known address with another host key: runplane does not
	// send the script.
	srv2 := newSSHServer(t, clientKey, func(string, []byte, io.Writer) uint32 { return 0 })
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherKey, _ := ssh.NewSignerFromKey(priv)
	f, err := os.OpenFile(filepath.Join(h.cfg.DataDir, "known_hosts"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, knownhosts.Line([]string{srv2.addr}, otherKey.PublicKey()))
	f.Close()
	id2, err := h.f.InstallSSH(ctx, "http://localhost:8484", api.SSHInstall{Name: "build-3", Host: "127.0.0.1", Port: srv2.port(), User: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failed install", func() bool { return h.server(id2).Status == store.ServerUnreachable })
	if d := h.server(id2).StatusDetail; !strings.Contains(d, "does not match") || !strings.Contains(d, "known_hosts") {
		t.Errorf("status detail %q, want the host key mismatch and the known_hosts file", d)
	}
	if got := srv2.commandCount(); got != 0 {
		t.Errorf("the server with the other host key got %d commands", got)
	}
}

// sshServer is an in-process SSH server that accepts one client key for the
// user deploy and answers each exec request with reply.
type sshServer struct {
	addr    string
	hostKey ssh.Signer
	reply   func(command string, stdin []byte, out io.Writer) uint32

	mu       sync.Mutex
	commands int
}

func newSSHServer(t *testing.T, clientKey ssh.PublicKey, reply func(string, []byte, io.Writer) uint32) *sshServer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hostKey, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	s := &sshServer{hostKey: hostKey, reply: reply}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() == "deploy" && bytes.Equal(key.Marshal(), clientKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	config.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s.addr = ln.Addr().String()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn, config)
		}
	}()
	return s
}

func (s *sshServer) handle(conn net.Conn, config *ssh.ServerConfig) {
	defer conn.Close()
	_, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, reqs, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			defer ch.Close()
			for req := range reqs {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				ssh.Unmarshal(req.Payload, &payload)
				req.Reply(true, nil)
				s.mu.Lock()
				s.commands++
				s.mu.Unlock()
				stdin, _ := io.ReadAll(ch)
				status := s.reply(payload.Command, stdin, ch)
				ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, status))
				return
			}
		}()
	}
}

func (s *sshServer) port() int {
	_, p, _ := net.SplitHostPort(s.addr)
	n, _ := strconv.Atoi(p)
	return n
}

func (s *sshServer) tcpAddr() net.Addr {
	addr, _ := net.ResolveTCPAddr("tcp", s.addr)
	return addr
}

func (s *sshServer) commandCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commands
}
