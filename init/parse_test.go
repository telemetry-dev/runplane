package main

import (
	"testing"

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
