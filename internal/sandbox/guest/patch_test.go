package guest

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

func TestPatch(t *testing.T) {
	url := utf16Bytes("ACTIONS_RESULTS_URL")
	orl := utf16Bytes("ACTIONS_RESULTS_ORL")
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	tests := []struct {
		name string
		in   []byte
		want []byte
		n    int
	}{
		{"each UTF-16 match", join([]byte{0x4d, 0x5a}, url, []byte{0, 0, 7}, url), join([]byte{0x4d, 0x5a}, orl, []byte{0, 0, 7}, orl), 2},
		{"adjacent matches", join(url, url), join(orl, orl), 2},
		{"ASCII is not a match", []byte("xxACTIONS_RESULTS_URLxx"), []byte("xxACTIONS_RESULTS_URLxx"), 0},
		{"cut match at the end", join([]byte{1}, url[:len(url)-2]), join([]byte{1}, url[:len(url)-2]), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeWorker(t, dir, tt.in)
			n, err := Patch(dir)
			if err != nil {
				t.Fatal(err)
			}
			if n != tt.n {
				t.Errorf("count = %d, want %d", n, tt.n)
			}
			if b := readWorker(t, dir); !bytes.Equal(b, tt.want) {
				t.Errorf("bytes = %q, want %q", b, tt.want)
			}
		})
	}
	if n, err := Patch(t.TempDir()); n != 0 || err != nil {
		t.Errorf("runner without the file: %d, %v", n, err)
	}
}

// A sandbox without runplane's cache must get GitHub's results URL back, or
// upload-artifact fails with "Unable to get the ACTIONS_RESULTS_URL env variable".
func TestUnpatch(t *testing.T) {
	original := dll("ACTIONS_RESULTS_URL")
	for _, in := range [][]byte{dll("ACTIONS_RESULTS_ORL"), original} {
		dir := t.TempDir()
		writeWorker(t, dir, in)
		if err := Unpatch(dir); err != nil {
			t.Fatal(err)
		}
		if got := readWorker(t, dir); !bytes.Equal(got, original) {
			t.Errorf("%q became %q, want %q", in, got, original)
		}
	}
	if err := Unpatch(t.TempDir()); err != nil {
		t.Errorf("runner without the file: %v", err)
	}
}

func writeWorker(t *testing.T, dir string, b []byte) {
	t.Helper()
	file := filepath.Join(dir, "bin", "Runner.Worker.dll")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readWorker(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "bin", "Runner.Worker.dll"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// dll is a fake Runner.Worker.dll with the name two times in UTF-16LE, as
// .NET stores string literals, between other bytes.
func dll(name string) []byte {
	var b bytes.Buffer
	b.WriteString("MZ head")
	for _, s := range []string{name, " and ", name} {
		b.Write(utf16Bytes(s))
	}
	b.WriteString("tail")
	return b.Bytes()
}

func utf16Bytes(s string) []byte {
	var b bytes.Buffer
	for _, u := range utf16.Encode([]rune(s)) {
		binary.Write(&b, binary.LittleEndian, u)
	}
	return b.Bytes()
}
