package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

// A sandbox without runplane's cache must get GitHub's results URL back, or
// upload-artifact fails with "Unable to get the ACTIONS_RESULTS_URL env variable".
func TestUnpatchWorker(t *testing.T) {
	patched := dll("ACTIONS_RESULTS_ORL", "ACTIONS_RESULTS_ORL")
	original := dll("ACTIONS_RESULTS_URL", "ACTIONS_RESULTS_URL")

	tests := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"patched runner", patched, original},
		{"unpatched runner", original, original},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "Runner.Worker.dll")
			if err := os.WriteFile(file, tt.in, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := unpatchWorker(file); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("file is %q, want %q", got, tt.want)
			}
		})
	}

	if err := unpatchWorker(filepath.Join(t.TempDir(), "missing.dll")); err != nil {
		t.Errorf("missing file: %v", err)
	}
}

// dll is a fake Runner.Worker.dll with the two names in UTF-16LE, as .NET
// stores string literals, between other bytes.
func dll(first, second string) []byte {
	var b bytes.Buffer
	b.WriteString("MZ head")
	for _, s := range []string{first, " and ", second} {
		for _, u := range utf16.Encode([]rune(s)) {
			binary.Write(&b, binary.LittleEndian, u)
		}
	}
	b.WriteString("tail")
	return b.Bytes()
}
