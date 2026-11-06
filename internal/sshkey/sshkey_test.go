package sshkey

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The servers have the authorized_keys line of the first key, so all
// callers must get that key.
func TestLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh", "id_ed25519")
	lines := make([]string, 8)
	var wg sync.WaitGroup
	for i := range lines {
		wg.Go(func() {
			_, line, err := LoadOrCreate(path)
			if err != nil {
				t.Error(err)
			}
			lines[i] = line
		})
	}
	wg.Wait()
	signer, line, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if l != line {
			t.Fatalf("callers got different keys: %q", lines)
		}
	}
	pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || pub.Type() != ssh.KeyAlgoED25519 || comment != "runplane" || !bytes.Equal(pub.Marshal(), signer.PublicKey().Marshal()) {
		t.Errorf("authorized_keys line %q is not the ed25519 key with the comment runplane: %v", line, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v, %v; want mode 600", info, err)
	}
}
