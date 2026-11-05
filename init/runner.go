package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/runplane/runplane/internal/sandbox"
)

var (
	// The runner puts "2026-11-04 10:00:00Z: " before its status lines.
	stampRe     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}Z: `)
	completedRe = regexp.MustCompile(`^Job (.+) completed with result: (\S+)$`)
)

// parseRunnerLine finds the runner lifecycle events in a line of the runner stdout.
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

// writeEnvFile writes the runner .env file. The runner sets each
// "key=value" line as an environment variable, so it skips an entry that
// cannot fit on one line.
func writeEnvFile(path string, env map[string]string, log *slog.Logger) error {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(env)) {
		v := env[k]
		if k == "" || strings.ContainsAny(k, "=\r\n") || strings.ContainsAny(v, "\r\n") {
			log.Warn("skipping a runner environment variable that does not fit in .env", "key", k)
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// The runner bundle on the host is patched: in bin/Runner.Worker.dll each
// UTF-16LE "ACTIONS_RESULTS_URL" is "ACTIONS_RESULTS_ORL", so the .env value
// of runplane's cache server reaches the job. A sandbox without that .env
// value needs the original name back, else the job has no results URL and
// artifacts and the GitHub cache stop working.
var (
	resultsURL = utf16le("ACTIONS_RESULTS_URL")
	resultsORL = utf16le("ACTIONS_RESULTS_ORL")
)

// unpatchWorker gives the runner in file its original results variable name.
// An unpatched or missing file stays as it is.
func unpatchWorker(file string) error {
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Contains(b, resultsORL) {
		return nil
	}
	return os.WriteFile(file, bytes.ReplaceAll(b, resultsORL, resultsURL), 0o644)
}

func utf16le(s string) []byte {
	b := make([]byte, 0, 2*len(s))
	for _, r := range s {
		b = append(b, byte(r), 0)
	}
	return b
}
