package fleet

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/runplane/runplane"
	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/sshinstall/sshtest"
	"github.com/runplane/runplane/internal/store"
)

// printValues parses the flags of install.sh and prints the values that an
// agent install uses.
const printValues = `parse_flags "$@"
printf '%s\n' "REPO=$REPO" "VERSION=$VERSION" "PRIMARY=$PRIMARY" "FINGERPRINT=$FINGERPRINT" "TOKEN=$TOKEN" "ROLE=$ROLE"
`

// replaceMain returns install.sh with body in the place of the call of main.
func replaceMain(script []byte, body string) ([]byte, error) {
	src, ok := strings.CutSuffix(strings.TrimRight(string(script), "\n"), `main "$@"`)
	if !ok {
		return nil, errors.New(`install.sh does not end with main "$@"`)
	}
	return []byte(src + body), nil
}

func sh(stdin []byte, env []string, args ...string) (string, error) {
	cmd := exec.Command("sh", args...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func values(out string) (map[string]string, error) {
	m := map[string]string{}
	for line := range strings.Lines(out) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			return nil, fmt.Errorf("unexpected output line %q in:\n%s", line, out)
		}
		m[k] = v
	}
	return m, nil
}

// TestInstallValues runs install commands with a curl that serves
// install.sh and a sudo that only runs its command: the commands that
// NewToken makes, and the command that runs the install.sh of the primary
// with only a token.
func TestInstallValues(t *testing.T) {
	tests := []struct {
		name, version string
		base          string // of NewToken; "": install.sh of the primary with a token
		url           string // that curl gets
		wantVersion   string
		wantHost      string // of the agent address of the primary
	}{
		{"script of a release", "v1.4.2", "", "http://ci.example.com:8484/install.sh", "v1.4.2", "ci.example.com"},
		{"script of a development build", "dev", "", "http://ci.example.com/install.sh", "latest", "ci.example.com"},
		{"script over IPv6", "v1.4.2", "", "http://[2001:db8::5]:8484/install.sh", "v1.4.2", "2001:db8::5"},
		{"script over an SSH tunnel", "v1.4.2", "", "http://localhost:9000/install.sh", "v1.4.2", "203.0.113.7"},
		{"command with a public URL", "v1.4.2", "https://ci.example.com", "https://ci.example.com/install.sh", "v1.4.2", "ci.example.com"},
		// Plain HTTP carries no proof of the primary, so the script comes from GitHub.
		{"command over an SSH tunnel", "v1.4.2", "http://localhost:9000",
			"https://github.com/acme/runplane-fork/releases/download/v1.4.2/install.sh", "v1.4.2", "203.0.113.7"},
		{"command with no domain", "v1.4.2", "http://203.0.113.7:8484",
			"https://github.com/acme/runplane-fork/releases/download/v1.4.2/install.sh", "v1.4.2", "203.0.113.7"},
		{"command of a development build", "dev", "http://203.0.113.7:8484",
			"https://github.com/acme/runplane-fork/releases/latest/download/install.sh", "latest", "203.0.113.7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, newFakeNode("arm64"), func(c *Config) { c.Repo, c.Version = "acme/runplane-fork", tt.version })
			h.run()
			_, port, _ := net.SplitHostPort(h.cfg.AgentListen)
			command, token := "curl -fsSL '"+tt.url+"' | sudo sh -s -- --token rp_srv_TEST", "rp_srv_TEST"
			if tt.base != "" {
				tok, err := h.f.NewToken(t.Context(), audit.Actor{}, tt.base, "build-1")
				if err != nil {
					t.Fatal(err)
				}
				command, token = tok.Command, tok.Token
			}
			// The release script on GitHub has no values of this primary.
			served := runplane.InstallScript
			if !strings.HasPrefix(tt.url, "https://github.com/") {
				served = h.f.Script(httptest.NewRequest("GET", tt.url, nil).Host)
			}
			script, err := replaceMain(served, printValues)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "install.sh"), script, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := sh(nil, []string{"DIR=" + dir}, "-c", `curl() { printf '%s\n' "$*" >"$DIR/curl-args"; cat "$DIR/install.sh"; }
sudo() { "$@"; }
`+command)
			if err != nil {
				t.Fatalf("command %q: %v\n%s", command, err, out)
			}
			if args, _ := os.ReadFile(filepath.Join(dir, "curl-args")); strings.TrimSpace(string(args)) != "-fsSL "+tt.url {
				t.Errorf("curl %s, want -fsSL %s", args, tt.url)
			}
			got, err := values(out)
			want := map[string]string{
				"REPO": "acme/runplane-fork", "VERSION": tt.wantVersion, "PRIMARY": net.JoinHostPort(tt.wantHost, port),
				"FINGERPRINT": h.fp, "TOKEN": token, "ROLE": "agent",
			}
			if err != nil || !maps.Equal(got, want) {
				t.Errorf("command %q gives\n%v, %v\nwant\n%v", command, got, err, want)
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
		dir := t.TempDir()
		path := filepath.Join(dir, "agent.json")
		registered := "{\n  \"primary\": \"" + primary + "\",\n  \"fingerprint\": \"sha256:aa\",\n" +
			"  \"server_id\": \"01jbd5x7k2m4n6p8q9r0s1t2v3\",\n  \"secret\": \"s3cret\"\n}\n"
		if err := os.WriteFile(path, []byte(registered), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := sh(script, nil, "-s", "--", dir, primary); err != nil {
			t.Fatalf("install.sh: %v\n%s", err, out)
		}
		if raw, _ := os.ReadFile(path); string(raw) != registered {
			t.Errorf("agent.json of %s after a second install with the same token:\n%s\nwant it unchanged", primary, raw)
		}
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
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, newFakeNode("arm64"), func(c *Config) {
		c.Repo, c.Version, c.PublicKey = "acme/runplane", "v1.4.2", base64.StdEncoding.EncodeToString(pub)
	})
	h.run()
	// curl takes the files from $RELEASES: install.sh calls it with
	// "-o <file> <url>" at the end.
	script, err := replaceMain(h.f.Script("ci.example.com"), `
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
			out, err := sh(script, []string{"RELEASES=" + releases, "WORK=" + t.TempDir()}, "-s")
			if tt.wantErr == "" {
				if err != nil || !strings.HasSuffix(out, "\nrelease binary") {
					t.Errorf("install.sh: %v; want the release binary:\n%s", err, out)
				}
				return
			}
			if err == nil || !strings.Contains(out, tt.wantErr) || strings.Contains(out, "changed binary") {
				t.Errorf("install.sh: %v; want the error %q and no binary:\n%s", err, tt.wantErr, out)
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
	if err = errors.Join(err, tw.Close(), gz.Close()); err != nil {
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
	// flags and starts the agent.
	var sawOutput atomic.Bool
	srv := sshtest.NewServer(t, clientKey, func(command string, stdin []byte, out, _ io.Writer) uint32 {
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
		script, err := replaceMain(stdin, printValues)
		if !ok || err != nil {
			fmt.Fprintf(out, "Error: the command %q does not run install.sh with sudo: %v\n", command, err)
			return 1
		}
		res, err := sh(script, nil, append([]string{"-s", "--"}, strings.Fields(args)...)...)
		v, verr := values(res)
		if err != nil || verr != nil {
			fmt.Fprintf(out, "Error: %v %v\n%s", err, verr, res)
			return 1
		}
		h.startAgent(map[string]string{"primary": v["PRIMARY"], "fingerprint": v["FINGERPRINT"], "token": v["TOKEN"]}, newFakeNode("x64"))
		fmt.Fprintln(out, "==> Done")
		return 0
	})

	id, err := h.f.InstallSSH(ctx, audit.Actor{}, "http://localhost:8484", SSHInstall{Name: "build-2", Host: "127.0.0.1", Port: port(srv.Addr), User: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the new server online", func() bool { return h.server(id).Status == store.ServerOnline && h.f.Driver(id) != nil })
	if v := h.server(id); v.Name != "build-2" || v.Kind != store.ServerAgent || v.Arch != "x64" {
		t.Errorf("installed server %+v, want build-2, an x64 agent", v)
	}
	entries, _, err := h.st.ListAudit(ctx, "", 10)
	if err != nil || !slices.ContainsFunc(entries, func(e store.AuditEntry) bool {
		return e.Action == "server.install" && e.Target == id && string(e.Detail) == fmt.Sprintf(`{"host":"127.0.0.1","name":"build-2","port":%d,"user":"deploy"}`, port(srv.Addr))
	}) {
		t.Errorf("audit entries %+v, %v; want the server.install of build-2", entries, err)
	}
	if !sawOutput.Load() {
		t.Error("the status detail did not show the install output during the install")
	}
	trusted, err := knownhosts.New(h.cfg.KnownHostsPath())
	if err != nil {
		t.Fatal(err)
	}
	tcpAddr, _ := net.ResolveTCPAddr("tcp", srv.Addr)
	if err := trusted(srv.Addr, tcpAddr, srv.HostKey); err != nil {
		t.Errorf("known_hosts does not trust the host key of the server: %v", err)
	}

	// A server at a known address with another host key: runplane does not
	// send the script.
	srv2 := sshtest.NewServer(t, clientKey, func(string, []byte, io.Writer, io.Writer) uint32 { return 0 })
	f, err := os.OpenFile(h.cfg.KnownHostsPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, knownhosts.Line([]string{srv2.Addr}, clientKey))
	f.Close()
	id2, err := h.f.InstallSSH(ctx, audit.Actor{}, "http://localhost:8484", SSHInstall{Name: "build-3", Host: "127.0.0.1", Port: port(srv2.Addr), User: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failed install", func() bool { return h.server(id2).Status == store.ServerUnreachable })
	if d := h.server(id2).StatusDetail; !strings.Contains(d, "does not match") || !strings.Contains(d, "known_hosts") {
		t.Errorf("status detail %q, want the host key mismatch and the known_hosts file", d)
	}
	if got := srv2.Commands(); len(got) != 0 {
		t.Errorf("the server with the other host key got the commands %q", got)
	}
}

// The dashboard form sends the name and the host as the user typed them.
func TestInstallSSHRefusesABadNameOrHost(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	_, err := h.f.InstallSSH(ctx, audit.Actor{}, "http://localhost:8484", SSHInstall{Name: " ", Host: "root@203.0.113.9"})
	var p field.Problems
	if !errors.As(err, &p) || len(p) != 2 || p[0].Key != "name" || p[1].Key != "host" {
		t.Fatalf("InstallSSH: %v, want problems at name and host", err)
	}
	if list, err := h.st.ListServers(ctx); err != nil || len(list) != 0 {
		t.Errorf("servers after the refused install: %+v, %v; want none", list, err)
	}
}

func port(addr string) int {
	_, p, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(p)
	return n
}
