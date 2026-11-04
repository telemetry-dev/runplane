package main

import (
	"bytes"
	"errors"
	"os"
)

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
