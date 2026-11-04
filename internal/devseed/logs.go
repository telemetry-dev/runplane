//go:build devseed

package devseed

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// logTime is the time format of GitHub job logs.
const logTime = "2006-01-02T15:04:05.0000000Z"

// writeLog writes the job log in the format of the GitHub job logs API: each
// line starts with its time, and each step that runs a command or an action
// starts with a "##[group]Run" line.
func (s *seeder) writeLog(j *job) error {
	r := j.run
	repl := strings.NewReplacer(
		"{repo}", r.repo.name, "{name}", path.Base(r.repo.name), "{short}", r.sha[:7],
		"{message}", r.message, "{arch}", s.pools[j.kind.pool].Arch, "{lock}", lockHash(r.repo.name),
	)
	var b strings.Builder
	write := func(t time.Time, line string) {
		b.WriteString(t.Format(logTime))
		b.WriteByte(' ')
		b.WriteString(repl.Replace(line))
		b.WriteByte('\n')
	}
	for i, st := range j.steps {
		if st.Conclusion == "skipped" {
			continue
		}
		head, out := stepLog(j, i)
		from, length := *st.StartedAt, st.CompletedAt.Sub(*st.StartedAt)
		for n, line := range head {
			write(from.Add(time.Duration(n)*time.Millisecond), line)
		}
		// The output comes during the step and ends before the next step starts.
		for n, line := range out {
			write(from.Add(length*time.Duration(n+1)/time.Duration(len(out)+1)), line)
		}
	}
	return os.WriteFile(filepath.Join(s.logsDir, j.id+".log"), []byte(b.String()), 0o600)
}

// stepLog returns the lines that start step i of j and the output of the step.
func stepLog(j *job, i int) (head, out []string) {
	st := j.kind.steps[i]
	switch st.name {
	case setUpJob.name:
		return nil, setUpLog(j)
	case completeJob.name:
		return nil, []string{"Cleaning up orphan processes"}
	}
	if st.run != "" {
		head = append(head, "##[group]Run "+st.run)
		if !strings.Contains(st.run, "@") { // a shell command, not an action
			head = append(head, st.run, "shell: /usr/bin/bash -e {0}")
		}
		head = append(head, "##[endgroup]")
	}
	switch j.steps[i].Conclusion {
	case "failure":
		out = append(slices.Clone(st.fail), "##[error]Process completed with exit code 1.")
	case "cancelled":
		out = []string{"##[error]The operation was canceled."}
	default:
		out = st.out
	}
	return head, out
}

func setUpLog(j *job) []string {
	b := j.sandbox
	return []string{
		"Current runner version: '" + b.RunnerVersion + "'",
		"Runner name: '" + b.RunnerName + "'",
		"Runner group name: 'Default'",
		"Machine name: '" + b.RunnerName + "'",
		"##[group]Operating System",
		"Ubuntu",
		"24.04",
		"LTS",
		"##[endgroup]",
		"##[group]Runner Image",
		"Image: ubuntu-24.04",
		"Image source: runplane builtin",
		"##[endgroup]",
		"##[group]GITHUB_TOKEN Permissions",
		"Contents: read",
		"Metadata: read",
		"##[endgroup]",
		"Secret source: Actions",
		"Prepare workflow directory",
		"Prepare all required actions",
		"Getting action download info",
		"Complete job name: " + j.kind.name,
	}
}
