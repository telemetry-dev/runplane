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

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("make ssh key dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".ssh-key-*")
	if err != nil {
		return nil, fmt.Errorf("write ssh key: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("write ssh key: %w", err)
	}
	// A hard link puts the complete file in place and fails if a key is
	// already there. If another caller made the key first, use that key.
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return os.ReadFile(path)
		}
		return nil, fmt.Errorf("write ssh key: %w", err)
	}
	return data, nil
}
