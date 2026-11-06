// Package sshkey keeps the ed25519 SSH keys of runplane in files: the key
// that installs agents and the host key of the SSH gateway.
package sshkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/runplane/runplane/internal/atomicfile"
)

// LoadOrCreate loads the SSH key from path, or makes a new ed25519 key
// there (mode 0600). It returns the signer and the authorized_keys line of
// the key, with the comment "runplane".
func LoadOrCreate(path string) (ssh.Signer, string, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		if data, err = newKey(path); err != nil {
			return nil, "", err
		}
	default:
		return nil, "", fmt.Errorf("read ssh key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, "", fmt.Errorf("parse ssh key %s: %w", path, err)
	}
	return signer, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " runplane", nil
}

func newKey(path string) ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ssh key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "runplane")
	if err != nil {
		return nil, fmt.Errorf("encode ssh key: %w", err)
	}
	data := pem.EncodeToMemory(block)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("make ssh key dir: %w", err)
	}
	// If another caller made the key first, use that key.
	if err := atomicfile.CreateOnce(path, data, 0o600); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return os.ReadFile(path)
		}
		return nil, fmt.Errorf("write ssh key: %w", err)
	}
	return data, nil
}
