// Package secret seals values that runplane keeps in the database, such as the
// GitHub App private key, with a key file that stays outside the database.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"

	"github.com/runplane/runplane/internal/atomicfile"
)

const KeySize = 32

var ErrOpen = errors.New("open sealed value: wrong key or changed data")

// LoadOrCreate reads the key file at path. If the file does not exist, it
// writes a new random key with mode 0600.
func LoadOrCreate(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return create(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read secret key: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("read secret key %s: %d bytes, want %d", path, len(key), KeySize)
	}
	return key, nil
}

// create writes a new key. If another process made the key first, it uses
// that key.
func create(path string) ([]byte, error) {
	key := make([]byte, KeySize)
	rand.Read(key)
	if err := atomicfile.CreateOnce(path, key, 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadOrCreate(path)
		}
		return nil, fmt.Errorf("create secret key: %w", err)
	}
	return key, nil
}

type Box struct {
	aead cipher.AEAD
}

func NewBox(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secret key: %d bytes, want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret key: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal returns a random nonce followed by the ciphertext and the tag.
func (b *Box) Seal(plain []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize(), b.aead.NonceSize()+len(plain)+b.aead.Overhead())
	rand.Read(nonce)
	return b.aead.Seal(nonce, nonce, plain, nil)
}

// Open returns ErrOpen if the key is wrong or the sealed value changed.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return nil, ErrOpen
	}
	plain, err := b.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}
