package main

import (
	"regexp"
	"strings"

	"github.com/runplane/runplane/internal/sandbox"
)

var (
	// The runner puts "2026-11-04 10:00:00Z: " before its status lines.
	stampRe     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}Z: `)
	completedRe = regexp.MustCompile(`^Job (.+) completed with result: (\S+)$`)
)

func parseRunnerLine(line string) (sandbox.RunnerEvent, bool) {
	msg := strings.TrimSpace(line)
	if loc := stampRe.FindStringIndex(msg); loc != nil {
		msg = msg[loc[1]:]
	}
	if msg == "Listening for Jobs" {
		return sandbox.RunnerEvent{Kind: sandbox.RunnerListening}, true
	}
	if name, ok := strings.CutPrefix(msg, "Running job: "); ok {
		return sandbox.RunnerEvent{Kind: sandbox.RunnerJobStarted, JobName: name}, true
	}
	if m := completedRe.FindStringSubmatch(msg); m != nil {
		return sandbox.RunnerEvent{Kind: sandbox.RunnerJobCompleted, JobName: m[1], Result: m[2]}, true
	}
	return sandbox.RunnerEvent{}, false
}
