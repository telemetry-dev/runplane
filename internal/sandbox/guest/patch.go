package guest

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// The runner patch: in bin/Runner.Worker.dll, each UTF-16LE
// "ACTIONS_RESULTS_URL" becomes "ACTIONS_RESULTS_ORL". Then the runner
// writes the GitHub value to that name, and ACTIONS_RESULTS_URL from the .env
// file stays, so that jobs use the cache server of runplane. .NET keeps
// string literals in UTF-16LE.
const (
	resultsURLVar = "ACTIONS_RESULTS_URL"
	workerDLL     = "bin/Runner.Worker.dll"
)

var (
	resultsURL = utf16le(resultsURLVar)
	resultsORL = utf16le("ACTIONS_RESULTS_ORL")
)

// Patch applies the runner patch to the runner in dir. It returns the count
// of changes, which is 0 for a runner without the file.
func Patch(dir string) (int, error) {
	return rewrite(filepath.Join(dir, workerDLL), resultsURL, resultsORL)
}

// Unpatch gives the runner in dir its original results variable name. A
// sandbox without the cache server needs it, else the job has no results
// URL, and artifacts and the GitHub cache stop working.
func Unpatch(dir string) error {
	_, err := rewrite(filepath.Join(dir, workerDLL), resultsORL, resultsURL)
	return err
}

// rewrite changes each from in file to to, which has the same length, and
// returns the count. A missing file has no match.
func rewrite(file string, from, to []byte) (int, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for i := 0; ; n++ {
		j := bytes.Index(b[i:], from)
		if j < 0 {
			break
		}
		i += j
		i += copy(b[i:], to)
	}
	if n == 0 {
		return 0, nil
	}
	return n, os.WriteFile(file, b, 0o644)
}

// utf16le encodes an ASCII string as UTF-16LE.
func utf16le(s string) []byte {
	b := make([]byte, 0, 2*len(s))
	for i := range len(s) {
		b = append(b, s[i], 0)
	}
	return b
}
