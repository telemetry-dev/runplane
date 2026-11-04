package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// agentFile is /etc/runplane/agent.json. install.sh writes the primary,
// the fingerprint, and the token. After the agent registers, the file has
// the server ID and the secret, and no token.
type agentFile struct {
	Primary     string `json:"primary"`     // host:port of the agent listener
	Fingerprint string `json:"fingerprint"` // sha256:<hex> of the primary certificate
	Token       string `json:"token,omitempty"`
	ServerID    string `json:"server_id"`
	Secret      string `json:"secret"`
}

func readAgentFile(path string) (agentFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return agentFile{}, fmt.Errorf("read agent config: %w", err)
	}
	var f agentFile
	if err := json.Unmarshal(b, &f); err != nil {
		return agentFile{}, fmt.Errorf("read agent config %s: %w", path, err)
	}
	if err := f.validate(); err != nil {
		return agentFile{}, fmt.Errorf("agent config %s: %w", path, err)
	}
	return f, nil
}

func (f agentFile) validate() error {
	if _, _, err := net.SplitHostPort(f.Primary); err != nil {
		return fmt.Errorf("primary %q is not host:port", f.Primary)
	}
	digest, ok := strings.CutPrefix(f.Fingerprint, "sha256:")
	if b, err := hex.DecodeString(digest); !ok || err != nil || len(b) != sha256.Size {
		return fmt.Errorf("fingerprint %q is not sha256:<64 hex digits>", f.Fingerprint)
	}
	if !f.registered() && f.Token == "" {
		return errors.New("no token, and no server_id and secret")
	}
	return nil
}

func (f agentFile) registered() bool { return f.ServerID != "" && f.Secret != "" }

// credentials returns the hello fields that identify the agent.
func (f agentFile) credentials() Hello {
	if f.registered() {
		return Hello{ServerID: f.ServerID, Secret: f.Secret}
	}
	return Hello{Token: f.Token}
}

// write replaces the file atomically, with mode 0600.
func (f agentFile) write(path string) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("write agent config: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(append(b, '\n'))
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("write agent config: %w", err)
	}
	// The used token cannot register again, so make the rename durable.
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
