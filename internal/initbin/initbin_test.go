package initbin

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"
)

// make init embeds a gzipped binary and the e2e test a plain one. A sandbox
// must get the plain binary in both cases, or its first process cannot run.
func TestUnpack(t *testing.T) {
	elf := []byte("\x7fELF\x02\x01\x01 runplane-init")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(elf)
	zw.Close()

	for name, in := range map[string][]byte{"gzipped": gz.Bytes(), "plain": elf} {
		t.Run(name, func(t *testing.T) {
			r, err := unpack(in)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, elf) {
				t.Errorf("unpacked %q, %v; want %q", got, err, elf)
			}
		})
	}
}
