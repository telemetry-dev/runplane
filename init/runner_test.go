package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"github.com/runplane/runplane/internal/sandbox"
)

func TestParseRunnerLine(t *testing.T) {
	tests := []struct {
		line string
		want sandbox.RunnerEvent
		ok   bool
	}{
		{line: "√ Connected to GitHub"},
		{line: ""},
		{line: "Current runner version: '2.330.0'"},
		{
			line: "2026-11-04 10:00:00Z: Listening for Jobs",
			want: sandbox.RunnerEvent{Kind: "listening"},
			ok:   true,
		},
		{
			line: "2026-11-04 10:00:05Z: Running job: test (node 22)",
			want: sandbox.RunnerEvent{Kind: "job.started", JobName: "test (node 22)"},
			ok:   true,
		},
		{
			line: "2026-11-04 10:03:12Z: Job test (node 22) completed with result: Succeeded",
			want: sandbox.RunnerEvent{Kind: "job.completed", JobName: "test (node 22)", Result: "Succeeded"},
			ok:   true,
		},
		{
			line: "2026-11-04 10:03:12Z: Job build completed with result: Failed\r",
			want: sandbox.RunnerEvent{Kind: "job.completed", JobName: "build", Result: "Failed"},
			ok:   true,
		},
		{
			// A job name can hold the words of the pattern; the result is the last word.
			line: "2026-11-04 10:03:12Z: Job Job x completed with result: y completed with result: Canceled",
			want: sandbox.RunnerEvent{Kind: "job.completed", JobName: "Job x completed with result: y", Result: "Canceled"},
			ok:   true,
		},
		{line: "2026-11-04 10:00:05Z: Running job: "},
		{line: "2026-11-04 10:00:00Z: Listening for Jobs soon"},
	}
	for _, tt := range tests {
		got, ok := parseRunnerLine(tt.line)
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseRunnerLine(%q) = %+v, %v; want %+v, %v", tt.line, got, ok, tt.want, tt.ok)
		}
	}
}

// A sandbox without runplane's cache must get GitHub's results URL back, or
// upload-artifact fails with "Unable to get the ACTIONS_RESULTS_URL env variable".
func TestUnpatchWorker(t *testing.T) {
	original := dll("ACTIONS_RESULTS_URL")
	for _, in := range [][]byte{dll("ACTIONS_RESULTS_ORL"), original} {
		file := filepath.Join(t.TempDir(), "Runner.Worker.dll")
		if err := os.WriteFile(file, in, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := unpatchWorker(file); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(file); !bytes.Equal(got, original) {
			t.Errorf("%q became %q, want %q", in, got, original)
		}
	}
	if err := unpatchWorker(filepath.Join(t.TempDir(), "missing.dll")); err != nil {
		t.Errorf("missing file: %v", err)
	}
}

// dll is a fake Runner.Worker.dll with the name two times in UTF-16LE, as
// .NET stores string literals, between other bytes.
func dll(name string) []byte {
	var b bytes.Buffer
	b.WriteString("MZ head")
	for _, s := range []string{name, " and ", name} {
		for _, u := range utf16.Encode([]rune(s)) {
			binary.Write(&b, binary.LittleEndian, u)
		}
	}
	b.WriteString("tail")
	return b.Bytes()
}
