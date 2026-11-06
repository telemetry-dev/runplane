package initbin

import (
	"bytes"
	"os"
	"testing"
)

// make init embeds a gzipped binary. A sandbox must get the plain binary,
// or its first process cannot run.
func TestPath(t *testing.T) {
	if _, err := os.Stat(File); err != nil {
		t.Skip("this build has no runplane-init: run make init first")
	}
	t.Setenv("TMPDIR", t.TempDir())
	elf, err := Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(elf, []byte("\x7fELF")) {
		t.Fatalf("Bytes starts with %q, want an ELF binary", elf[:min(len(elf), 4)])
	}
	// A cleaner of the temporary folder removes the file, and Path writes it again.
	for range 2 {
		p, err := Path()
		if err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(p); err != nil || !bytes.Equal(b, elf) {
			t.Fatalf("%s has %d bytes, %v; want the %d bytes of Bytes", p, len(b), err, len(elf))
		}
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o755 {
			t.Errorf("%s has mode %v, want 0755", p, st.Mode().Perm())
		}
		os.Remove(p)
	}
}
