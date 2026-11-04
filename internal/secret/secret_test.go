package secret

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	key, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != KeySize {
		t.Fatalf("key file: mode %v, size %d", info.Mode().Perm(), info.Size())
	}
	again, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, again) {
		t.Fatal("the second call made a new key")
	}
	if bytes.Equal(key, make([]byte, KeySize)) {
		t.Fatal("the key is all zeros")
	}
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(path); err == nil {
		t.Fatal("a 5-byte key file was accepted")
	}
}

func TestBox(t *testing.T) {
	key := bytes.Repeat([]byte{7}, KeySize)
	box, err := NewBox(key)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("-----BEGIN RSA PRIVATE KEY-----")
	sealed := box.Seal(plain)
	if bytes.Contains(sealed, plain) {
		t.Fatal("the sealed value contains the plain text")
	}
	if other := box.Seal(plain); bytes.Equal(other, sealed) {
		t.Fatal("two seals of the same value are equal: the nonce is not random")
	}
	got, err := box.Open(sealed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip: %q, %v", got, err)
	}

	for i := range sealed {
		changed := bytes.Clone(sealed)
		changed[i] ^= 1
		if _, err := box.Open(changed); !errors.Is(err, ErrOpen) {
			t.Fatalf("a change at byte %d was not found", i)
		}
	}
	if _, err := box.Open(sealed[:10]); !errors.Is(err, ErrOpen) {
		t.Fatal("a short value was accepted")
	}

	wrong, err := NewBox(bytes.Repeat([]byte{8}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Open(sealed); !errors.Is(err, ErrOpen) {
		t.Fatalf("wrong key: err = %v", err)
	}
	if _, err := NewBox(key[:16]); err == nil {
		t.Fatal("a 16-byte key was accepted")
	}
}
