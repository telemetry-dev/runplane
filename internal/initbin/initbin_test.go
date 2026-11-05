package initbin

import (
	"bytes"
	"compress/gzip"
	"os"
	"testing"
)

// make init embeds a gzipped binary. A sandbox must get the plain binary,
// or its first process cannot run.
func TestEmbeddedBinary(t *testing.T) {
	elf := []byte("\x7fELF\x02\x01\x01 runplane-init")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(elf)
	zw.Close()
	old := gzipped
	gzipped = gz.Bytes()
	t.Cleanup(func() { gzipped, path = old, "" })
	t.Setenv("TMPDIR", t.TempDir())

	if b, err := Bytes(); err != nil || !bytes.Equal(b, elf) {
		t.Errorf("Bytes = %q, %v; want %q", b, err, elf)
	}
	// A cleaner of the temporary folder removes the file, and Path writes it again.
	for range 2 {
		p, err := Path()
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(b, elf) {
			t.Fatalf("%s has %q, %v; want %q", p, b, err, elf)
		}
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o755 {
			t.Errorf("%s has mode %v, want 0755", p, st.Mode().Perm())
		}
		os.Remove(p)
	}
}
